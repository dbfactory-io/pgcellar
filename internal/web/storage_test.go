package web

import (
	"os"
	"strings"
	"testing"
)

// The storage line, the card and the metrics come from the cache: no Barman
// command runs to draw them.
func TestStoragePagesAndMetrics(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	count := func() int {
		b, _ := os.ReadFile(f.env.CallLog)
		return strings.Count(string(b), "\n")
	}
	before := count()
	status := f.get("/status", admin).Body.String()
	page := f.get("/servers/pg-main", admin).Body.String()
	metrics := f.get("/metrics", nil).Body.String()
	if count() != before {
		t.Fatal("storage display ran Barman")
	}
	// 1503238553 + 1509949440 + 1516560384 = 4529748377 bytes = 4.2 GiB; 7 x 16 MiB = 112.0 MiB.
	for _, want := range []string{"4.2 GiB + 112.0 MiB WAL", "Storage: ", " free of ", "% used)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q", want)
		}
	}
	for _, want := range []string{`id="storage"`, "<strong>4.2 GiB</strong>", "<strong>112.0 MiB</strong>", `class="stbar"`, "free of"} {
		if !strings.Contains(page, want) {
			t.Errorf("server page lacks %q", want)
		}
	}
	for _, want := range []string{`pgcellar_backup_bytes{server="pg-main"} 4529748377`, `pgcellar_wal_bytes{server="pg-main"} 117440512`, `pgcellar_recoveries_bytes{server="pg-main"} 0`, "pgcellar_fs_free_bytes{path=", "pgcellar_fs_size_bytes{path="} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// The fragment refreshed every 30 s carries the line too.
	if !strings.Contains(f.get("/status/table", admin).Body.String(), "Storage: ") {
		t.Error("status table fragment lacks the storage line")
	}
}
