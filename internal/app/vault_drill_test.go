package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/report"
)

func drillFromVault(t *testing.T, e *apptest.Env, in app.DrillInput) *app.DrillView {
	t.Helper()
	in.Source = "vault"
	id, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", in)
	if err != nil {
		t.Fatal(err)
	}
	apptest.WaitRun(t, e.App, id)
	d, ok := e.App.DrillOfRun(id)
	if !ok {
		t.Fatalf("no drill recorded for run %d", id)
	}
	return d
}

func TestDrillFromVault(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	if c := lastCopy(t, a, "pg-main"); !c.OK {
		t.Fatalf("copy: %+v", c)
	}

	d := drillFromVault(t, e, app.DrillInput{})
	if !d.OK || d.BackupID != "20261020T020001" || !d.Details.FromVault() || d.Details.VaultSnapshot == "" || d.Details.VaultSnapshotTime.IsZero() || d.Details.FetchedBytes == 0 {
		t.Fatalf("drill from the vault: %+v", d)
	}
	if stepStatus(d, "fetch") != "ok" || stepStatus(d, "recover") != "ok" || d.Details.Steps[0].Key != "fetch" {
		t.Errorf("steps: %+v", d.Details.Steps)
	}
	if !strings.Contains(d.Details.Summary, "restored from the off-site copy") {
		t.Errorf("summary: %s", d.Details.Summary)
	}
	// Barman read the fetched copy through its own configuration.
	calls := apptest.Calls(t, e)
	fetch := filepath.Join(a.Config().Recoveries.Dir, "pg-main", "vault-")
	if !strings.Contains(calls, "-c "+fetch) || !strings.Contains(calls, "rebuild-xlogdb pg-main") || !strings.Contains(calls, "barman.conf recover") {
		t.Errorf("barman calls:\n%s", calls)
	}
	restic := resticCalls(t, e)
	if !strings.Contains(restic, "--include "+filepath.Join(e.Home, "pg-main")+"/base/*/backup.info") || !strings.Contains(restic, "snapshots --json --host pgcellar --latest 1") {
		t.Errorf("restic calls:\n%s", restic)
	}
	// The fetched copy is gone, the kept data directory is the restored one.
	left, _ := filepath.Glob(filepath.Join(a.Config().Recoveries.Dir, "pg-main", "vault-*"))
	if len(left) != 0 {
		t.Errorf("fetched copy left behind: %v", left)
	}
	// The report says where the backup came from.
	path, _, err := a.DrillReport("gilles", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	pdf, _ := os.ReadFile(path)
	if _, err := report.Check(pdf); err != nil || !strings.Contains(string(pdf), "off-site copy") || !strings.Contains(string(pdf), "restic snapshot") {
		t.Errorf("report: %v", err)
	}

	// A backup that is not in the copy fails at the fetch, with words.
	d = drillFromVault(t, e, app.DrillInput{BackupID: "20250101T000000"})
	if d.OK || d.StepFailed != "fetch" || !strings.Contains(d.Details.Error, "is not in the off-site copy") {
		t.Errorf("unknown backup: %+v", d.Details)
	}
}

func TestDrillFromVaultRefusals(t *testing.T) {
	e := apptest.New(t)
	var pe *app.ProblemsError
	if _, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{Source: "vault"}); !errors.As(err, &pe) || !strings.Contains(err.Error(), "needs the vault") {
		t.Errorf("vault off: %v", err)
	}
	if _, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{Source: "tape"}); !errors.As(err, &pe) {
		t.Errorf("unknown source: %v", err)
	}

	v, _ := vaultEnv(t, func(c *config.Config) { c.Servers[0].Vault = config.Ptr(false) })
	if _, err := v.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{Source: "vault"}); !errors.As(err, &pe) || !strings.Contains(err.Error(), "not copied") {
		t.Errorf("vault: false: %v", err)
	}
}

func TestDrillFromVaultWithoutCopyAndDuringACopy(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	d := drillFromVault(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "fetch" {
		t.Fatalf("no copy yet: %+v", d.Details)
	}

	// A copy in progress keeps the drill from the vault waiting.
	t.Setenv("FAKE_RESTIC_DELAY", "1")
	copyNow(t, a, "pg-main")
	done := make(chan struct{})
	go func() { a.VaultTick(context.Background(), a.Now()); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := a.State("pg-main"); st.Vault != nil && st.Vault.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the copy never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var ce *app.ConflictError
	if _, err := a.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{Source: "vault"}); !errors.As(err, &ce) || !ce.Retry {
		t.Errorf("drill during a copy: %v", err)
	}
	<-done
}

func TestDrillSourceFollowsTheConfiguration(t *testing.T) {
	e, _ := vaultEnv(t, func(c *config.Config) { c.Drills.Source = "vault" })
	a := e.App
	copyNow(t, a, "pg-main")
	a.VaultTick(context.Background(), a.Now())
	id, err := a.StartDrill(context.Background(), app.SchedulerActor, "pg-main", app.DrillInput{}) // what the scheduler asks
	if err != nil {
		t.Fatal(err)
	}
	apptest.WaitRun(t, a, id)
	if d, _ := a.DrillOfRun(id); d == nil || !d.OK || !d.Details.FromVault() {
		t.Errorf("drills.source vault: %+v", d)
	}
	id, _ = a.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{Source: "local"})
	apptest.WaitRun(t, a, id)
	if d, _ := a.DrillOfRun(id); d == nil || !d.OK || d.Details.FromVault() || d.Details.Source != "local" {
		t.Errorf("explicit local: %+v", d)
	}
}
