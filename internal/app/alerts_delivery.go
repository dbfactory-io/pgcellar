package app

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// A channel is "failing" after this many failed sends in a row...
const AlertFailingAfter = 3

// ...or after a single one when it has not delivered anything for this long
// (or never did): one refused message after a day of silence is enough.
const AlertQuietFor = 24 * time.Hour

// Delivery states of a channel.
const (
	DeliveryOK      = "delivering" // its last send went through
	DeliveryFailing = "failing"    // see AlertFailingAfter and AlertQuietFor
	DeliveryShaky   = "shaky"      // one or two failures after a recent success
	DeliveryUnknown = "unknown"    // nothing sent through it yet
)

// ChannelDelivery says whether the messages of one configured channel get
// through, from the journal of sends.
type ChannelDelivery struct {
	Channel             string     `json:"channel"`
	State               string     `json:"state"`
	Failing             bool       `json:"failing"`
	LastOK              *time.Time `json:"last_ok,omitempty"`
	LastFailure         *time.Time `json:"last_failure,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	FailingSince        *time.Time `json:"failing_since,omitempty"` // first failure of the current run
	Reason              string     `json:"reason,omitempty"`        // "Telegram answered 404 Not Found"
}

// AlertsDelivery is the delivery state of every configured channel.
type AlertsDelivery struct {
	Configured bool              `json:"configured"`  // at least one channel is configured
	Failing    bool              `json:"failing"`     // at least one channel is failing
	AllFailing bool              `json:"all_failing"` // every configured channel is failing: nothing gets through
	Channels   []ChannelDelivery `json:"channels"`
}

// Channel returns the state of one channel (nil when it is not configured).
func (d AlertsDelivery) Channel(name string) *ChannelDelivery {
	for i := range d.Channels {
		if d.Channels[i].Channel == name {
			return &d.Channels[i]
		}
	}
	return nil
}

// AlertsDelivery reads the journal and says, channel by channel, whether
// the messages get through. Channels no longer configured are left out.
func (a *App) AlertsDelivery() AlertsDelivery {
	var names []string
	for _, c := range a.alertChannels() {
		names = append(names, c.Name())
	}
	var rows []store.AlertDelivery
	if a.Store != nil && len(names) > 0 {
		var err error
		if rows, err = a.Store.AlertDeliveries(); err != nil {
			a.Logf("cannot read the alert journal: %v", err)
		}
	}
	return DeliveryOf(names, rows, a.now())
}

// DeliveryOf computes the delivery states of the channels `names` from what
// the journal says about them, at `now`.
func DeliveryOf(names []string, rows []store.AlertDelivery, now time.Time) AlertsDelivery {
	by := map[string]store.AlertDelivery{}
	for _, r := range rows {
		by[r.Channel] = r
	}
	out := AlertsDelivery{Configured: len(names) > 0, Channels: []ChannelDelivery{}}
	failing := 0
	for _, n := range names {
		r, ok := by[n]
		c := ChannelDelivery{Channel: n, State: DeliveryUnknown}
		if ok {
			c = channelDelivery(r, now)
		}
		if c.Failing {
			failing++
		}
		out.Channels = append(out.Channels, c)
	}
	out.Failing = failing > 0
	out.AllFailing = failing > 0 && failing == len(names)
	return out
}

func channelDelivery(r store.AlertDelivery, now time.Time) ChannelDelivery {
	c := ChannelDelivery{Channel: r.Channel, ConsecutiveFailures: r.Failures, LastError: r.LastError}
	if !r.LastOK.IsZero() {
		t := r.LastOK
		c.LastOK = &t
	}
	if r.Failures == 0 {
		c.State = DeliveryOK
		if c.LastOK == nil {
			c.State = DeliveryUnknown
		}
		return c
	}
	last, first := r.LastFailAt, r.FirstFailAt
	c.LastFailure, c.FailingSince = &last, &first
	c.Reason = DeliveryReason(r.Channel, r.LastError)
	quiet := r.LastOK.IsZero() || now.Sub(r.LastOK) > AlertQuietFor
	if r.Failures >= AlertFailingAfter || quiet {
		c.State, c.Failing = DeliveryFailing, true
	} else {
		c.State = DeliveryShaky
	}
	return c
}

var answered = regexp.MustCompile(`^the (.+?) answered (\d{3})(?::\s*(.*))?$`)

// DeliveryReason words the last error of a channel for people:
// "the Telegram API answered 404: Not Found" -> "Telegram answered 404 Not Found".
func DeliveryReason(channel, err string) string {
	err = strings.TrimSpace(err)
	if m := answered.FindStringSubmatch(err); m != nil {
		who := m[1]
		if channel == alerts.Telegram {
			who = "Telegram"
		} else {
			who = "the " + who
		}
		return strings.TrimSpace(fmt.Sprintf("%s answered %s %s", who, m[2], m[3]))
	}
	name := ChannelLabel(channel)
	if err == "" {
		return name + " failed"
	}
	return name + " failed: " + shorten(err, 200)
}

// ChannelLabel is the name of a channel in a sentence.
func ChannelLabel(channel string) string {
	switch channel {
	case alerts.Telegram:
		return "Telegram"
	case alerts.Mail:
		return "mail"
	case alerts.Webhook:
		return "the webhook"
	}
	return channel
}

// DeliveryBanner is the sentence of the Status page when channels are
// failing, "" when everything gets through. `when` formats a date.
func DeliveryBanner(d AlertsDelivery, when func(time.Time) string) string {
	if !d.Failing {
		return ""
	}
	var parts []string
	for _, c := range d.Channels {
		if !c.Failing {
			continue
		}
		p := c.Reason
		if c.FailingSince != nil {
			p += " since " + when(*c.FailingSince)
		}
		lost := "messages"
		if c.ConsecutiveFailures == 1 {
			lost = "message"
		}
		p += fmt.Sprintf(" (%d %s lost)", c.ConsecutiveFailures, lost)
		parts = append(parts, p)
	}
	head := "⚠️ Your alerts are not getting through: "
	if !d.AllFailing {
		var names []string
		for _, c := range d.Channels {
			if c.Failing {
				names = append(names, ChannelLabel(c.Channel))
			}
		}
		head = "⚠️ Your " + strings.Join(names, " and ") + " alerts are not getting through: "
	}
	return head + strings.Join(parts, "; ")
}
