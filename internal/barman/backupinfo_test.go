package barman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBackupInfoReal(t *testing.T) {
	b, err := ParseBackupInfo("20261020T145835", strings.NewReader(string(realFile(t, "chutag-20261020T145835-backup.info"))))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "DONE" || b.BeginWAL != "00000001000000000000000C" || b.EndWAL != "00000001000000000000000C" ||
		b.Timeline != 1 || b.Size != 4404337 || b.DeduplicatedSize != 4404337 || b.ParentID != "" ||
		b.Version != 160013 || b.SystemID != "7681232322925416483" || b.SegmentSize != 16777216 {
		t.Fatalf("%+v", b)
	}
	want := time.Date(2026, 10, 20, 12, 58, 35, 703214000, time.UTC)
	if !b.BeginTime.Equal(want) || !b.EndTime.Equal(time.Date(2026, 10, 20, 12, 58, 48, 641004000, time.UTC)) {
		t.Errorf("times %v %v", b.BeginTime, b.EndTime)
	}
	if b.Incremental() {
		t.Error("not incremental")
	}
}

func TestParseBackupInfoErrorsAndNone(t *testing.T) {
	if _, err := ParseBackupInfo("x", strings.NewReader("size=1\n")); err == nil {
		t.Error("no status/begin_time must fail")
	}
	b, err := ParseBackupInfo("x", strings.NewReader("status=FAILED\nbegin_time=2026-10-20 12:00:00+00:00\nsize=None\nend_time=None\nparent_backup_id=20261019T020001\n"))
	if err != nil || b.Size != 0 || !b.EndTime.IsZero() || !b.Incremental() {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestReadBackupInfosMetaAndBaseFallback(t *testing.T) {
	home := t.TempDir()
	meta := filepath.Join(home, "chutag", "meta")
	if err := os.MkdirAll(meta, 0o750); err != nil {
		t.Fatal(err)
	}
	// Written in reverse order: the result is sorted by begin time.
	for _, id := range []string{"20261020T145835", "20261020T131229", "20261020T123810"} {
		if err := os.WriteFile(filepath.Join(meta, id+"-backup.info"), realFile(t, "chutag-"+id+"-backup.info"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// An older Barman layout, and an id also present in meta/ (meta wins).
	for _, id := range []string{"20261001T020000", "20261020T123810"} {
		dir := filepath.Join(home, "chutag", "base", id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		body := "status=DONE\nbegin_time=2026-10-01 02:00:00+00:00\nend_time=2026-10-01 02:05:00+00:00\nsize=100\n"
		if err := os.WriteFile(filepath.Join(dir, "backup.info"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(meta, "20261020T000000-backup.info"), []byte("garbage"), 0o640); err != nil {
		t.Fatal(err)
	}
	infos, skipped, err := ReadBackupInfos(home, "chutag")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range infos {
		ids = append(ids, b.ID)
	}
	if got := strings.Join(ids, " "); got != "20261001T020000 20261020T123810 20261020T131229 20261020T145835" {
		t.Errorf("ids = %s", got)
	}
	if infos[1].Size != 4404333 {
		t.Errorf("meta must win over base for the same id: %+v", infos[1])
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "20261020T000000") {
		t.Errorf("skipped = %v", skipped)
	}
	if _, _, err := ReadBackupInfos(home, "absent"); err == nil {
		t.Error("no meta/ and no base/ must be an error")
	}
}
