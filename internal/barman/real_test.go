package barman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files in testdata/real are the exact outputs of Barman 3.20.1
// (`barman --format json <command>`) captured on a test bench.
func realOut(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRealListServers(t *testing.T) {
	names, err := ParseListServers(realOut(t, "list-servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "chutag,deckhand,listmonk,nyx,postiz,sygnet" {
		t.Errorf("names = %s", got)
	}
}

func TestRealServerOutputs(t *testing.T) {
	checks, err := ParseCheck(realOut(t, "check-chutag.json"), "chutag")
	if err != nil || len(checks) != 22 {
		t.Fatalf("check: %d %v", len(checks), err)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %s should be OK", c.Name)
		}
	}
	backups, err := ParseListBackups(realOut(t, "list-backups-chutag.json"), "chutag")
	if err != nil || len(backups) != 2 || backups[0].Status != "DONE" || backups[0].Size != 4420883 || backups[0].EndTime.IsZero() {
		t.Fatalf("list-backups: %+v %v", backups, err)
	}
	b, err := ParseShowBackup(realOut(t, "show-backup-chutag-latest.json"), "chutag")
	if err != nil || b.ID != "20261020T131229" || b.PGVersion != "16.13" || b.Size != 4404334 || b.WALSize != 16549 {
		t.Fatalf("show-backup: %+v %v", b, err)
	}
	st, err := ParseStatus(realOut(t, "status-chutag.json"), "chutag")
	if err != nil || !st.Active || st.BackupCount != 2 || st.LastBackup != "20261020T131229" {
		t.Fatalf("status: %+v %v", st, err)
	}
}
