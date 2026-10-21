package demo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// backup is one entry of the catalogue of a server.
type backup struct {
	occ
	ID       string
	Size     int64
	Disk     int64
	BeginWAL int64
	EndWAL   int64
	WALBytes int64
	Oldest   bool
}

// walRange is the span of segments the catalogue of a server holds.
func (p plan) walRange(s server, now time.Time, bs []backup) (first, last int64, ok bool) {
	if len(bs) == 0 {
		return 0, 0, false
	}
	oldest := bs[len(bs)-1].Start
	switch s.Name {
	case Legacy9:
		first = segOf(s, oldest.Add(-3*s.SegEvery))
		last = segOf(s, bs[0].End.Add(3*s.SegEvery))
	case ShopStaging:
		first = segOf(s, oldest.Add(-2*s.SegEvery))
		last = segOf(s, p.streamingStopped()) - 1
	default:
		first = segOf(s, oldest.Add(-2*s.SegEvery))
		// A segment is archived when it is full, a few seconds later.
		last = segOf(s, now.Add(-20*time.Second)) - 1
	}
	return first, last, last >= first
}

func (p plan) catalogue(s server, now time.Time) []backup {
	list, _ := p.backups(s, now)
	out := make([]backup, 0, len(list))
	for i, o := range list {
		b := backup{occ: o, ID: backupID(o.Start), Oldest: i == len(list)-1}
		if o.Status == "DONE" {
			b.Size = sizeOf(s, o, now)
			b.Disk = diskOf(s, b.Size, b.Oldest)
		}
		b.BeginWAL, b.EndWAL = segOf(s, o.Start), segOf(s, o.End)
		out = append(out, b)
	}
	// WAL volume between a backup and the next one, from the catalogue itself.
	for i := range out {
		next := now
		if i > 0 {
			next = out[i-1].Start
		}
		n := int64(next.Sub(out[i].End) / s.SegEvery)
		if n < 0 {
			n = 0
		}
		out[i].WALBytes = n * s.SegSize
	}
	return out
}

// ---- Barman's own files ----

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

// writeHome writes, under barman_home, what pgCellar reads of a server without
// running Barman: the backup.info files, the WAL catalogue and the streaming
// directory.
func (p plan) writeHome(home string, s server, now time.Time) error {
	bs := p.catalogue(s, now)
	meta := filepath.Join(home, s.Name, "meta")
	if err := os.MkdirAll(meta, 0o750); err != nil {
		return err
	}
	want := map[string]bool{}
	const layout = "2006-01-02 15:04:05.000000-07:00"
	for _, b := range bs {
		name := b.ID + "-backup.info"
		want[name] = true
		var body strings.Builder
		fmt.Fprintf(&body, "backup_label=None\nbegin_offset=40\nbegin_time=%s\n", b.Start.Format(layout))
		fmt.Fprintf(&body, "begin_wal=%s\nversion=%d\nsystemid=%s\ntimeline=1\nxlog_segment_size=16777216\nparent_backup_id=None\n",
			walName(b.BeginWAL), s.VersionNum, s.SystemID)
		if b.Status == "DONE" {
			fmt.Fprintf(&body, "end_time=%s\nend_wal=%s\nsize=%d\ndeduplicated_size=%d\nstatus=DONE\n",
				b.End.Format(layout), walName(b.EndWAL), b.Size, b.Disk)
		} else {
			fmt.Fprintf(&body, "end_time=None\nend_wal=None\nsize=None\ndeduplicated_size=None\nstatus=FAILED\nerror=the WAL streaming connection was lost\n")
		}
		if err := atomicWrite(filepath.Join(meta, name), []byte(body.String())); err != nil {
			return err
		}
	}
	if entries, err := os.ReadDir(meta); err == nil {
		for _, e := range entries {
			if !want[e.Name()] {
				_ = os.Remove(filepath.Join(meta, e.Name()))
			}
		}
	}

	// WAL catalogue.
	var wal strings.Builder
	if first, last, ok := p.walRange(s, now, bs); ok {
		for n := first; n <= last; n++ {
			at := segStart(s, n).Add(s.SegEvery + 20*time.Second)
			fmt.Fprintf(&wal, "%s\t%d\t%d.0\tgzip\tNone\n", walName(n), segmentSize(s, n), at.Unix())
		}
	}
	if err := atomicWrite(filepath.Join(home, s.Name, "wals", s.Name+"-xlog.db"), []byte(wal.String())); err != nil {
		return err
	}
	return p.touchStreaming(home, s, now)
}

// touchStreaming keeps the file the WAL receiver "writes into": its date is
// the time data last reached Barman.
func (p plan) touchStreaming(home string, s server, now time.Time) error {
	if s.WAL == app.WALArchiveCommand {
		return nil
	}
	dir := filepath.Join(home, s.Name, "streaming")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	at := now.Add(-25 * time.Second)
	n := segOf(s, now)
	if s.Name == ShopStaging { // the receiver died
		at = p.streamingStopped()
		n = segOf(s, at)
	}
	name := walName(n) + ".partial"
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != name {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		return err
	}
	return os.Chtimes(path, at, at)
}

// ---- what the fake Barman and the fake psql answer ----

func humanBytes(n int64) string { return app.FormatSize(n) }

func barmanTime(t time.Time) string { return t.Format("Mon Jan _2 15:04:05 2006") }

func (p plan) writeFixtures(dir string, all []server, now time.Time) error {
	list := map[string]any{}
	for _, s := range all {
		list[s.Name] = map[string]string{"description": s.Desc}
	}
	if err := writeJSON(filepath.Join(dir, "list-servers.json"), list); err != nil {
		return err
	}
	for _, s := range all {
		bs := p.catalogue(s, now)
		if err := p.writeServerFixtures(dir, s, bs, now); err != nil {
			return err
		}
	}
	return nil
}

func (p plan) writeServerFixtures(dir string, s server, bs []backup, now time.Time) error {
	// list-backups, oldest first like Barman.
	var rows []any
	var done []backup
	for i := len(bs) - 1; i >= 0; i-- {
		b := bs[i]
		row := map[string]any{"backup_id": b.ID, "backup_type": s.Method, "retention_status": "VALID", "status": b.Status}
		if b.Status == "DONE" {
			row["end_time"] = barmanTime(b.End)
			row["end_time_timestamp"] = fmt.Sprint(b.End.Unix())
			row["size"] = humanBytes(b.Size)
			row["size_bytes"] = b.Size
			row["wal_size"] = humanBytes(b.WALBytes)
			row["wal_size_bytes"] = b.WALBytes
			done = append(done, b)
		}
		rows = append(rows, row)
		if err := writeJSON(filepath.Join(dir, "show-backup-"+s.Name+"-"+b.ID+".json"), map[string]any{s.Name: showBackup(s, b, bs, i)}); err != nil {
			return err
		}
	}
	// Remove the show-backup files of backups that are gone.
	keep := map[string]bool{}
	for _, b := range bs {
		keep["show-backup-"+s.Name+"-"+b.ID+".json"] = true
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "show-backup-"+s.Name+"-") && !keep[e.Name()] {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if err := writeJSON(filepath.Join(dir, "list-backups-"+s.Name+".json"), map[string]any{s.Name: rows}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "check-"+s.Name+".json"), map[string]any{s.Name: p.checks(s, bs, now)}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "status-"+s.Name+".json"), map[string]any{s.Name: p.status(s, bs, done, now)}); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "psql-"+s.Name+".tsv"), []byte(p.settings(s)))
}

func showBackup(s server, b backup, all []backup, idx int) map[string]any {
	prev := "- (this is the first base backup)"
	if idx+1 < len(all) {
		prev = all[idx+1].ID
	}
	next := "- (this is the latest base backup)"
	if idx > 0 {
		next = all[idx-1].ID
	}
	bi := map[string]any{
		"backup_method": s.Method, "begin_lsn": fmt.Sprintf("%X/%X", b.BeginWAL/256, (b.BeginWAL%256)<<24|0x28),
		"begin_offset": 40, "begin_time": b.Start.Format("2006-01-02 15:04:05.000000-07:00"),
		"begin_time_timestamp": fmt.Sprint(b.Start.Unix()), "begin_wal": walName(b.BeginWAL), "compression": nil,
		"encryption": nil, "number_of_workers": 1, "timeline": 1,
	}
	out := map[string]any{
		"backup_id": b.ID, "backup_type": s.Method, "base_backup_information": bi,
		"catalog_information": map[string]any{"children_backup_ids": nil, "next_backup": next, "previous_backup": prev, "retention_policy": "VALID"},
		"pgdata_directory":    fmt.Sprintf("/var/lib/postgresql/%d/main", s.Major), "postgresql_version": s.VersionNum,
		"server_information": map[string]any{"data_checksums": "on", "summarize_wal": nil},
		"status":             b.Status, "system_id": s.SystemID, "tablespaces": []any{},
	}
	if b.Status != "DONE" {
		bi["error"] = "the WAL streaming connection was lost"
		return out
	}
	bi["backup_size"], bi["backup_size_bytes"] = humanBytes(b.Size), b.Size
	bi["backup_size_with_wals"], bi["backup_size_with_wals_bytes"] = humanBytes(b.Size+b.WALBytes), b.Size+b.WALBytes
	bi["end_time"] = b.End.Format("2006-01-02 15:04:05.000000-07:00")
	bi["end_time_timestamp"] = fmt.Sprint(b.End.Unix())
	bi["end_wal"] = walName(b.EndWAL)
	bi["end_lsn"] = fmt.Sprintf("%X/%X", b.EndWAL/256, (b.EndWAL%256)<<24)
	secs := b.End.Sub(b.Start).Seconds()
	bi["copy_time_seconds"] = secs
	bi["copy_time"] = fmt.Sprintf("%d minutes, %d seconds", int(secs)/60, int(secs)%60)
	tp := int64(float64(b.Size) / secs)
	bi["throughput"], bi["throughput_bytes"] = humanBytes(tp)+"/s", tp
	bi["wal_num"], bi["wal_size"], bi["wal_size_bytes"] = int(b.EndWAL-b.BeginWAL+1), humanBytes(b.WALBytes), b.WALBytes
	out["cluster_size"], out["cluster_size_bytes"] = humanBytes(s.Cluster), s.Cluster
	out["wal_information"] = map[string]any{"compression_ratio": 0, "disk_usage": humanBytes(b.WALBytes), "disk_usage_bytes": b.WALBytes,
		"last_available": walName(b.EndWAL), "no_of_files": int(b.EndWAL - b.BeginWAL + 1), "timelines": []any{}, "wal_rate": 0, "wal_rate_per_second": 0}
	return out
}

type chk struct {
	Hint   string `json:"hint"`
	Status string `json:"status"`
}

func (p plan) checks(s server, bs []backup, now time.Time) map[string]chk {
	ok := func(h string) chk { return chk{Hint: h, Status: "OK"} }
	nDone := 0
	failed := 0
	for _, b := range bs {
		switch b.Status {
		case "DONE":
			nDone++
		case "FAILED":
			failed++
		}
	}
	c := map[string]chk{
		"directories":                     ok(""),
		"compression_settings":            ok(""),
		"failed_backups":                  ok(fmt.Sprintf("there are %d failed backups", 0)),
		"minimum_redundancy_requirements": ok(fmt.Sprintf("have %d backups, expected at least %d", nDone, s.Ret.MinimumRedundancy)),
		"postgresql":                      ok(""),
		"retention_policy_settings":       ok(""),
		"superuser_or_standard_user_with_backup_privileges": ok(""),
		"wal_level": ok(""),
	}
	if failed > 0 {
		c["failed_backups"] = chk{Hint: fmt.Sprintf("there are %d failed backups", failed), Status: "FAILED"}
	}
	if s.Method == "rsync" {
		c["ssh"] = ok("")
	} else {
		for _, k := range []string{"pg_basebackup", "pg_basebackup_compatible", "pg_basebackup_supports_tablespaces_mapping"} {
			c[k] = ok("")
		}
	}
	if s.WAL != app.WALArchiveCommand {
		c["postgresql_streaming"] = ok("")
		c["pg_receivexlog"] = ok("")
		c["pg_receivexlog_compatible"] = ok("")
		c["receive_wal_running"] = ok("")
		c["replication_slot"] = ok("")
	}
	if s.WAL != app.WALStreaming {
		c["archiver_errors"] = ok("")
		c["wal_archive"] = ok("")
	}
	if s.Name == ShopStaging {
		c["receive_wal_running"] = chk{Hint: "No WAL receiver process for this server: streaming stopped " + app.HumanDuration(now.Sub(p.streamingStopped())) + " ago", Status: "FAILED"}
	}
	if s.Name == Legacy9 {
		c["wal_archive"] = ok("")
	}
	return c
}

func (p plan) status(s server, bs, done []backup, now time.Time) map[string]any {
	f := func(d, m string) map[string]string { return map[string]string{"description": d, "message": m} }
	first, last := "None", "None"
	if len(done) > 0 {
		first, last = done[0].ID, done[len(done)-1].ID
	}
	return map[string]any{
		"active": f("Active", "True"), "active_model": f("Active configuration model", "None"),
		"backups_number": f("No. of available backups", fmt.Sprint(len(done))),
		"current_size":   f("Current data size", humanBytes(s.Cluster)),
		"current_xlog":   f("Current WAL segment", walName(segOf(s, now))),
		"data_directory": f("PostgreSQL Data directory", fmt.Sprintf("/var/lib/postgresql/%d/main", s.Major)),
		"description":    f("Description", s.Desc), "disabled": f("Disabled", "False"),
		"first_backup": f("First available backup", first), "last_backup": f("Last available backup", last),
		"is_in_recovery":     f("Cluster state", "in production"),
		"minimum_redundancy": f("Minimum redundancy requirements", fmt.Sprintf("satisfied (%d/%d)", len(done), s.Ret.MinimumRedundancy)),
		"passive_node":       f("Passive node", "False"), "pg_version": f("PostgreSQL version", s.PGVersion),
		"retention_policies": f("Retention policies", fmt.Sprintf("enforced (mode: auto, retention: RECOVERY WINDOW OF %d DAYS, WAL retention: MAIN)", s.Ret.RecoveryWindowDays)),
	}
}

// settings is what the fake psql answers to the server-setup query.
func (p plan) settings(s server) string {
	var b strings.Builder
	row := func(name, val, unit, ctx string) { fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", name, val, unit, ctx) }
	archive := s.WAL != app.WALStreaming
	if archive {
		row("archive_mode", "on", "", "postmaster")
		row("archive_command", fmt.Sprintf("barman-wal-archive --port 2222 demo.pgcellar.example %s %%p", s.Name), "", "sighup")
	} else {
		row("archive_mode", "off", "", "postmaster")
	}
	row("archive_timeout", s.ArchiveTimeout, "s", "sighup")
	row("data_checksums", "on", "", "internal")
	row("full_page_writes", "on", "", "sighup")
	row("max_replication_slots", "10", "", "postmaster")
	if s.SlotKeep != "" {
		row("max_slot_wal_keep_size", s.SlotKeep, "MB", "sighup")
	}
	row("max_wal_senders", "10", "", "postmaster")
	row("server_version_num", fmt.Sprint(s.VersionNum), "", "internal")
	if s.Major >= 17 {
		sum := "off"
		if s.Major >= 18 {
			sum = "on"
		}
		row("summarize_wal", sum, "", "sighup")
	}
	row("wal_compression", "lz4", "", "superuser")
	row("wal_level", "replica", "", "postmaster")
	return b.String()
}

// barmanConf is the section of barman.d for a server.
func (p plan) barmanConf(s server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "; /etc/barman.d/%s.conf\n[%s]\ndescription = \"%s\"\n\n", s.Name, s.Name, s.Desc)
	fmt.Fprintf(&b, "conninfo = host=%s user=barman dbname=postgres port=5432\n", s.host())
	if s.WAL != app.WALArchiveCommand {
		fmt.Fprintf(&b, "streaming_conninfo = host=%s user=streaming_barman dbname=postgres port=5432\n", s.host())
	}
	fmt.Fprintf(&b, "\nbackup_method = %s\n", s.Method)
	if s.Method == "rsync" {
		fmt.Fprintf(&b, "ssh_command = ssh postgres@%s\nreuse_backup = link\n", s.host())
	} else {
		b.WriteString("backup_compression = gzip\n")
	}
	switch s.WAL {
	case app.WALStreaming:
		b.WriteString("streaming_archiver = on\narchiver = off\n")
	case app.WALBoth:
		b.WriteString("streaming_archiver = on\narchiver = on\n")
	default:
		b.WriteString("streaming_archiver = off\narchiver = on\n")
	}
	if s.WAL != app.WALArchiveCommand {
		b.WriteString("slot_name = barman\ncreate_slot = auto\n")
	}
	fmt.Fprintf(&b, "\nretention_policy = RECOVERY WINDOW OF %d DAYS\nminimum_redundancy = %d\n", s.Ret.RecoveryWindowDays, s.Ret.MinimumRedundancy)
	return b.String()
}
