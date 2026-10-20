package barman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineDeleteBackup(t *testing.T) {
	e, entries := newTestEngine(t)
	ctx := context.Background()

	out, err := e.DeleteBackup(ctx, "pg-main", "20261018T020001")
	if err != nil || !strings.Contains(out, "Deleted backup 20261018T020001") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got := strings.Join((*entries)[0].Args, " "); got != "delete pg-main 20261018T020001" {
		t.Errorf("args = %q", got)
	}

	// Barman's own refusal comes back as is.
	_, err = e.DeleteBackup(ctx, "pg-rsync", "20261019T230001")
	if err == nil || !strings.HasPrefix(err.Error(), "ERROR: Minimum redundancy requirements") {
		t.Errorf("minimum redundancy: %v", err)
	}
	if _, err := e.DeleteBackup(ctx, "pg-main", "19990101T000000"); err == nil || !strings.Contains(err.Error(), "Unknown backup") {
		t.Errorf("unknown backup: %v", err)
	}
	n := len(*entries)
	if _, err := e.DeleteBackup(ctx, "pg-main", "../etc"); err == nil || len(*entries) != n {
		t.Errorf("a malformed id must be refused before anything runs: %v", err)
	}
}

func TestEngineRecover(t *testing.T) {
	e, entries := newTestEngine(t)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "restored")

	out, err := e.Recover(ctx, "pg-main", "20261020T020001", dest, RecoverOptions{})
	if err != nil || !strings.Contains(out, "successfully prepared for recovery") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	want := "recover pg-main 20261020T020001 " + dest
	if got := strings.Join((*entries)[0].Args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	for _, f := range []string{"PG_VERSION", "postgresql.auto.conf", filepath.Join("base", "1", "1259")} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("fake restore misses %s: %v", f, err)
		}
	}

	dest2 := filepath.Join(t.TempDir(), "pitr")
	if _, err := e.Recover(ctx, "pg-main", "20261020T020001", dest2, RecoverOptions{TargetTime: "2026-10-20 12:00:00+02:00"}); err != nil {
		t.Fatal(err)
	}
	want = "recover --target-time 2026-10-20 12:00:00+02:00 pg-main 20261020T020001 " + dest2
	if got := strings.Join((*entries)[1].Args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dest2, "postgresql.auto.conf")); !strings.Contains(string(b), "recovery_target_time = '2026-10-20 12:00:00+02:00'") {
		t.Errorf("target time not written: %q", b)
	}

	dest3 := filepath.Join(t.TempDir(), "immediate")
	if out, err := e.Recover(ctx, "pg-main", "20261020T020001", dest3, RecoverOptions{TargetImmediate: true}); err != nil || !strings.Contains(out, "immediate") {
		t.Fatalf("immediate: %q %v", out, err)
	}
	if got := strings.Join((*entries)[2].Args, " "); got != "recover --target-immediate pg-main 20261020T020001 "+dest3 {
		t.Errorf("args = %q", got)
	}

	if _, err := e.Recover(ctx, "pg-main", "19990101T000000", t.TempDir(), RecoverOptions{}); err == nil || !strings.Contains(err.Error(), "Unknown backup") {
		t.Errorf("unknown backup: %v", err)
	}
	if _, err := e.Recover(ctx, "pg-main", "latest", t.TempDir(), RecoverOptions{}); err == nil {
		t.Error("a malformed id must be refused")
	}
}
