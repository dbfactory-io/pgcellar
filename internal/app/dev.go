package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stranix79/pgcellar/internal/config"
)

const (
	devUser     = "admin"
	devPassword = "admin"
	fakeBinary  = "fake-barman"
)

// setupDev prepares --dev mode: everything that is written goes to a
// temporary directory (a copy of pgcellar.yaml, of a barman.d.example folder
// found next to it, a minimal barman.conf and the data directory), the fake
// Barman is put first on PATH, and the web user admin/admin is created.
// It returns the path of the temporary copy of pgcellar.yaml.
func (a *App) setupDev(src string, cfg *config.Config) (string, error) {
	if err := ensureFakeBarman(); err != nil {
		return "", err
	}
	// The fixtures are dated 2026-10-20 around 02:10: freeze the clock used
	// for ages and thresholds so the demo shows a healthy pg-main.
	frozen := time.Date(2026, 10, 20, 2, 30, 0, 0, time.Local)
	a.Now = func() time.Time { return frozen }
	tmp, err := os.MkdirTemp("", "pgcellar-dev-")
	if err != nil {
		return "", err
	}
	a.TempDir = tmp

	cfg.DataDir = filepath.Join(tmp, "data")
	cfg.Barman.Binary = fakeBinary
	cfg.Barman.User = ""
	cfg.Barman.ConfigFile = filepath.Join(tmp, "barman.conf")
	cfg.Barman.ConfigDir = filepath.Join(tmp, "barman.d")
	if err := os.MkdirAll(cfg.Barman.ConfigDir, 0o750); err != nil {
		return "", err
	}
	minimal := fmt.Sprintf("[barman]\nbarman_home = %s/barman\nbarman_user = barman\nlog_file = %s/barman.log\nconfiguration_files_directory = %s\n",
		tmp, tmp, cfg.Barman.ConfigDir)
	if err := os.WriteFile(cfg.Barman.ConfigFile, []byte(minimal), 0o640); err != nil {
		return "", err
	}
	if example := filepath.Join(filepath.Dir(src), "barman.d.example"); isDir(example) {
		if err := copyDir(example, cfg.Barman.ConfigDir); err != nil {
			return "", err
		}
	}

	// The example file ships a placeholder hash: dev mode always has a
	// working admin/admin.
	hash, err := config.HashPassword(devPassword)
	if err != nil {
		return "", err
	}
	found := false
	for i := range cfg.Users {
		if cfg.Users[i].Name == devUser {
			cfg.Users[i].PasswordHash, cfg.Users[i].Role = hash, RoleAdmin
			found = true
		}
	}
	if !found {
		cfg.Users = append(cfg.Users, config.User{Name: devUser, Role: RoleAdmin, PasswordHash: hash})
	}

	dst := filepath.Join(tmp, "pgcellar.yaml")
	if err := config.Save(dst, cfg); err != nil {
		return "", err
	}
	return dst, nil
}

// ensureFakeBarman puts internal/barman/testdata first on PATH unless the
// fake is already reachable.
func ensureFakeBarman() error {
	if _, err := exec.LookPath(fakeBinary); err == nil {
		return nil
	}
	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, dir := range starts {
		for ; ; dir = filepath.Dir(dir) {
			cand := filepath.Join(dir, "internal", "barman", "testdata")
			if _, err := os.Stat(filepath.Join(cand, fakeBinary)); err == nil {
				return os.Setenv("PATH", cand+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return errors.New("cannot find internal/barman/testdata/fake-barman: run from the pgCellar source tree or put fake-barman on PATH")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		in, err := os.Open(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dst, e.Name()), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		_ = in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
