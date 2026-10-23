package demo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// Actors of the fictional team.
const (
	maria = "maria"
	tom   = "tom"
)

// history is everything the instance shows that does not come from Barman's
// files: runs, drills, restores, audit lines, the alert journal.
type history struct {
	Snapshot store.Snapshot
	// Last is the value of scheduler.last.<server> and scheduler.drill.last.<server>.
	Last map[string]app.ScheduledRun
	// Reports are the PDFs to write, by path.
	Reports map[string]report
}

type report struct {
	Server, Backup string
	Start          time.Time
	Drill          store.Drill
	Details        app.DrillDetails
}

func runID(t time.Time, idx int) int64 { return t.Unix()/60*8 + int64(idx) }

func lastWeekly(now time.Time, wd time.Weekday, h, m int, dur time.Duration) time.Time {
	d := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.Local)
	for d.Weekday() != wd || d.Add(dur).After(now.Add(-time.Minute)) {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

func dur(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d minutes, %d seconds", int(d.Minutes()), int(d.Seconds())%60)
}

func exit(n int) *int { return &n }

func tp(t time.Time) *time.Time { return &t }

func (p plan) backupLog(s server, b backup) (stdout, stderr string, code int) {
	base := fmt.Sprintf("/var/lib/barman/%s/base/%s", s.Name, b.ID)
	if b.Status == "FAILED" {
		return fmt.Sprintf("Starting backup using %s method for server %s in %s\nBackup start at LSN: %X/%X\n", s.Method, s.Name, base, b.BeginWAL/256, (b.BeginWAL%256)<<24|0x28),
			fmt.Sprintf("ERROR: the WAL streaming connection of server %s is not active (receive-wal is not running)\nERROR: Backup failed: the WAL needed to make the backup consistent never arrived\n", s.Name), 1
	}
	var o strings.Builder
	fmt.Fprintf(&o, "Starting backup using %s method for server %s in %s\n", s.Method, s.Name, base)
	fmt.Fprintf(&o, "Backup start at LSN: %X/%X (%s, 00000028)\n", b.BeginWAL/256, (b.BeginWAL%256)<<24|0x28, walName(b.BeginWAL))
	o.WriteString("Copying files.\nCopy done.\n")
	if s.DedupRatio < 1 {
		fmt.Fprintf(&o, "Backup size: %s. Actual size on disk: %s (-%.1f%% deduplication ratio).\n", humanBytes(b.Size), humanBytes(b.Disk), 100*(1-float64(b.Disk)/float64(b.Size)))
	} else {
		fmt.Fprintf(&o, "Backup size: %s\n", humanBytes(b.Size))
	}
	fmt.Fprintf(&o, "Backup end at LSN: %X/%X (%s, 00000000)\n", b.EndWAL/256, (b.EndWAL%256)<<24, walName(b.EndWAL))
	fmt.Fprintf(&o, "Backup completed (start time: %s, elapsed time: %s)\n", b.Start.Format("2006-01-02 15:04:05.000000"), dur(b.End.Sub(b.Start)))
	return o.String(), "", 0
}

func (p plan) drillDetails(s server, ok bool, at time.Time) (app.DrillDetails, time.Duration) {
	step := func(key string, ms int64, note string) app.DrillStep {
		return app.DrillStep{Key: key, Label: app.DrillStepLabel(key), Status: app.StepOK, Millis: ms, Note: note}
	}
	skip := func(key, why string) app.DrillStep {
		return app.DrillStep{Key: key, Label: app.DrillStepLabel(key), Status: app.StepSkipped, Note: why}
	}
	d := app.DrillDetails{Target: app.TargetLatest, PGMajor: fmt.Sprint(s.Major), PGVersion: "PostgreSQL " + s.PGVersion,
		SizeBytes: s.Cluster, Port: 54400, Actor: app.SchedulerActor, PGUser: "postgres", Checksums: "verified"}
	if ok {
		d.LSN = "A/3F1B2C48"
		d.Steps = []app.DrillStep{
			step(app.StepRecover, 702_000, "restored 48.0 GiB in 11 min 42 s"),
			step(app.StepVersion, 310, "PostgreSQL "+s.PGVersion),
			step(app.StepConfigure, 180, "private port 54400, archiving off, no outside access"),
			step(app.StepStart, 3_100, "server started"),
			step(app.StepRecovery, 41_000, "recovery finished at A/3F1B2C48"),
			step(app.StepChecks, 1_240, "2 databases, 359 tables"),
			step(app.StepStop, 1_800, "stopped"),
			step(app.StepChecksums, 250_000, "no bad checksum (Blocks scanned: 6291456)"),
			step(app.StepCleanup, 520, "kept the restored copy (drills.keep_last), the previous copy was removed"),
		}
		d.Databases = []app.DrillDatabase{{Name: "northwind_erp", SizeBytes: 48961429504, Tables: 318}, {Name: "northwind_reports", SizeBytes: 2040109465, Tables: 41}}
		d.DatabasesN, d.TablesN = 2, 359
		d.DurationMs = 1_000_150
		d.DataDir = "/var/lib/pgcellar/recoveries/" + s.Name + "/drill-" + at.Format("20060102T150405")
		d.Summary = app.DrillSummary(s.Name, "", at, true, "", d)
		return d, time.Duration(d.DurationMs) * time.Millisecond
	}
	d.Checksums = "not verified"
	d.SizeBytes = 3 * gib
	d.Steps = []app.DrillStep{
		step(app.StepRecover, 41_000, "restored 1.1 GiB in 41 s"),
		step(app.StepVersion, 280, "PostgreSQL "+s.PGVersion),
		step(app.StepConfigure, 150, "private port 54400, archiving off, no outside access"),
		{Key: app.StepStart, Label: app.DrillStepLabel(app.StepStart), Status: app.StepFailed, Millis: 1_900,
			Note: "pg_ctl: could not start server: FATAL:  could not locate a valid checkpoint record"},
		skip(app.StepRecovery, "PostgreSQL did not start"),
		skip(app.StepChecks, "PostgreSQL did not start"),
		skip(app.StepChecksums, "the copy never ran"),
		step(app.StepCleanup, 410, "removed the restored copy"),
	}
	d.Error = "PostgreSQL did not start: the backup needs WAL files that Barman does not hold (the WAL streaming of shop-staging stopped, so the WAL after the base backup never arrived)."
	d.LogTail = "LOG:  database system was interrupted; last known up at " + at.Add(-26*time.Hour).Format("2006-01-02 15:04:05 MST") +
		"\nLOG:  creating missing WAL directory \"pg_wal/archive_status\"\nFATAL:  could not locate a valid checkpoint record\nLOG:  startup process (PID 2214) exited with exit code 1\nLOG:  aborting startup due to startup process failure"
	d.DurationMs = 44_900
	d.DatabasesN = 0
	d.Summary = app.DrillSummary(s.Name, "", at, false, app.StepStart, d)
	return d, time.Duration(d.DurationMs) * time.Millisecond
}

func (p plan) history(all []server, now time.Time, recDir string, fps map[string]string) history {
	h := history{Last: map[string]app.ScheduledRun{}, Reports: map[string]report{}}
	idx := map[string]int{}
	byName := map[string]server{}
	cats := map[string][]backup{}
	for i, s := range all {
		idx[s.Name], byName[s.Name] = i, s
		cats[s.Name] = p.catalogue(s, now)
	}
	type ev struct {
		at                          time.Time
		actor, action, target, text string
	}
	var evs []ev
	add := func(at time.Time, actor, action, target, text string) {
		evs = append(evs, ev{at, actor, action, target, text})
	}

	// Scheduled backups of the last three days.
	for _, s := range all {
		if s.ScheduleOff {
			continue
		}
		for _, b := range cats[s.Name] {
			if now.Sub(b.Start) > 72*time.Hour {
				continue
			}
			id := runID(b.Start, idx[s.Name])
			stdout, stderr, code := p.backupLog(s, b)
			end := b.End
			if b.Status == "FAILED" {
				end = b.Start.Add(2*time.Minute + 14*time.Second)
			}
			h.Snapshot.Runs = append(h.Snapshot.Runs, store.Run{ID: id, Server: s.Name, Kind: store.KindBackup, StartedAt: b.Start,
				FinishedAt: tp(end), ExitCode: exit(code), Actor: app.SchedulerActor, StdoutTail: stdout, StderrTail: stderr})
			add(b.Start, app.SchedulerActor, "backup.start", s.Name, fmt.Sprintf("run #%d", id))
			add(b.Start.Add(time.Second), app.SchedulerActor, "barman backup "+s.Name, s.Name,
				fmt.Sprintf("exit=%d duration=%s", code, end.Sub(b.Start).Round(time.Millisecond)))
			if _, ok := h.Last[s.Name]; !ok {
				h.Last[s.Name] = app.ScheduledRun{DueAt: b.Start, RunID: id}
			}
		}
	}
	// The night analytics was not started.
	if _, skipped := p.backups(byName[Analytics], now); !skipped.IsZero() {
		h.Last[Analytics] = app.ScheduledRun{DueAt: skipped, Note: "pgCellar was being upgraded at that time"}
	}

	// A manual backup and two checks by the team.
	manual := now.Add(-52 * time.Hour).Truncate(time.Minute)
	mb := backup{occ: occ{Start: manual, End: manual.Add(4 * time.Minute), Status: "DONE"}, ID: backupID(manual), Size: sizeOf(byName[CrmProd], occ{Start: manual}, now)}
	mb.Disk, mb.BeginWAL, mb.EndWAL = diskOf(byName[CrmProd], mb.Size, false), segOf(byName[CrmProd], manual), segOf(byName[CrmProd], mb.End)
	so, se, _ := p.backupLog(byName[CrmProd], mb)
	mid := runID(manual, idx[CrmProd])
	h.Snapshot.Runs = append(h.Snapshot.Runs, store.Run{ID: mid, Server: CrmProd, Kind: store.KindBackup, StartedAt: manual, FinishedAt: tp(mb.End), ExitCode: exit(0), Actor: tom, StdoutTail: so, StderrTail: se})
	add(manual, tom, "backup.start", CrmProd, fmt.Sprintf("run #%d", mid))
	add(manual.Add(time.Second), tom, "barman backup "+CrmProd, CrmProd, "exit=0 duration=4m5.2s")
	for _, c := range []struct {
		server, actor string
		ago           time.Duration
		out           string
		code          int
	}{
		{ShopStaging, maria, 20 * time.Hour, "Server shop-staging:\n\tPostgreSQL: OK\n\treceive-wal running: FAILED (No WAL receiver process for this server)\n\tfailed backups: FAILED (there are 1 failed backups)\n", 1},
		{ErpProd, tom, 60 * time.Hour, "Server erp-prod:\n\tPostgreSQL: OK\n\tPostgreSQL streaming: OK\n\treceive-wal running: OK\n\tWAL archive: OK\n", 0},
	} {
		at := now.Add(-c.ago).Truncate(time.Minute)
		id := runID(at, idx[c.server]) + 4
		h.Snapshot.Runs = append(h.Snapshot.Runs, store.Run{ID: id, Server: c.server, Kind: store.KindCheck, StartedAt: at, FinishedAt: tp(at.Add(3 * time.Second)), ExitCode: exit(c.code), Actor: c.actor, StdoutTail: c.out})
		add(at, c.actor, "barman check "+c.server, c.server, fmt.Sprintf("exit=%d duration=3.1s", c.code))
	}

	// Restore drills: erp-prod passes, shop-staging fails.
	for _, name := range []string{ErpProd, ShopStaging} {
		s := byName[name]
		hh, mm := 4, 30
		if name == ShopStaging {
			hh, mm = 5, 30
		}
		start := lastWeekly(now, time.Sunday, hh, mm, 20*time.Minute)
		ok := name == ErpProd
		det, took := p.drillDetails(s, ok, start)
		var bid string
		for _, b := range cats[name] {
			if b.Status == "DONE" && b.End.Before(start) {
				bid = b.ID
				break
			}
		}
		if bid == "" && len(cats[name]) > 0 {
			bid = cats[name][len(cats[name])-1].ID
		}
		det.Summary = app.DrillSummary(name, bid, start, ok, map[bool]string{true: "", false: app.StepStart}[ok], det)
		fin := start.Add(took)
		id := runID(start, idx[name])
		failed := ""
		if !ok {
			failed = app.StepStart
		}
		path := filepath.Join(recDir, name, "reports", "drill-"+start.Format("20060102T150405")+".pdf")
		dr := store.Drill{ID: id, Server: name, BackupID: bid, StartedAt: start, FinishedAt: fin, OK: ok, StepFailed: failed, RunID: id, ReportPath: path}
		det.ReportWritten = true
		h.Reports[path] = report{Server: name, Backup: bid, Start: start, Drill: dr, Details: det}
		var journal strings.Builder
		for _, st := range det.Steps {
			res := "ok"
			switch st.Status {
			case app.StepFailed:
				res = "FAILED"
			case app.StepSkipped:
				res = "skipped"
			}
			fmt.Fprintf(&journal, "%s  %s\n   %s: %s\n", start.Format("15:04:05"), st.Label, res, st.Note)
		}
		fmt.Fprintf(&journal, "Report: %s\n", path)
		code := 0
		if !ok {
			code = 1
		}
		b, _ := json.Marshal(det)
		dr.Details = string(b)
		h.Snapshot.Drills = append(h.Snapshot.Drills, dr)
		h.Snapshot.Runs = append(h.Snapshot.Runs, store.Run{ID: id, Server: name, Kind: store.KindDrill, StartedAt: start, FinishedAt: tp(fin), ExitCode: exit(code), Actor: app.SchedulerActor, StdoutTail: journal.String()})
		h.Last[drillKey(name)] = app.ScheduledRun{DueAt: start, RunID: id}
		add(start, app.SchedulerActor, "drill.start", name+"/"+bid, fmt.Sprintf("run #%d target=latest", id))
		add(fin, app.SchedulerActor, "drill.end", name+"/"+bid, fmt.Sprintf("run #%d ok=%t %s", id, ok, det.Summary))
	}

	// One restore to a tar.gz by Maria.
	{
		start := now.Add(-4*24*time.Hour - 3*time.Hour).Truncate(time.Minute)
		var bid string
		for _, b := range cats[CrmProd] {
			if b.Status == "DONE" && b.End.Before(start.Add(-6*time.Hour)) {
				bid = b.ID
				break
			}
		}
		id := runID(start, idx[CrmProd]) + 5
		fin := start.Add(9*time.Minute + 36*time.Second)
		target := start.Add(-6 * time.Hour)
		dest := filepath.Join(recDir, CrmProd, bid+"-"+start.Format("20060102T150405"))
		h.Snapshot.Runs = append(h.Snapshot.Runs, store.Run{ID: id, Server: CrmProd, Kind: store.KindRecover, StartedAt: start, FinishedAt: tp(fin), ExitCode: exit(0), Actor: maria,
			StdoutTail: fmt.Sprintf("Starting local restore for server crm-prod using backup %s\nDestination directory: %s\nTarget time: %s\nYour PostgreSQL server has been successfully prepared for recovery!\nArchive: %s.tar.gz (%s)\n", bid, dest, target.Format("2006-01-02 15:04:05-07:00"), dest, humanBytes(3329*mib))})
		h.Snapshot.Recoveries = append(h.Snapshot.Recoveries, store.Recovery{ID: id, Server: CrmProd, BackupID: bid, Target: app.TargetTime, TargetTime: target.Format("2006-01-02 15:04:05-07:00"),
			Path: dest, ArchivePath: dest + ".tar.gz", SizeBytes: byName[CrmProd].Cluster, ArchiveSize: 3329 * mib, RunID: id, CreatedAt: fin, Actor: maria})
		add(start, maria, "recover.start", CrmProd+"/"+bid, fmt.Sprintf("run #%d target=time %s archive=true", id, target.Format("2006-01-02 15:04:05-07:00")))
		add(start.Add(time.Second), maria, "barman recover "+CrmProd+" "+bid, CrmProd, "exit=0 duration=9m35s")
	}

	// Things the team did earlier.
	st := p.start
	add(st.Add(-31*24*time.Hour), maria, "sshkey.create", "", "ed25519 key pair created in /var/lib/barman/.ssh")
	add(st.Add(-31*24*time.Hour).Add(5*time.Minute), maria, "sshkey.allow", ErpProd, "key "+fps[ErpProd]+" may run \"barman put-wal erp-prod\" only")
	add(st.Add(-30*24*time.Hour), maria, "sshkey.allow", Legacy9, "key "+fps[Legacy9]+" may run \"barman put-wal legacy-9\" only")
	add(st.Add(-9*24*time.Hour), tom, "server.update", Analytics, "")
	add(st.Add(-11*24*time.Hour), maria, "alerts.test", "telegram", "telegram: sent")
	add(st.Add(-11*24*time.Hour).Add(40*time.Second), maria, "alerts.test", "mail", "mail: sent")
	add(st.Add(-5*24*time.Hour), tom, "login", "", "from 10.20.0.14")
	add(st.Add(-3*24*time.Hour), maria, "login", "", "from 10.20.0.9")
	add(st.Add(-22*time.Hour), maria, "login", "", "from 10.20.0.9")
	add(st.Add(-20*time.Hour), maria, "switch-wal", ShopStaging, "exit=1")
	add(st.Add(-vaultAge), maria, "vault.settings", "", "enabled=true bucket="+vaultBucket)
	add(st.Add(-vaultAge).Add(4*time.Minute), maria, "vault.test", vaultEndpoint+"/"+vaultBucket+"/pgcellar", "ok=true")
	add(st.Add(-vaultAge).Add(11*time.Minute), maria, "vault.key.download", "", "recovery key downloaded")

	// The newest 40 lines.
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })
	if len(evs) > 40 {
		evs = evs[len(evs)-40:]
	}
	for i, e := range evs {
		h.Snapshot.Audit = append(h.Snapshot.Audit, store.AuditEntry{ID: e.at.Unix()*4 + int64(i%4), At: e.at, Actor: e.actor, Action: e.action, Target: e.target, Details: e.text})
	}

	sort.SliceStable(h.Snapshot.Runs, func(i, j int) bool { return h.Snapshot.Runs[i].StartedAt.Before(h.Snapshot.Runs[j].StartedAt) })

	// The alert journal: five sends.
	stopped := p.streamingStopped()
	var shopDrill time.Time
	for _, d := range h.Snapshot.Drills {
		if d.Server == ShopStaging {
			shopDrill = d.FinishedAt.Add(time.Minute)
		}
	}
	lastGood := time.Time{}
	if list, _ := p.backups(byName[Analytics], now); len(list) > 0 {
		lastGood = list[0].End
	}
	subject := func(kind, level, title string) string {
		return alerts.Message{Kind: kind, Level: level, Title: title}.Subject()
	}
	add2 := func(at time.Time, ch, kind, srv, level, title, errText string) {
		h.Snapshot.Alerts = append(h.Snapshot.Alerts, store.AlertSent{ID: at.Unix()*2 + int64(len(h.Snapshot.Alerts)%2), At: at, Channel: ch, Kind: kind, Server: srv, Level: level,
			Text: subject(kind, level, title), OK: errText == "", Error: errText})
	}
	add2(stopped.Add(35*time.Minute), alerts.Telegram, alerts.KindLevel, ShopStaging, alerts.LevelFail, "shop-staging is failing", "")
	add2(stopped.Add(35*time.Minute), alerts.Mail, alerts.KindLevel, ShopStaging, alerts.LevelFail, "shop-staging is failing", "")
	if !lastGood.IsZero() {
		add2(lastGood.Add(27*time.Hour+time.Minute), alerts.Telegram, alerts.KindLevel, Analytics, alerts.LevelWarn, "analytics needs attention", "")
	}
	if !shopDrill.IsZero() {
		add2(shopDrill, alerts.Telegram, alerts.KindDrill, ShopStaging, alerts.LevelFail, "Restore drill of shop-staging failed", "")
		add2(shopDrill, alerts.Mail, alerts.KindDrill, ShopStaging, alerts.LevelFail, "Restore drill of shop-staging failed", "smtp: 421 4.7.0 Try again later, closing connection")
	}
	sort.SliceStable(h.Snapshot.Alerts, func(i, j int) bool { return h.Snapshot.Alerts[i].At.Before(h.Snapshot.Alerts[j].At) })
	for i := range h.Snapshot.Alerts {
		h.Snapshot.Alerts[i].ID = int64(i + 1)
	}
	h.Snapshot.VaultCopies = p.vaultCopies(all, cats, now)
	h.renumber()
	return h
}

var runRef = regexp.MustCompile(`run #(\d+)`)

// renumber gives the rows the small, ordered ids a real instance has (1 is the
// oldest), whatever the provisional ids built above, and follows every
// reference to a run.
func (h *history) renumber() {
	sn := &h.Snapshot
	sort.SliceStable(sn.Runs, func(i, j int) bool { return sn.Runs[i].StartedAt.Before(sn.Runs[j].StartedAt) })
	ids := map[int64]int64{}
	for i := range sn.Runs {
		ids[sn.Runs[i].ID] = int64(i + 1)
		sn.Runs[i].ID = int64(i + 1)
	}
	sort.SliceStable(sn.Drills, func(i, j int) bool { return sn.Drills[i].StartedAt.Before(sn.Drills[j].StartedAt) })
	for i := range sn.Drills {
		sn.Drills[i].RunID = ids[sn.Drills[i].RunID]
		sn.Drills[i].ID = int64(i + 1)
		r := h.Reports[sn.Drills[i].ReportPath]
		r.Drill.RunID, r.Drill.ID = sn.Drills[i].RunID, sn.Drills[i].ID
		h.Reports[sn.Drills[i].ReportPath] = r
	}
	for i := range sn.Recoveries {
		sn.Recoveries[i].RunID = ids[sn.Recoveries[i].RunID]
		sn.Recoveries[i].ID = int64(i + 1)
	}
	for k, rec := range h.Last {
		if rec.RunID != 0 {
			rec.RunID = ids[rec.RunID]
			h.Last[k] = rec
		}
	}
	for i := range sn.Audit {
		sn.Audit[i].ID = int64(i + 1)
		sn.Audit[i].Details = runRef.ReplaceAllStringFunc(sn.Audit[i].Details, func(m string) string {
			old, _ := strconv.ParseInt(m[len("run #"):], 10, 64)
			return fmt.Sprintf("run #%d", ids[old])
		})
	}
}

func drillKey(server string) string { return "drill:" + server }

// writeReports writes the PDF of each fictional drill.
func (h history) writeReports() error {
	for path, r := range h.Reports {
		pdf, _, err := app.DrillReportPDF(r.Server, r.Backup, r.Start, r.Drill, r.Details)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, pdf, 0o640); err != nil {
			return err
		}
	}
	return nil
}
