package barman

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useFakeBarman puts testdata/fake-barman first on PATH.
func useFakeBarman(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	t.Setenv("PATH", abs+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunnerAudit(t *testing.T) {
	useFakeBarman(t)
	var entries []AuditEntry
	r := CLIRunner{Binary: "fake-barman", Audit: func(e AuditEntry) { entries = append(entries, e) }}

	ctx := WithActor(context.Background(), "alice")
	out, _, err := r.Run(ctx, "--format", "json", "list-servers", "--minimal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "pg-main") {
		t.Errorf("stdout = %s", out)
	}
	_, _, err = r.Run(ctx, "--format", "json", "check", "broken")
	var re *RunError
	if !errors.As(err, &re) || re.ExitCode != 1 {
		t.Fatalf("err = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d", len(entries))
	}
	e := entries[0]
	if strings.Join(e.Args, " ") != "--format json list-servers --minimal" || e.ExitCode != 0 || e.Actor != "alice" ||
		e.Command != "fake-barman --format json list-servers --minimal" || e.When.IsZero() || e.Duration <= 0 {
		t.Errorf("entry 0 = %+v", e)
	}
	if entries[1].ExitCode != 1 || entries[1].Args[3] != "broken" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
}

func TestRunnerSudoPrefix(t *testing.T) {
	old := currentUser
	currentUser = func() string { return "pgcellar" }
	defer func() { currentUser = old }()

	got := CLIRunner{User: "barman"}.argv([]string{"check", "x"})
	if strings.Join(got, " ") != "sudo -u barman barman check x" {
		t.Errorf("argv = %v", got)
	}
	got = CLIRunner{User: "pgcellar", Binary: "/usr/bin/barman"}.argv([]string{"cron"})
	if strings.Join(got, " ") != "/usr/bin/barman cron" {
		t.Errorf("same user must not use sudo: %v", got)
	}
	got = CLIRunner{}.argv(nil)
	if strings.Join(got, " ") != "barman" {
		t.Errorf("default binary: %v", got)
	}
}

func TestRunnerContextAndMissingBinary(t *testing.T) {
	var entries []AuditEntry
	r := CLIRunner{Binary: "definitely-not-installed-barman", Audit: func(e AuditEntry) { entries = append(entries, e) }}
	_, _, err := r.Run(context.Background(), "cron")
	if err == nil || len(entries) != 1 || entries[0].ExitCode != -1 {
		t.Errorf("missing binary: err=%v entries=%+v", err, entries)
	}

	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	r = CLIRunner{Binary: "sleep"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = r.Run(ctx, "5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("context was not respected")
	}
}

func TestOnlyUnderRefusesOtherPrograms(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mine")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := CLIRunner{Binary: "mine", OnlyUnder: dir}
	if out, _, err := r.Run(context.Background()); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("own program: %q %v", out, err)
	}
	var re *RunError
	if _, _, err := r.RunBinary(context.Background(), "sh", "-c", "true"); !errors.As(err, &re) || re.ExitCode != -1 {
		t.Fatalf("a program outside the directory ran: %v", err)
	}
}

func TestRunBinaryEnv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	var entries []AuditEntry
	r := CLIRunner{Audit: func(e AuditEntry) { entries = append(entries, e) }}
	out, _, err := r.RunBinaryEnv(context.Background(), []string{"PGC_SECRET=s3cr3t"}, "sh", "-c", "echo $PGC_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "s3cr3t" {
		t.Errorf("the program did not get its variable: %q", out)
	}
	if len(entries) != 1 || strings.Contains(entries[0].Command, "s3cr3t") {
		t.Errorf("the value must never reach the audit: %+v", entries)
	}

	// Through sudo, the names (never the values) are kept with --preserve-env.
	old := currentUser
	currentUser = func() string { return "root" }
	defer func() { currentUser = old }()
	got := strings.Join(CLIRunner{User: "barman"}.argvEnv("restic", []string{"snapshots"}, []string{"A=1", "B=2"}), " ")
	if got != "sudo --preserve-env=A,B -u barman restic snapshots" {
		t.Errorf("argv = %q", got)
	}
}
