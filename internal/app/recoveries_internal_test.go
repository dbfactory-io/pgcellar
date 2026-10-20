package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/store"
)

func TestRecoveryDestStaysUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	got, err := recoveryDest(root, "pg-main", "20261020T020001", at)
	if err != nil || got != filepath.Join(root, "pg-main", "20261020T020001-20261020T093000") {
		t.Fatalf("%q %v", got, err)
	}
	for _, c := range []struct{ server, id string }{
		{"..", "20261020T020001"},
		{"../etc", "20261020T020001"},
		{"pg-main/../../x", "20261020T020001"},
		{"", "20261020T020001"},
		{"pg-main", "../../etc"},
		{"pg-main", "latest"},
		{"pg-main", ""},
	} {
		if p, err := recoveryDest(root, c.server, c.id, at); err == nil {
			t.Errorf("%q/%q must be refused, got %q", c.server, c.id, p)
		}
	}
}

func TestUnderRecoveries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "srv", "r1"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A symbolic link that leads out of the root.
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "srv", "link")); err != nil {
		t.Fatal(err)
	}
	ok := []string{filepath.Join(root, "srv", "r1"), filepath.Join(root, "srv", "r1.tar.gz"), filepath.Join(root, "new", "r1")}
	for _, p := range ok {
		if err := underRecoveries(root, p); err != nil && err != errNothingThere {
			t.Errorf("%s: %v", p, err)
		}
	}
	bad := []string{
		root, filepath.Join(root, "srv"), filepath.Join(root, "srv", "r1", "deeper"),
		filepath.Join(root, "..", "x", "y"), outside, filepath.Join(outside, "a", "b"),
		filepath.Join(root, "evil", "x"),   // parent is a link to elsewhere
		filepath.Join(root, "srv", "link"), // the entry itself is a link
		"/", "/etc/passwd", "relative/path",
	}
	for _, p := range bad {
		if err := underRecoveries(root, p); err == nil || err == errNothingThere {
			t.Errorf("%s must be refused, got %v", p, err)
		}
	}
	if !strings.Contains(underRecoveries(root, outside).Error(), "recoveries directory") {
		t.Error("the refusal must say why")
	}
}

func TestRemoveRecoveryFilesRefusesForeignPaths(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(t.TempDir(), "precious", "data")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.Recovery{
		{Path: victim},
		{Path: filepath.Join(root, "srv", "r1"), ArchivePath: victim + ".tar.gz"},
		{Path: filepath.Join(root, "srv", "r1"), ArchivePath: filepath.Join(root, "srv", "other.tar.gz")},
	} {
		if err := removeRecoveryFiles(root, r); err == nil {
			t.Errorf("%+v must be refused", r)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the foreign directory was touched: %v", err)
	}
}

func TestWriteTarGzSkipsNothingThatMatters(t *testing.T) {
	src := filepath.Join(t.TempDir(), "restore-1")
	if err := os.MkdirAll(filepath.Join(src, "base", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "base", "1", "f"), []byte(strings.Repeat("x", 100000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("base/1/f", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	out := src + ".tar.gz"
	n, err := writeTarGz(src, out)
	if err != nil || n <= 0 || n >= 100000 {
		t.Fatalf("size %d (gzip must shrink repeated bytes): %v", n, err)
	}
	if _, err := os.Stat(out + ".partial"); !os.IsNotExist(err) {
		t.Error("the .partial file must be gone")
	}
}

func TestDrillDestIsUniqueAndUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	first, err := drillDest(root, "pg-main", at)
	if err != nil || first != filepath.Join(root, "pg-main", "drill-20261020T031000") {
		t.Fatalf("%q %v", first, err)
	}
	if err := os.MkdirAll(first, 0o750); err != nil {
		t.Fatal(err)
	}
	second, _ := drillDest(root, "pg-main", at)
	if second != first+"-2" {
		t.Errorf("a second drill in the same second gets its own directory: %q", second)
	}
	for _, bad := range []string{"..", "a/b", "", "../x"} {
		if p, err := drillDest(root, bad, at); err == nil {
			t.Errorf("%q must be refused, got %q", bad, p)
		}
	}
}
