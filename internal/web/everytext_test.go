package web

import (
	"testing"
	"time"
)

func TestEveryText(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "minute", time.Minute: "minute", 5 * time.Minute: "5 minutes", 45 * time.Second: "45 seconds", 90 * time.Second: "1m30s"} {
		if got := everyText(d); got != want {
			t.Errorf("everyText(%v) = %q, want %q", d, got, want)
		}
	}
}
