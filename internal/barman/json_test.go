package barman

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseListServers(t *testing.T) {
	names, err := ParseListServers(fixture(t, "list-servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "broken" || names[2] != "pg-rsync" {
		t.Errorf("names = %v", names)
	}
	names, _ = ParseListServers(fixture(t, "list-servers-minimal.json"))
	if len(names) != 3 || names[1] != "pg-main" {
		t.Errorf("minimal names = %v", names)
	}
}

func TestParseCheck(t *testing.T) {
	res, err := ParseCheck(fixture(t, "check-broken.json"), "broken")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CheckResult{}
	for _, c := range res {
		byName[c.Name] = c
	}
	if !byName["directories"].OK || byName["postgresql"].OK || byName["ssh"].OK {
		t.Errorf("statuses wrong: %+v", res)
	}
	if byName["receive_wal_running"].Hint != "See the Barman log file for more details" {
		t.Errorf("hint = %q", byName["receive_wal_running"].Hint)
	}
	if _, err := ParseCheck(fixture(t, "check-broken.json"), "other"); err == nil {
		t.Error("wrong server name must be an error")
	}
	if _, err := ParseCheck([]byte(`{"_ERROR": ["Unknown server 'x'"]}`), "x"); err == nil || err.Error() != "Unknown server 'x'" {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseCheck([]byte("not json"), "x"); err == nil {
		t.Error("garbage must be an error")
	}
	// A warning printed before the JSON is tolerated.
	if res, err := ParseCheck(append([]byte("WARNING: something\n"), fixture(t, "check-pg-main.json")...), "pg-main"); err != nil || len(res) == 0 {
		t.Errorf("prefixed output: %v %v", res, err)
	}
}

func TestParseListBackups(t *testing.T) {
	list, err := ParseListBackups(fixture(t, "list-backups-pg-main.json"), "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d backups", len(list))
	}
	b := list[2]
	if b.ID != "20261020T020001" || b.Status != "DONE" || b.Size != 1516560384 || b.WALSize != 20971520 {
		t.Errorf("backup = %+v", b)
	}
	if want := time.Unix(1792455030, 0); !b.EndTime.Equal(want) {
		t.Errorf("end time = %v", b.EndTime)
	}
	if b.BeginTime.Format("2006-01-02 15:04:05") != "2026-10-20 02:00:01" {
		t.Errorf("begin time = %v", b.BeginTime)
	}
	if b.Raw["retention_status"] != "VALID" {
		t.Errorf("raw = %v", b.Raw)
	}
	failed, _ := ParseListBackups(fixture(t, "list-backups-pg-rsync.json"), "pg-rsync")
	if len(failed) != 2 || failed[1].Status != "FAILED" || !failed[1].EndTime.IsZero() {
		t.Errorf("failed entry = %+v", failed)
	}
	// Empty list and minimal form.
	if l, err := ParseListBackups([]byte(`{"s": []}`), "s"); err != nil || len(l) != 0 {
		t.Errorf("empty = %v %v", l, err)
	}
	if l, _ := ParseListBackups([]byte(`{"s": ["20261020T020001"]}`), "s"); len(l) != 1 || l[0].ID != "20261020T020001" {
		t.Errorf("minimal = %v", l)
	}
	// ctime-only end time (no epoch) still parses.
	l, _ := ParseListBackups([]byte(`{"s": [{"backup_id":"20261020T020001","end_time":"Tue Oct 20 02:10:30 2026","size_bytes":"12","status":"DONE"}]}`), "s")
	if len(l) != 1 || l[0].EndTime.Format("15:04:05") != "02:10:30" || l[0].Size != 12 {
		t.Errorf("ctime = %+v", l)
	}
}

func TestParseShowBackup(t *testing.T) {
	b, err := ParseShowBackup(fixture(t, "show-backup-pg-main-20261020T020001.json"), "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "20261020T020001" || b.Status != "DONE" || b.PGVersion != "16.4" {
		t.Errorf("backup = %+v", b)
	}
	if b.Size != 1516560384 || b.WALSize != 20971520 {
		t.Errorf("sizes = %d %d", b.Size, b.WALSize)
	}
	if !b.BeginTime.Equal(time.Unix(1792454401, 0)) || !b.EndTime.Equal(time.Unix(1792455030, 0)) {
		t.Errorf("times = %v %v", b.BeginTime, b.EndTime)
	}
	if _, ok := b.Raw["base_backup_information"]; !ok {
		t.Error("Raw must hold the whole object")
	}
	r, err := ParseShowBackup(fixture(t, "show-backup-pg-rsync-20261019T230001.json"), "pg-rsync")
	if err != nil || r.PGVersion != "13.14" {
		t.Errorf("rsync = %+v %v", r, err)
	}
	// Failed backup: no base_backup_information at all.
	f, err := ParseShowBackup([]byte(`{"s": {"backup_id": "20261020T230001", "status": "FAILED", "error": "boom"}}`), "s")
	if err != nil || f.Status != "FAILED" || f.Raw["error"] != "boom" {
		t.Errorf("failed = %+v %v", f, err)
	}
}

func TestParseStatus(t *testing.T) {
	st, err := ParseStatus(fixture(t, "status-pg-main.json"), "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.BackupCount != 3 || st.LastBackup != "20261020T020001" {
		t.Errorf("status = %+v", st)
	}
	if st.LastWAL != "000000010000000000000007" || st.LastWALTime.Format("2006-01-02 15:04:05") != "2026-10-20 02:10:29" {
		t.Errorf("last wal = %q %v", st.LastWAL, st.LastWALTime)
	}
	r, _ := ParseStatus(fixture(t, "status-pg-rsync.json"), "pg-rsync")
	if r.LastWAL != "" || !r.LastWALTime.IsZero() {
		t.Errorf("no WAL yet: %+v", r)
	}
	b, _ := ParseStatus(fixture(t, "status-broken.json"), "broken")
	if b.LastBackup != "" || b.BackupCount != 0 {
		t.Errorf("broken = %+v", b)
	}
	off, _ := ParseStatus([]byte(`{"s": {"active": {"description":"Active","message":"False"}}}`), "s")
	if off.Active {
		t.Error("inactive server not detected")
	}
}

func TestPGVersion(t *testing.T) {
	for in, want := range map[string]string{"160004": "16.4", "90624": "9.6.24", "15": "15", "": ""} {
		if got := pgVersion(in); got != want {
			t.Errorf("pgVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
