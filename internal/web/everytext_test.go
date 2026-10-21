package web

import (
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

func TestEveryText(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "minute", time.Minute: "minute", 5 * time.Minute: "5 minutes", 45 * time.Second: "45 seconds", 90 * time.Second: "1m30s"} {
		if got := everyText(d); got != want {
			t.Errorf("everyText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWhenText(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	for in, want := range map[time.Time]string{
		time.Date(2026, 10, 20, 14, 58, 0, 0, time.Local): "today 14:58",
		time.Date(2026, 10, 19, 23, 5, 0, 0, time.Local):  "yesterday 23:05",
		time.Date(2026, 10, 3, 9, 7, 0, 0, time.Local):    "3 Oct, 09:07",
		time.Date(2025, 12, 31, 9, 7, 0, 0, time.Local):   "31 Dec 2025, 09:07",
		{}: "-",
	} {
		if got := whenText(in, now); got != want {
			t.Errorf("whenText(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarize(t *testing.T) {
	st := func(levels ...string) []app.ServerState {
		var out []app.ServerState
		for _, l := range levels {
			out = append(out, app.ServerState{Level: l})
		}
		return out
	}
	for _, c := range []struct {
		in         []app.ServerState
		level, txt string
	}{
		{nil, "empty", "No server to watch yet 🍷"},
		{st("ok", "ok"), "ok", "Everything is fine ✅"},
		{st("ok", "warn", "warn"), "warn", "2 servers need attention ⚠️"},
		{st("warn", "fail"), "fail", "1 server is failing ❌ — start there."},
		{st("fail", "fail", "ok"), "fail", "2 servers are failing ❌ — start there."},
	} {
		got := summarize(c.in, false)
		if got.Level != c.level || got.Text != c.txt {
			t.Errorf("summarize = %+v, want %s / %s", got, c.level, c.txt)
		}
	}
}
