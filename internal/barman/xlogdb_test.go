package barman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogue(t *testing.T, home, server, file, content string) {
	t.Helper()
	dir := filepath.Join(home, server, "wals")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestReadLastWAL(t *testing.T) {
	home := t.TempDir()
	// Blank trailing lines, legacy file name.
	writeCatalogue(t, home, "s", "xlog.db", "000000010000000000000001\t10\t1000000000.5\tgzip\tNone\n000000010000000000000002\t10\t1000000100.5\tNone\tNone\n\n\n")
	_, name, when, err := ReadLastWAL(home, "s")
	if err != nil || name != "000000010000000000000002" || when.Unix() != 1000000100 {
		t.Fatalf("%q %v %v", name, when, err)
	}
}

func TestReadLastWALLargeFile(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("000000010000000000000001\t10\t1000000000.0\tgzip\tNone\n")
	}
	b.WriteString("0000000100000000000000FF\t10\t1000000999.0\tgzip\tNone\n")
	writeCatalogue(t, home, "s", "s-xlog.db", b.String())
	_, name, when, err := ReadLastWAL(home, "s")
	if err != nil || name != "0000000100000000000000FF" || when.Unix() != 1000000999 {
		t.Fatalf("%q %v %v", name, when, err)
	}
}

func TestReadLastWALEmptyMissingBroken(t *testing.T) {
	home := t.TempDir()
	writeCatalogue(t, home, "empty", "empty-xlog.db", "")
	if _, name, when, err := ReadLastWAL(home, "empty"); err != nil || name != "" || !when.IsZero() {
		t.Errorf("empty: %q %v %v", name, when, err)
	}
	path, _, _, err := ReadLastWAL(home, "absent")
	if err == nil || !strings.HasSuffix(path, filepath.Join("absent", "wals", "absent-xlog.db")) {
		t.Errorf("missing: %s %v", path, err)
	}
	writeCatalogue(t, home, "bad", "bad-xlog.db", "garbage\n")
	if _, _, _, err := ReadLastWAL(home, "bad"); err == nil {
		t.Error("garbage should fail")
	}
}
