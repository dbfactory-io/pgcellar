package app_test

import (
	"testing"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
)

// A configuration written by hand before the first start gets one baseline
// commit, and loading again adds nothing.
func TestLoadImportsExistingConfigOnce(t *testing.T) {
	e := apptest.New(t)
	log, err := config.Log(e.Dir, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("after the first load: %v, %v", log, err)
	}
	if log[0].Message != "Import the existing configuration" || log[0].Author != "pgcellar" {
		t.Fatalf("unexpected baseline commit: %+v", log[0])
	}
	a, err := app.Load(e.App.ConfigPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if log, _ := config.Log(e.Dir, 10); len(log) != 1 {
		t.Fatalf("after the second load: %d commits, want 1", len(log))
	}
}
