package web

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stranix79/pgcellar/internal/app"
)

// storageLine is the "Storage: ..." sentence of the Status page, one per file
// system that holds backups.
type storageLine struct {
	Level string // ok | warn | fail
	Text  string
}

// storageLines lists each file system once (by the path shown), worst first
// on ties of path order, with the sentence for people.
func storageLines(states []app.ServerState) []storageLine {
	seen := map[string]bool{}
	var fss []*app.FSUsage
	for _, st := range states {
		if st.Storage == nil {
			continue
		}
		for _, f := range []*app.FSUsage{st.Storage.FS, st.Storage.RecoveriesFS} {
			if f != nil && !seen[f.Path] {
				seen[f.Path] = true
				fss = append(fss, f)
			}
		}
	}
	sort.SliceStable(fss, func(i, j int) bool { return fss[i].Path < fss[j].Path })
	out := make([]storageLine, 0, len(fss))
	for _, f := range fss {
		out = append(out, storageLine{
			Level: f.Level,
			Text:  fmt.Sprintf(tr("storage.status_line"), humanSizeZero(f.FreeBytes), humanSizeZero(f.TotalBytes), f.Path, f.UsedPct()),
		})
	}
	return out
}

func humanSizeZero(n int64) string { return app.FormatSize(n) }

// writeStorageMetrics prints the storage gauges of the Prometheus page.
func writeStorageMetrics(b *strings.Builder, states []app.ServerState) {
	per := func(name, help string, value func(*app.StorageInfo) int64) {
		_, _ = fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, st := range states {
			if st.Storage != nil {
				_, _ = fmt.Fprintf(b, "%s{server=\"%s\"} %d\n", name, promEscape(st.Name), value(st.Storage))
			}
		}
	}
	per("pgcellar_backup_bytes", "Size of the finished backups of the server (sum of size in backup.info).", func(s *app.StorageInfo) int64 { return s.BackupBytes })
	per("pgcellar_wal_bytes", "Size of the archived WAL files of the server (sum of the WAL catalogue).", func(s *app.StorageInfo) int64 { return s.WALBytes })
	per("pgcellar_recoveries_bytes", "Size of the restores of the server under recoveries.dir.", func(s *app.StorageInfo) int64 { return s.RecoveriesBytes })
	seen := map[string]bool{}
	var fss []*app.FSUsage
	for _, st := range states {
		if st.Storage == nil {
			continue
		}
		for _, f := range []*app.FSUsage{st.Storage.FS, st.Storage.RecoveriesFS} {
			if f != nil && !seen[f.Path] {
				seen[f.Path] = true
				fss = append(fss, f)
			}
		}
	}
	for _, m := range []struct {
		name, help string
		v          func(*app.FSUsage) int64
	}{
		{"pgcellar_fs_free_bytes", "Space available to a normal user on the file system holding backups (or restores).", func(f *app.FSUsage) int64 { return f.FreeBytes }},
		{"pgcellar_fs_size_bytes", "Size of the file system holding backups (or restores).", func(f *app.FSUsage) int64 { return f.TotalBytes }},
	} {
		_, _ = fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", m.name, m.help, m.name)
		for _, f := range fss {
			_, _ = fmt.Fprintf(b, "%s{path=\"%s\"} %d\n", m.name, promEscape(f.Path), m.v(f))
		}
	}
}
