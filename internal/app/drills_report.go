package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/report"
	"github.com/stranix79/pgcellar/internal/store"
)

// elephant is the small drawing in the header of the report.
var elephant = []string{
	"      __        __",
	"   .-'  `-.__.-'  `-.",
	"  /  o       __      \\",
	" |  ,--.    /  \\      |___",
	"  \\ |  |   (    )     ,---'",
	"   `|  |____\\__/____ /",
	"    |  ||  ||   ||  |",
	"    '--'`--'`---'`--'",
}

func fmtMillis(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		s := int(d.Round(time.Second).Seconds())
		return fmt.Sprintf("%d min %02d s", s/60, s%60)
	}
	return fmt.Sprintf("%d h %02d min", int(d.Hours()), int(d.Minutes())%60)
}

func targetText(d DrillDetails) string {
	switch d.Target {
	case TargetBackupEnd:
		return "end of the backup (as soon as the copy is consistent)"
	case TargetTime:
		return "point in time " + d.TargetTime
	}
	return "latest (all the WAL archived by Barman)"
}

// drillSummary is the sentence in plain language at the top of the report.
func drillSummary(server, backup string, at time.Time, ok bool, failedKey string, d DrillDetails) string {
	when := at.Format("2 Jan 2006") + " at " + at.Format("15:04")
	if ok {
		s := fmt.Sprintf("On %s the backup %s of %s was restored and started successfully in %s; %s, %s.",
			when, backup, server, fmtMillis(d.DurationMs), plural(d.DatabasesN, "database"), plural(d.TablesN, "table"))
		if d.Checksums == "verified" {
			s += " Data checksums were verified."
		}
		return s
	}
	what := DrillStepLabel(failedKey)
	if failedKey == "" {
		what = "an unknown step"
	}
	s := fmt.Sprintf("On %s the restore drill of backup %s of %s FAILED at the step \"%s\" after %s.", when, backup, server, what, fmtMillis(d.DurationMs))
	if d.Error != "" {
		s += " " + d.Error
	}
	return s
}

// record builds the verdict, writes the report and stores the drill. It
// returns the stored drill (ok false when the drill failed).
func (r *drillRun) record(runID int64) store.Drill {
	a := r.a
	ok := r.failedKey == ""
	d := &r.det
	d.Summary = drillSummary(r.server, r.in.BackupID, r.startAt, ok, r.failedKey, *d)
	rec := store.Drill{Server: r.server, BackupID: r.in.BackupID, StartedAt: r.startAt, OK: ok,
		StepFailed: r.failedKey, RunID: runID}
	rec.FinishedAt = r.startAt.Add(time.Duration(d.DurationMs) * time.Millisecond)

	path, fp, err := a.writeDrillReport(r, rec)
	if err != nil {
		a.Logf("cannot write the report of the drill of %s: %v", r.server, err)
		r.logf("The report could not be written: %v", err)
	} else {
		rec.ReportPath, d.Fingerprint, d.ReportWritten = path, fp, true
		r.logf("Report: %s", path)
	}
	b, _ := json.Marshal(d)
	rec.Details = string(b)
	id, err := a.Store.AddDrill(rec)
	if err != nil {
		a.Logf("cannot record the drill of %s: %v", r.server, err)
	}
	rec.ID = id
	return rec
}

// reportDir is where the PDFs of a server go.
func reportDir(root, server string) string { return filepath.Join(root, server, "reports") }

func (a *App) writeDrillReport(r *drillRun, rec store.Drill) (string, string, error) {
	if !barman.ValidName(r.server) {
		return "", "", errors.New("invalid server name")
	}
	d := r.det
	doc := report.New("Restore drill report - " + r.server)
	doc.FooterL = fmt.Sprintf("pgCellar restore drill report - %s - %s", r.server, r.startAt.Format("2006-01-02 15:04"))
	doc.Header("pgCellar", "Restore drill report", elephant)
	if rec.OK {
		doc.Banner(true, "RESTORE VERIFIED")
	} else {
		doc.Banner(false, "FAILED")
	}
	doc.Paragraph(d.Summary, 11)
	doc.Space(6)
	doc.Heading("The drill")
	rows := [][2]string{
		{"Server", r.server},
		{"Source backup", r.in.BackupID},
		{"Recovery target", targetText(d)},
		{"Date", r.startAt.Format("Monday 2 January 2006, 15:04 MST")},
		{"Duration", fmtMillis(d.DurationMs)},
		{"PostgreSQL version", firstNonEmpty(d.PGVersion, d.PGMajor, "-")},
		{"WAL position reached", firstNonEmpty(d.LSN, "-")},
		{"Restored size", HumanBytes(d.SizeBytes)},
		{"Data checksums", firstNonEmpty(d.Checksums, "-")},
		{"Started by", firstNonEmpty(d.Actor, "-")},
		{"Run", fmt.Sprintf("#%d", rec.RunID)},
	}
	doc.KeyValues(rows)
	doc.Heading("Steps")
	var srows [][]string
	for _, s := range d.Steps {
		res := "+ok"
		switch s.Status {
		case StepFailed:
			res = "!FAILED"
		case StepSkipped:
			res = "skipped"
		}
		t := "-"
		if s.Status != StepSkipped {
			t = fmtMillis(s.Millis)
		}
		srows = append(srows, []string{s.Label, res, t, s.Note})
	}
	doc.Table([]report.Column{{Title: "Step", Share: 4}, {Title: "Result", Share: 1.3}, {Title: "Time", Share: 1.2, Right: true}, {Title: "Note", Share: 5}}, srows)
	if len(d.Databases) > 0 {
		doc.Heading("Databases in the restored copy")
		cols := []report.Column{{Title: "Database", Share: 4}, {Title: "Size", Share: 2, Right: true}, {Title: "Tables", Share: 1.5, Right: true}}
		if d.VerifyTables {
			cols = append(cols, report.Column{Title: "Rows counted", Share: 2, Right: true})
		}
		var drows [][]string
		for _, db := range d.Databases {
			row := []string{db.Name, HumanBytes(db.SizeBytes), fmt.Sprint(db.Tables)}
			if d.VerifyTables {
				rc := "-"
				if db.Rows != nil {
					rc = fmt.Sprint(*db.Rows)
				}
				row = append(row, rc)
			}
			drows = append(drows, row)
		}
		doc.Table(cols, drows)
	}
	if !rec.OK {
		doc.Heading("What went wrong")
		doc.Paragraph(firstNonEmpty(d.Error, "The drill stopped before it could say why."), 10)
		if d.LogTail != "" {
			doc.Space(4)
			doc.Paragraph("End of the PostgreSQL log of the copy:", 9)
			doc.Mono(strings.Split(d.LogTail, "\n"))
		}
	}
	doc.Space(8)
	doc.Paragraph("This report records one restore of a real backup into a throwaway PostgreSQL server of the same major version. The copy was started on a private port, reachable from this host only, and removed or kept as the configuration says. It shows that the backup can be restored and that the result starts; it does not compare the data with the source.", 8)
	pdf, fp := doc.Bytes()
	if _, err := report.Check(pdf); err != nil {
		return "", "", fmt.Errorf("the generated PDF is damaged: %w", err)
	}

	dir := reportDir(r.cfg.Recoveries.Dir, r.server)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", err
	}
	chownBarman(r.cfg, dir)
	name := "drill-" + time.Now().Format("20060102T150405") + ".pdf"
	if r.dest != "" {
		name = filepath.Base(r.dest) + ".pdf"
	}
	path := filepath.Join(dir, name)
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, pdf, 0o640); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	return path, fp, nil
}

// dropOlderDrillCopies removes the restored copies kept by the earlier drills
// of a server (drills.keep_last keeps the newest only).
func (a *App) dropOlderDrillCopies(server, keep string) {
	list, err := a.Store.ListDrills(server, 500)
	if err != nil {
		return
	}
	root := a.Config().Recoveries.Dir
	for _, d := range list {
		var det DrillDetails
		if json.Unmarshal([]byte(d.Details), &det) != nil || det.DataDir == "" || det.DataDir == keep {
			continue
		}
		if err := removeTree(root, det.DataDir); err != nil {
			a.Logf("cannot remove the copy of drill #%d: %v", d.ID, err)
			continue
		}
		det.DataDir = ""
		b, _ := json.Marshal(det)
		_ = a.Store.UpdateDrillDetails(d.ID, string(b), d.ReportPath)
	}
}

// purgeOrphanDrillCopies removes drill-* directories no drill record points to
// and that are older than a day (a drill interrupted by a restart leaves one).
func (a *App) purgeOrphanDrillCopies() {
	root := a.Config().Recoveries.Dir
	list, err := a.Store.ListDrills("", 5000)
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, d := range list {
		var det DrillDetails
		if json.Unmarshal([]byte(d.Details), &det) == nil && det.DataDir != "" {
			kept[det.DataDir] = true
		}
	}
	a.runMu.Lock()
	busy := false
	for k := range a.running {
		if strings.HasPrefix(k, drillRunKey) {
			busy = true
		}
	}
	a.runMu.Unlock()
	if busy {
		return
	}
	servers, _ := filepath.Glob(filepath.Join(root, "*"))
	for _, sdir := range servers {
		dirs, _ := filepath.Glob(filepath.Join(sdir, "drill-*"))
		for _, d := range dirs {
			fi, err := os.Lstat(d)
			if err != nil || !fi.IsDir() || kept[d] || time.Since(fi.ModTime()) < 24*time.Hour {
				continue
			}
			if err := removeTree(root, d); err == nil {
				a.Logf("drills: removed the abandoned copy %s", d)
			}
		}
	}
}

// ---- read ----

// ListDrills lists the drills of a server (all when empty), newest first.
func (a *App) ListDrills(server string, limit int) ([]DrillView, error) {
	list, err := a.Store.ListDrills(server, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DrillView, 0, len(list))
	for _, d := range list {
		out = append(out, drillView(d))
	}
	return out, nil
}

// GetDrill returns one drill, or ErrNotFound.
func (a *App) GetDrill(id int64) (*DrillView, error) {
	d, err := a.Store.GetDrill(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := drillView(*d)
	return &v, nil
}

// DrillOfRun returns the drill a run produced, if it produced one.
func (a *App) DrillOfRun(runID int64) (*DrillView, bool) {
	d, err := a.Store.DrillForRun(runID)
	if err != nil {
		return nil, false
	}
	v := drillView(*d)
	return &v, true
}

// DrillReport returns the path of the PDF of a drill, for download. The
// download is audited.
func (a *App) DrillReport(actor string, id int64) (string, *DrillView, error) {
	d, err := a.Store.GetDrill(id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	v := drillView(*d)
	if d.ReportPath == "" || !barman.ValidName(d.Server) {
		return "", &v, ErrNotFound
	}
	want := reportDir(a.Config().Recoveries.Dir, d.Server)
	if filepath.Dir(filepath.Clean(d.ReportPath)) != filepath.Clean(want) {
		return "", &v, fmt.Errorf("refusing to serve %s: it is not inside %s", d.ReportPath, want)
	}
	fi, err := os.Lstat(d.ReportPath)
	if err != nil || !fi.Mode().IsRegular() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return "", &v, ErrNotFound
		}
		return "", &v, err
	}
	_ = a.Store.AddAudit(actor, "drill.report", d.Server+"/"+filepath.Base(d.ReportPath), fmt.Sprintf("drill #%d", id))
	return d.ReportPath, &v, nil
}
