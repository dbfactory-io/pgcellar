package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type entry struct {
	name string
	body string
	typ  byte
}

func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: typ}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type fixedSource struct {
	rel Release
	ok  bool
}

func (s fixedSource) Latest() (Release, bool) { return s.rel, s.ok }

type guardFunc func() (bool, string)

func (g guardFunc) Ready() (bool, string) { return g() }

// world is one temporary installation: a fake binary, an HTTPS server holding
// the archive, and a recorder of exec calls.
type world struct {
	t       *testing.T
	dir     string
	exe     string
	srv     *httptest.Server
	archive []byte
	execs   []string
	execErr error
	version string // what RunVersion prints
	mu      sync.Mutex
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir(), version: "pgcellar 0.2.0\n"}
	w.exe = filepath.Join(w.dir, "pgcellar")
	if err := os.WriteFile(w.exe, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.archive = tarGz(t, entry{name: "LICENSE", body: "licence"}, entry{name: "pgcellar", body: "NEW BINARY"})
	w.srv = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		b := w.archive
		w.mu.Unlock()
		_, _ = rw.Write(b)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) sys() System {
	return System{
		Executable:  func() (string, error) { return w.exe, nil },
		InContainer: func() bool { return false },
		Writable:    DefaultSystem().Writable,
		Exec: func(path string, args, env []string) error {
			b, _ := os.ReadFile(path)
			w.execs = append(w.execs, string(b))
			if w.execErr != nil {
				return w.execErr
			}
			// The real exec never returns; the tests stop here.
			return errExecDone
		},
		RunVersion: func(ctx context.Context, path string) (string, error) { return w.version, nil },
		HTTP:       w.srv.Client(),
		Now:        func() time.Time { return time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC) },
		Platform:   "linux_amd64",
		Args:       func() []string { return []string{"pgcellar", "serve"} },
		Environ:    func() []string { return nil },
	}
}

// errExecDone stands for a successful exec: in the tests, exec "returns" this
// error, which the updater handles like a failure, so tests that want to see
// the state right after a successful switch inspect it inside Exec instead.
var errExecDone = errors.New("exec reached")

func (w *world) release() Release {
	return Release{Version: "0.2.0", Binaries: map[string]Binary{
		"linux_amd64": {URL: w.srv.URL + "/pgcellar_0.2.0_linux_amd64.tar.gz", SHA256: sum(w.archive)},
	}}
}

func (w *world) updater(rel Release) *Updater {
	u := &Updater{Current: "0.1.0", Enabled: true, Source: fixedSource{rel: rel, ok: true}, Sys: w.sys()}
	u.Logf = w.t.Logf
	return u
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// leftovers are temporary files the updater should have removed.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".pgcellar-") || strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestSwitchExecsNewBinaryWithMarker(t *testing.T) {
	w := newWorld(t)
	u := w.updater(w.release())
	var seen struct {
		exe, prev string
		marker    *Marker
	}
	u.Sys.Exec = func(path string, args, env []string) error {
		seen.exe = read(t, path)
		seen.prev = read(t, path+prevSuffix)
		seen.marker, _ = readMarker(path)
		if strings.Join(args, " ") != "pgcellar serve" {
			t.Errorf("args = %v", args)
		}
		return nil // pretend the exec happened
	}
	u.Tick(context.Background())
	if seen.exe != "NEW BINARY" || seen.prev != "OLD BINARY" {
		t.Fatalf("at exec: binary %q, prev %q", seen.exe, seen.prev)
	}
	if seen.marker == nil || seen.marker.From != "0.1.0" || seen.marker.To != "0.2.0" || seen.marker.Attempts != 0 {
		t.Fatalf("marker at exec: %+v", seen.marker)
	}
	info, err := os.Stat(w.exe)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode of the new binary: %v %v", info, err)
	}
	if l := leftovers(t, w.dir); len(l) != 0 {
		t.Fatalf("temporary files left: %v", l)
	}
}

func TestBadChecksumAbandons(t *testing.T) {
	w := newWorld(t)
	rel := w.release()
	b := rel.Binaries["linux_amd64"]
	b.SHA256 = strings.Repeat("0", 64)
	rel.Binaries["linux_amd64"] = b
	u := w.updater(rel)
	u.Tick(context.Background())
	st := u.Status()
	if !strings.Contains(st.LastError, "checksum mismatch") {
		t.Fatalf("last error = %q", st.LastError)
	}
	if st.LastAttempt == nil {
		t.Fatal("no last attempt recorded")
	}
	assertUntouched(t, w)
}

func assertUntouched(t *testing.T, w *world) {
	t.Helper()
	if got := read(t, w.exe); got != "OLD BINARY" {
		t.Fatalf("binary changed: %q", got)
	}
	if exists(w.exe+prevSuffix) || exists(markerPath(w.exe)) {
		t.Fatal(".prev or marker left behind")
	}
	if len(w.execs) != 0 {
		t.Fatalf("exec called: %v", w.execs)
	}
	if l := leftovers(t, w.dir); len(l) != 0 {
		t.Fatalf("temporary files left: %v", l)
	}
}

func TestMaliciousArchiveRefused(t *testing.T) {
	for name, entries := range map[string][]entry{
		"dotdot":   {{name: "../pgcellar", body: "EVIL"}},
		"dotdot2":  {{name: "pgcellar", body: "NEW"}, {name: "a/../../etc/passwd", body: "EVIL"}},
		"absolute": {{name: "/etc/pgcellar", body: "EVIL"}},
		"symlink":  {{name: "pgcellar", typ: tar.TypeSymlink}},
		"missing":  {{name: "README.md", body: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.archive = tarGz(t, entries...)
			u := w.updater(w.release())
			u.Tick(context.Background())
			if st := u.Status(); !strings.Contains(st.LastError, "archive:") {
				t.Fatalf("last error = %q", st.LastError)
			}
			assertUntouched(t, w)
			if exists(filepath.Join(filepath.Dir(w.dir), "pgcellar")) {
				t.Fatal("a file was written outside the directory")
			}
		})
	}
}

func TestHTTPOnlyRefused(t *testing.T) {
	w := newWorld(t)
	rel := w.release()
	b := rel.Binaries["linux_amd64"]
	b.URL = strings.Replace(b.URL, "https://", "http://", 1)
	rel.Binaries["linux_amd64"] = b
	u := w.updater(rel)
	u.Tick(context.Background())
	if st := u.Status(); !strings.Contains(st.LastError, "only https") {
		t.Fatalf("last error = %q", st.LastError)
	}
	assertUntouched(t, w)
}

func TestVersionCheckFails(t *testing.T) {
	for name, out := range map[string]string{
		"wrong version":  "pgcellar 0.1.9\n",
		"prefix only":    "pgcellar 0.2.00\n",
		"does not start": "",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			u := w.updater(w.release())
			if out == "" {
				u.Sys.RunVersion = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
			} else {
				w.version = out
			}
			u.Tick(context.Background())
			if st := u.Status(); st.LastError == "" {
				t.Fatal("no error")
			}
			assertUntouched(t, w)
		})
	}
}

func TestExecFailsRestoresOldBinary(t *testing.T) {
	w := newWorld(t)
	w.execErr = errors.New("permission denied")
	u := w.updater(w.release())
	u.Tick(context.Background())
	if len(w.execs) != 1 || w.execs[0] != "NEW BINARY" {
		t.Fatalf("execs = %v", w.execs)
	}
	st := u.Status()
	if !strings.Contains(st.LastError, "old binary was put back") {
		t.Fatalf("last error = %q", st.LastError)
	}
	w.execs = nil
	assertUntouched(t, w)
}

func TestRefusals(t *testing.T) {
	cases := map[string]struct {
		setup func(w *world, u *Updater)
		want  string
	}{
		"container": {func(w *world, u *Updater) { u.Sys.InContainer = func() bool { return true } }, "container"},
		"dev":       {func(w *world, u *Updater) { u.Current = "dev" }, "development build"},
		"empty":     {func(w *world, u *Updater) { u.Current = "" }, "development build"},
		"read-only dir": {func(w *world, u *Updater) {
			u.Sys.Writable = func(p string) error {
				if p == w.dir {
					return errors.New("EACCES")
				}
				return nil
			}
		}, "directory of the binary"},
		"read-only file": {func(w *world, u *Updater) {
			u.Sys.Writable = func(p string) error {
				if p == w.exe {
					return errors.New("EACCES")
				}
				return nil
			}
		}, "binary ("},
		"no platform": {func(w *world, u *Updater) { u.Sys.Platform = "freebsd_riscv64" }, "no binary for freebsd_riscv64"},
		"window":      {func(w *world, u *Updater) { u.InWindow = func(time.Time) bool { return false } }, "update window"},
		"busy": {func(w *world, u *Updater) {
			u.Guard = guardFunc(func() (bool, string) { return false, "a backup of pg-main is running" })
		}, "a backup of pg-main is running"},
		"not newer":  {func(w *world, u *Updater) { u.Current = "0.2.0" }, ""},
		"no release": {func(w *world, u *Updater) { u.Source = fixedSource{} }, "not known"},
		"pending marker": {func(w *world, u *Updater) {
			_ = writeMarker(w.exe, Marker{From: "0.0.9", To: "0.1.0"})
		}, "not confirmed"},
		"failed before": {func(w *world, u *Updater) {
			_ = addFailed(w.exe, FailedVersion{Version: "0.2.0"})
		}, "not tried again"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			u := w.updater(w.release())
			c.setup(w, u)
			u.Tick(context.Background())
			st := u.Status()
			if c.want == "" && st.RefusedReason != "" || !strings.Contains(st.RefusedReason, c.want) {
				t.Fatalf("refused_reason = %q, want %q", st.RefusedReason, c.want)
			}
			if st.LastAttempt != nil || len(w.execs) != 0 || read(t, w.exe) != "OLD BINARY" {
				t.Fatal("an update was attempted")
			}
		})
	}
}

func TestRealPermissionCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	w := newWorld(t)
	if err := os.Chmod(w.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.dir, 0o755) })
	u := w.updater(w.release())
	u.Tick(context.Background())
	if st := u.Status(); !strings.Contains(st.RefusedReason, "not writable") {
		t.Fatalf("refused_reason = %q", st.RefusedReason)
	}
}

func TestStartupCountsThenRollsBack(t *testing.T) {
	w := newWorld(t)
	// State right after a switch: the new binary in place, the old one aside.
	if err := os.WriteFile(w.exe, []byte("NEW BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.exe+prevSuffix, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(w.exe, Marker{From: "0.1.0", To: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	sys := w.sys()
	for i := 1; i <= 2; i++ {
		r := Startup(sys, "v0.2.0", t.Logf)
		m, _ := readMarker(w.exe)
		if !r.Confirm || m == nil || m.Attempts != i {
			t.Fatalf("start %d: %+v marker %+v", i, r, m)
		}
	}
	r := Startup(sys, "0.2.0", t.Logf)
	if r.Confirm {
		t.Fatal("third start must not be confirmed")
	}
	if len(w.execs) != 1 || w.execs[0] != "OLD BINARY" {
		t.Fatalf("rollback exec: %v", w.execs)
	}
	if read(t, w.exe) != "OLD BINARY" || exists(w.exe+prevSuffix) || exists(markerPath(w.exe)) {
		t.Fatal("rollback did not restore the old binary or left files")
	}
	failed, err := readFailed(w.exe)
	if err != nil || len(failed) != 1 || failed[0].Version != "0.2.0" {
		t.Fatalf("failed versions: %+v %v", failed, err)
	}

	// The old version is running again: 0.2.0 is never retried.
	w.execs = nil
	u := w.updater(w.release())
	u.Tick(context.Background())
	st := u.Status()
	if !strings.Contains(st.RefusedReason, "not tried again") || len(st.FailedVersions) != 1 {
		t.Fatalf("status after rollback: %+v", st)
	}
	if len(w.execs) != 0 {
		t.Fatal("failed version retried")
	}
}

func TestStartupStaleMarkers(t *testing.T) {
	w := newWorld(t)
	sys := w.sys()
	if r := Startup(sys, "0.1.0", nil); r.Confirm || r.Note != "" {
		t.Fatalf("no marker: %+v", r)
	}
	_ = writeMarker(w.exe, Marker{From: "0.1.0", To: "0.2.0"})
	if r := Startup(sys, "0.1.0", nil); r.Confirm || exists(markerPath(w.exe)) {
		t.Fatalf("restarted on the old version: %+v", r)
	}
	_ = writeMarker(w.exe, Marker{From: "0.0.1", To: "0.0.2"})
	if r := Startup(sys, "0.1.0", nil); r.Confirm || exists(markerPath(w.exe)) {
		t.Fatalf("unrelated marker: %+v", r)
	}
	if err := os.WriteFile(markerPath(w.exe), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := Startup(sys, "0.1.0", nil); r.Confirm || exists(markerPath(w.exe)) {
		t.Fatalf("damaged marker: %+v", r)
	}
}

func TestConfirmRemovesMarkerWhenHealthy(t *testing.T) {
	w := newWorld(t)
	_ = os.WriteFile(w.exe+prevSuffix, []byte("OLD BINARY"), 0o755)
	_ = writeMarker(w.exe, Marker{From: "0.1.0", To: "0.2.0", Attempts: 1})
	var calls int
	var mu sync.Mutex
	health := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			http.Error(rw, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = rw.Write([]byte("ok"))
	}))
	defer health.Close()

	u := w.updater(w.release())
	u.Current = "0.2.0"
	u.Enabled = false
	u.Confirm = true
	u.HealthURL = health.URL + "/healthz"
	u.SettleDelay = 20 * time.Millisecond
	u.retry = 20 * time.Millisecond

	// Before the delay the marker is still there.
	done := make(chan struct{})
	go func() { u.confirm(context.Background()); close(done) }()
	if !exists(markerPath(w.exe)) {
		t.Fatal("marker removed before the delay")
	}
	// The first probe answers 503, the second 200.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never confirmed")
	}
	mu.Lock()
	if calls < 2 {
		t.Errorf("confirmed after %d probe(s), the first one was a 503", calls)
	}
	mu.Unlock()
	if exists(markerPath(w.exe)) {
		t.Fatal("marker still there after confirmation")
	}
	if !exists(w.exe + prevSuffix) {
		t.Fatal(".prev must be kept for a manual return")
	}
}

func TestConfirmStopsWithContext(t *testing.T) {
	w := newWorld(t)
	_ = writeMarker(w.exe, Marker{From: "0.1.0", To: "0.2.0", Attempts: 1})
	u := w.updater(w.release())
	u.SettleDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.confirm(ctx)
	if !exists(markerPath(w.exe)) {
		t.Fatal("marker removed without the delay")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true},
		{"v0.1.10", "0.1.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.1.0-3-gabc", true},
		{"0.2.0-rc.1", "0.2.0", false},
		{"1.0.0", "dev", false},
		{"0.1", "0.0.1", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestRunFollowsIsEnabled(t *testing.T) {
	w := newWorld(t)
	u := w.updater(w.release())
	var on bool
	var mu sync.Mutex
	u.IsEnabled = func() bool { mu.Lock(); defer mu.Unlock(); return on }
	u.Interval = 10 * time.Millisecond
	u.Sys.Exec = func(string, []string, []string) error { return errors.New("stop here") }
	if st := u.Status(); st.Enabled || st.RefusedReason != offReason {
		t.Fatalf("off: %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if u.Status().LastAttempt != nil {
		t.Fatal("attempted while off")
	}
	mu.Lock()
	on = true
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for u.Status().LastError == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if st := u.Status(); st.LastAttempt == nil || !strings.Contains(st.LastError, "stop here") {
		t.Fatalf("on: %+v", st)
	}
}
