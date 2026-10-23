package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

func TestDeliveryThresholds(t *testing.T) {
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	recent, old := now.Add(-2*time.Hour), now.Add(-30*time.Hour)
	row := func(lastOK time.Time, fails int) store.AlertDelivery {
		r := store.AlertDelivery{Channel: alerts.Telegram, LastOK: lastOK, Failures: fails}
		if fails > 0 {
			r.FirstFailAt, r.LastFailAt, r.LastError = now.Add(-time.Hour), now.Add(-time.Minute), "the Telegram API answered 404: Not Found"
		}
		return r
	}
	for _, tc := range []struct {
		name    string
		row     *store.AlertDelivery
		state   string
		failing bool
	}{
		{"never used", nil, app.DeliveryUnknown, false},
		{"last send went through", ptr(row(recent, 0)), app.DeliveryOK, false},
		{"one failure after a recent success", ptr(row(recent, 1)), app.DeliveryShaky, false},
		{"two failures after a recent success", ptr(row(recent, 2)), app.DeliveryShaky, false},
		{"three failures in a row", ptr(row(recent, 3)), app.DeliveryFailing, true},
		{"one failure after a day of silence", ptr(row(old, 1)), app.DeliveryFailing, true},
		{"one failure and never a success", ptr(row(time.Time{}, 1)), app.DeliveryFailing, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []store.AlertDelivery
			if tc.row != nil {
				rows = append(rows, *tc.row)
			}
			d := app.DeliveryOf([]string{alerts.Telegram}, rows, now)
			c := d.Channel(alerts.Telegram)
			if c == nil || c.State != tc.state || c.Failing != tc.failing || d.Failing != tc.failing || d.AllFailing != tc.failing {
				t.Fatalf("got %+v / %+v", d, c)
			}
		})
	}

	// One channel of two fails: not "all failing", and the banner names it.
	d := app.DeliveryOf([]string{alerts.Telegram, alerts.Mail}, []store.AlertDelivery{row(old, 4), {Channel: alerts.Mail, LastOK: recent}}, now)
	if !d.Failing || d.AllFailing {
		t.Fatalf("partial: %+v", d)
	}
	if b := app.DeliveryBanner(d, func(t time.Time) string { return "then" }); b != "⚠️ Your Telegram alerts are not getting through: Telegram answered 404 Not Found since then (4 messages lost)" {
		t.Errorf("partial banner %q", b)
	}
	// A channel no longer configured is ignored; no channel at all is not "failing".
	if d := app.DeliveryOf(nil, []store.AlertDelivery{row(old, 9)}, now); d.Configured || d.Failing || len(d.Channels) != 0 {
		t.Errorf("no channel: %+v", d)
	}
}

func ptr[T any](v T) *T { return &v }

func TestDeliveryReason(t *testing.T) {
	for _, tc := range []struct{ ch, err, want string }{
		{alerts.Telegram, "the Telegram API answered 404: Not Found", "Telegram answered 404 Not Found"},
		{alerts.Telegram, "the Telegram API answered 401: Unauthorized", "Telegram answered 401 Unauthorized"},
		{alerts.Webhook, "the webhook answered 502", "the webhook answered 502"},
		{alerts.Mail, "smtp: 421 4.7.0 Try again later", "mail failed: smtp: 421 4.7.0 Try again later"},
		{alerts.Telegram, `Post "https://api.telegram.org/bot***/sendMessage": dial tcp: i/o timeout`, `Telegram failed: Post "https://api.telegram.org/bot***/sendMessage": dial tcp: i/o timeout`},
	} {
		if got := app.DeliveryReason(tc.ch, tc.err); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.ch, tc.err, got, tc.want)
		}
	}
}

// The test bed, 21-23 October: the Telegram token died, every message was
// refused with a 404 and nobody knew. The journal now says so.
func TestAlertsDeliveryFromJournal(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()
	t.Setenv("PGC_DELIVERY_TG", "123:dead")
	e := apptest.NewWith(t, func(c *config.Config) {
		c.Alerts.Telegram = &config.TelegramAlert{TokenEnv: "PGC_DELIVERY_TG", ChatID: "-1001"}
	})
	e.App.AlertOptions.TelegramBase = tg.URL
	ctx := context.Background()

	if d := e.App.AlertsDelivery(); !d.Configured || d.Failing || d.Channels[0].State != app.DeliveryUnknown {
		t.Fatalf("before any send: %+v", d)
	}
	start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.Local)
	for i := range 12 {
		at := start.Add(time.Duration(i) * time.Hour)
		e.App.Now = func() time.Time { return at }
		if _, err := e.App.SendTestAlert(ctx, "admin", ""); err != nil {
			t.Fatal(err)
		}
	}
	d := e.App.AlertsDelivery()
	c := d.Channel(alerts.Telegram)
	if !d.AllFailing || c == nil || c.ConsecutiveFailures != 12 || c.LastOK != nil || !c.FailingSince.Equal(start) {
		t.Fatalf("after 12 refusals: %+v %+v", d, c)
	}
	banner := app.DeliveryBanner(d, func(t time.Time) string { return t.Local().Format("2 Jan 15:04") })
	if want := "⚠️ Your alerts are not getting through: Telegram answered 404 Not Found since 21 Oct 01:00 (12 messages lost)"; banner != want {
		t.Errorf("banner\n got %q\nwant %q", banner, want)
	}
	if strings.Contains(c.LastError, "123:dead") {
		t.Fatal("the token must never reach the delivery state")
	}

	// One message gets through: the banner is gone at once.
	refuse.Store(false)
	if _, err := e.App.SendTestAlert(ctx, "admin", alerts.Telegram); err != nil {
		t.Fatal(err)
	}
	d = e.App.AlertsDelivery()
	if d.Failing || d.Channels[0].State != app.DeliveryOK || d.Channels[0].ConsecutiveFailures != 0 {
		t.Fatalf("after a success: %+v", d)
	}
	if b := app.DeliveryBanner(d, nil); b != "" {
		t.Errorf("banner after a success: %q", b)
	}
}
