package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
)

func TestDeleteBackup(t *testing.T) {
	e := apptest.New(t)
	ctx := context.Background()

	if err := e.App.DeleteBackup(ctx, "gilles", "pg-main", "20261018T020001"); err != nil {
		t.Fatal(err)
	}
	calls := apptest.Calls(t, e)
	if !strings.Contains(calls, "delete pg-main 20261018T020001") {
		t.Errorf("barman delete not called:\n%s", calls)
	}
	// The cache of the server is refreshed afterwards.
	if !strings.Contains(calls[strings.Index(calls, "delete pg-main"):], "list-backups pg-main") {
		t.Errorf("the server must be refreshed after the delete:\n%s", calls)
	}
	audit, _ := e.App.Audit(50)
	found := false
	for _, a := range audit {
		if a.Action == "backup.delete" && a.Actor == "gilles" && a.Target == "pg-main/20261018T020001" {
			found = true
		}
	}
	if !found {
		t.Errorf("backup.delete missing from the audit log: %+v", audit)
	}

	// Barman's refusal is a conflict that carries Barman's message.
	err := e.App.DeleteBackup(ctx, "gilles", "pg-rsync", "20261019T230001")
	var ce *app.ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "Minimum redundancy requirements") {
		t.Errorf("minimum redundancy: %v", err)
	}

	// Bad id, unknown backup, unknown server.
	var pe *app.ProblemsError
	if err := e.App.DeleteBackup(ctx, "gilles", "pg-main", "../../etc"); !errors.As(err, &pe) {
		t.Errorf("malformed id: %v", err)
	}
	if err := e.App.DeleteBackup(ctx, "gilles", "pg-main", "19990101T000000"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown backup: %v", err)
	}
	if err := e.App.DeleteBackup(ctx, "gilles", "nope", "20261018T020001"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown server: %v", err)
	}
}
