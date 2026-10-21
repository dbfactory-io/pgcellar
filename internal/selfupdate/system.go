package selfupdate

import (
	"strings"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// System is everything the updater needs from the machine. Each field is a
// small function so the tests can run the whole logic in a temporary directory
// without replacing the test binary or touching the network.
type System struct {
	// Executable is the absolute path of the running binary, symbolic links
	// resolved (the file that will be replaced).
	Executable func() (string, error)
	// InContainer reports whether the process runs in a container, where the
	// image is the unit of update and the binary must never be touched.
	InContainer func() bool
	// Writable returns nil when the process may write path.
	Writable func(path string) error
	// Exec replaces the process image (syscall.Exec). It only returns on error.
	Exec func(path string, args, env []string) error
	// RunVersion runs `<path> version` and returns its output.
	RunVersion func(ctx context.Context, path string) (string, error)
	// HTTP downloads the archives.
	HTTP *http.Client
	// Now is the clock.
	Now func() time.Time
	// Platform is GOOS_GOARCH, the key of the binaries in the manifest.
	Platform string
	// Args and Environ are passed to Exec.
	Args    func() []string
	Environ func() []string
}

// DefaultSystem is the real machine.
func DefaultSystem() System {
	return System{
		Executable: func() (string, error) {
			p, err := os.Executable()
			if err != nil {
				return "", err
			}
			return filepath.EvalSymlinks(p)
		},
		InContainer: func() bool {
			// Same rule as app.InDocker: "1" or "true" forces a container,
			// any other non-empty value (such as "0") forces a plain host.
			if v := strings.TrimSpace(os.Getenv("PGCELLAR_IN_DOCKER")); v != "" {
				return v == "1" || strings.EqualFold(v, "true")
			}
			// Docker writes /.dockerenv, Podman /run/.containerenv.
			for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
				if _, err := os.Stat(f); err == nil {
					return true
				}
			}
			return false
		},
		Writable: func(path string) error {
			// W_OK = 2: the kernel answers for the real uid, which is the
			// user of the service (no setuid binary here).
			return syscall.Access(path, 2)
		},
		Exec: syscall.Exec,
		RunVersion: func(ctx context.Context, path string) (string, error) {
			out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
			return string(out), err
		},
		HTTP:     &http.Client{Timeout: 10 * time.Minute},
		Now:      time.Now,
		Platform: runtime.GOOS + "_" + runtime.GOARCH,
		Args:     func() []string { return os.Args },
		Environ:  os.Environ,
	}
}
