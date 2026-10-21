package app_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
)

// barman cron starts the WAL receivers: the first status refresh must come
// after it, or every server is reported with receive-wal not running.
func TestCronRunsBeforeFirstStatus(t *testing.T) {
	e := apptest.New(t)
	if err := os.WriteFile(e.CallLog, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_BARMAN_CRON_DELAY", "1")
	e.App.CronInterval = time.Hour
	e.App.StatusInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	e.App.Start(ctx)

	var lines []string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		lines = strings.Split(strings.TrimSpace(apptest.Calls(t, e)), "\n")
		if strings.Contains(apptest.Calls(t, e), " status ") && strings.Contains(apptest.Calls(t, e), " check ") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	firstCron, firstStatus := -1, -1
	for i, l := range lines {
		f := strings.Fields(l)
		switch {
		case firstCron < 0 && len(f) == 1 && f[0] == "cron-done":
			firstCron = i
		case firstStatus < 0 && strings.Contains(" "+l+" ", " status "):
			firstStatus = i
		}
	}
	if firstCron < 0 || firstStatus < 0 || firstCron > firstStatus {
		t.Fatalf("cron must have finished before the first status (cron-done at %d, status at %d):\n%s", firstCron, firstStatus, strings.Join(lines, "\n"))
	}
}
