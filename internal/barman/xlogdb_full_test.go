package barman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWALName(t *testing.T) {
	tli, log, seg, ok := ParseWALName("0000000200000003000000FE")
	if !ok || tli != 2 || log != 3 || seg != 0xFE {
		t.Fatalf("%d %d %d %v", tli, log, seg, ok)
	}
	for _, bad := range []string{"", "00000002.history", "000000010000000000000007.partial", "00000001000000000000000G", "0000000100000000000000"} {
		if _, _, _, ok := ParseWALName(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
	// 256 segments of 16 MiB make one log; 64 MiB segments make 64.
	if SegmentIndex(1, 0, 0) != 256 || SegmentIndex(1, 0, 64<<20) != 64 {
		t.Error("segment index")
	}
}

func TestReadXlogDBReal(t *testing.T) {
	home := t.TempDir()
	writeCatalogue(t, home, "chutag", "chutag-xlog.db", string(realFile(t, "chutag-xlog-full.db")))
	files, err := ReadXlogDB(home, "chutag")
	if err != nil || len(files) != 7 {
		t.Fatalf("%d %v", len(files), err)
	}
	if files[0].Name != "000000010000000000000007" || files[6].Name != "00000001000000000000000D" || files[6].Seg != 0xD || files[6].Size != 16600 || files[6].Compression != "gzip" {
		t.Errorf("%+v %+v", files[0], files[6])
	}
	if files[6].Time.Unix() != 1792501138 {
		t.Errorf("time %v", files[6].Time)
	}
	if g := FindWALGaps(files, 16<<20); len(g) != 0 {
		t.Errorf("the real chain has no gap: %+v", g)
	}
}

func wal(tli, seg int, epoch int64, size int) string {
	return fmt.Sprintf("%08X%08X%08X\t%d\t%d.5\tgzip\tNone\n", tli, 0, seg, size, epoch+1792000000)
}

func TestReadXlogDBSortsAndSkipsOtherFiles(t *testing.T) {
	home := t.TempDir()
	body := wal(2, 5, 3000, 10) + wal(1, 3, 2000, 10) + "00000001000000000000000A.00000028.backup\t100\t1792002100.0\tNone\tNone\n" + "00000002.history\t50\t1792002900.0\tNone\tNone\n\n" + wal(1, 2, 1000, 10)
	writeCatalogue(t, home, "s", "s-xlog.db", body)
	files, err := ReadXlogDB(home, "s")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, fmt.Sprintf("%d:%d", f.Timeline, f.Seg))
	}
	if strings.Join(names, " ") != "1:2 1:3 2:5" {
		t.Errorf("order %v", names)
	}
	writeCatalogue(t, home, "bad", "bad-xlog.db", "000000010000000000000001\tx\n")
	if _, err := ReadXlogDB(home, "bad"); err == nil {
		t.Error("short line must fail")
	}
	if _, err := ReadXlogDB(home, "absent"); err == nil {
		t.Error("missing file must fail")
	}
}

func TestFindWALGaps(t *testing.T) {
	home := t.TempDir()
	// Timeline 1 misses 4 and 5 (so two segments), then 8; timeline 2 is whole.
	body := wal(1, 1, 1000, 1) + wal(1, 2, 1100, 1) + wal(1, 3, 1200, 1) + wal(1, 6, 1300, 1) + wal(1, 7, 1400, 1) + wal(1, 9, 1500, 1) + wal(2, 7, 1600, 1) + wal(2, 8, 1700, 1)
	writeCatalogue(t, home, "s", "s-xlog.db", body)
	files, _ := ReadXlogDB(home, "s")
	gaps := FindWALGaps(files, 16<<20)
	if len(gaps) != 2 || gaps[0].Missing != 2 || gaps[0].First != 4 || gaps[0].After.Seg != 6 || gaps[1].Missing != 1 || gaps[1].Timeline != 1 {
		t.Fatalf("%+v", gaps)
	}
	// The gap crosses a log boundary: 0/FF then 1/01 misses 1/00.
	files = []WALFile{{Timeline: 1, Log: 0, Seg: 0xFF}, {Timeline: 1, Log: 1, Seg: 1}}
	if g := FindWALGaps(files, 0); len(g) != 1 || g[0].Missing != 1 {
		t.Errorf("log boundary: %+v", g)
	}
	files = []WALFile{{Timeline: 1, Log: 0, Seg: 0xFF}, {Timeline: 1, Log: 1, Seg: 0}}
	if g := FindWALGaps(files, 0); len(g) != 0 {
		t.Errorf("0/FF then 1/00 are consecutive: %+v", g)
	}
}

func TestReadHistories(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "s", "wals")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	h := "# comment\n1\t0/5000060\tno recovery target specified\n"
	if err := os.WriteFile(filepath.Join(dir, "00000002.history"), []byte(h), 0o640); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	_ = os.Chtimes(filepath.Join(dir, "00000002.history"), at, at)
	_ = os.WriteFile(filepath.Join(dir, "00000003.history"), []byte("2\t1/10\tat restore point\n"), 0o640)
	_ = os.WriteFile(filepath.Join(dir, "garbage.history"), []byte("x"), 0o640)
	got := ReadHistories(home, "s")
	if len(got) != 2 || got[0].Timeline != 2 || got[0].Parent != 1 || got[0].Switch != "0/5000060" || got[0].Reason != "no recovery target specified" || !got[0].Time.Equal(at) {
		t.Fatalf("%+v", got)
	}
	if idx, ok := got[0].SwitchIndex(0); !ok || idx != 0x5000060/(16<<20) {
		t.Errorf("switch index %d %v", idx, ok)
	}
	if idx, _ := got[1].SwitchIndex(0); idx != 256 {
		t.Errorf("1/10 is segment 256: %d", idx)
	}
	if ReadHistories(home, "absent") != nil {
		t.Error("absent dir")
	}
}
