//go:build selfupdate_e2e

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// go test -tags selfupdate_e2e -run E2E -v ./internal/selfupdate
//
// Builds the stand-in program of testdata/e2e at two or three versions and
// lets it replace itself for real: download over HTTPS, checks, renames,
// syscall.Exec, confirmation, and the rollback of a version that keeps
// crashing (the restarts systemd would do are done by the test).

func build(t *testing.T, version, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-X main.Version="+version, "-o", out, "./testdata/e2e")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, b)
	}
}

func archiveOf(t *testing.T, bin string) string {
	t.Helper()
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	p := bin + ".tar.gz"
	if err := os.WriteFile(p, tarGz(t, entry{name: "LICENSE", body: "x"}, entry{name: "pgcellar", body: string(body)}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runOnce(t *testing.T, exe string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(exe, "serve")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	t.Logf("run of %s (exit %d):\n%s", exe, code, out)
	return string(out), code
}

func version(t *testing.T, exe string) string {
	t.Helper()
	out, err := exec.Command(exe, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestE2EUpdateAndConfirm(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	exe := filepath.Join(dir, "pgcellar")
	build(t, "1.0.0", exe)
	build(t, "1.1.0", filepath.Join(work, "pgcellar"))
	arch := archiveOf(t, filepath.Join(work, "pgcellar"))

	out, code := runOnce(t, exe, "E2E_ARCHIVE="+arch, "E2E_TARGET=1.1.0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"[1.0.0] running 1.0.0", "[1.1.0] running 1.1.0", "version 1.1.0 confirmed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if v := version(t, exe); v != "pgcellar 1.1.0" {
		t.Errorf("binary on disk: %q", v)
	}
	if v := version(t, exe+prevSuffix); v != "pgcellar 1.0.0" {
		t.Errorf(".prev: %q", v)
	}
	if exists(markerPath(exe)) {
		t.Error("marker left after confirmation")
	}
}

func TestE2ECrashingVersionRolledBack(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	exe := filepath.Join(dir, "pgcellar")
	build(t, "1.1.0", exe)
	build(t, "1.2.0", filepath.Join(work, "pgcellar"))
	arch := archiveOf(t, filepath.Join(work, "pgcellar"))
	env := []string{"E2E_ARCHIVE=" + arch, "E2E_TARGET=1.2.0", "E2E_CRASH_VERSION=1.2.0", "E2E_RUN_FOR=4s"}

	// First start: 1.1.0 installs 1.2.0, execs it, 1.2.0 crashes (start 1).
	if _, code := runOnce(t, exe, env...); code != 3 {
		t.Fatalf("first run: exit %d, want the crash of 1.2.0", code)
	}
	// What systemd would do: start again. Start 2 crashes too.
	if _, code := runOnce(t, exe, env...); code != 3 {
		t.Fatalf("second run: exit %d", code)
	}
	// Start 3: rollback, exec of 1.1.0, which refuses 1.2.0 for good.
	out, code := runOnce(t, exe, env...)
	if code != 0 {
		t.Fatalf("third run: exit %d", code)
	}
	for _, want := range []string{"did not stay up, rolled back to 1.1.0", "[1.1.0] running 1.1.0", "not tried again"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if v := version(t, exe); v != "pgcellar 1.1.0" {
		t.Errorf("binary on disk: %q", v)
	}
	failed, _ := readFailed(exe)
	if len(failed) != 1 || failed[0].Version != "1.2.0" {
		t.Errorf("failed versions: %+v", failed)
	}
	if exists(markerPath(exe)) || exists(exe+prevSuffix) {
		t.Error("marker or .prev left after the rollback")
	}
}
