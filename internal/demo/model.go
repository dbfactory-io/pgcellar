package demo

import (
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

const (
	kib = int64(1) << 10
	mib = kib << 10
	gib = mib << 10
	tib = gib << 10
)

// Company is the fictional customer of the demo.
const Company = "Northwind Logistics"

// Names of the five fictional servers.
const (
	ErpProd     = "erp-prod"
	CrmProd     = "crm-prod"
	Analytics   = "analytics"
	ShopStaging = "shop-staging"
	Legacy9     = "legacy-9"
)

// server describes one PostgreSQL server of Northwind Logistics and the way
// Barman backs it up.
type server struct {
	Name, Label, Desc string
	Major             int
	VersionNum        int
	PGVersion         string
	Cluster           int64 // size of the data directory
	BackupSize        int64 // size of one backup (what Barman calls size)
	DedupRatio        float64
	Method            string // postgres | rsync
	WAL               string // app.WALStreaming | app.WALArchiveCommand | app.WALBoth
	Dur               time.Duration
	SegEvery          time.Duration // one WAL file is closed this often
	SegSize           int64         // size of an archived WAL file (compressed)
	Ret               config.Retention
	Schedule          string // "daily HH:MM", filled at start
	ScheduleOff       bool
	DrillSchedule     string
	Tags              []string
	SystemID          string
	// Setup answers: pg_settings rows of the fake psql.
	ArchiveTimeout string
	SlotKeep       string
}

func (s server) host() string { return s.Name + ".db.northwind.example" }

func servers() []server {
	return []server{
		{
			Name: ErpProd, Label: "ERP (orders, invoices, stock)", Desc: "Northwind ERP, primary",
			Major: 16, VersionNum: 160006, PGVersion: "16.6", Cluster: 48 * gib, BackupSize: 48 * gib, DedupRatio: 0.07,
			Method: "rsync", WAL: app.WALBoth, Dur: 11 * time.Minute, SegEvery: 10 * time.Minute, SegSize: 7 * mib,
			Ret:           config.Retention{RecoveryWindowDays: 14, MinimumRedundancy: 3},
			DrillSchedule: "weekly sun 04:30", Tags: []string{"production", "erp"}, SystemID: "7421903311840203011",
			ArchiveTimeout: "300", SlotKeep: "20480",
		},
		{
			Name: CrmProd, Label: "CRM", Desc: "Northwind CRM, primary",
			Major: 17, VersionNum: 170002, PGVersion: "17.2", Cluster: 12 * gib, BackupSize: 12 * gib, DedupRatio: 0.09,
			Method: "rsync", WAL: app.WALStreaming, Dur: 4 * time.Minute, SegEvery: 15 * time.Minute, SegSize: 4 * mib,
			Ret:  config.Retention{RecoveryWindowDays: 14, MinimumRedundancy: 3},
			Tags: []string{"production", "crm"}, SystemID: "7433120987114477201",
			ArchiveTimeout: "600", SlotKeep: "-1",
		},
		{
			Name: Analytics, Label: "Analytics warehouse", Desc: "Northwind analytics warehouse",
			Major: 18, VersionNum: 180001, PGVersion: "18.1", Cluster: 210 * gib, BackupSize: 210 * gib, DedupRatio: 0.045,
			Method: "rsync", WAL: app.WALStreaming, Dur: 52 * time.Minute, SegEvery: 10 * time.Minute, SegSize: 9 * mib,
			Ret:  config.Retention{RecoveryWindowDays: 14, MinimumRedundancy: 3},
			Tags: []string{"production", "bi"}, SystemID: "7451900442118830977",
			ArchiveTimeout: "600", SlotKeep: "102400",
		},
		{
			Name: ShopStaging, Label: "Web shop (staging)", Desc: "Northwind web shop, staging copy",
			Major: 16, VersionNum: 160006, PGVersion: "16.6", Cluster: 3 * gib, BackupSize: 1126 * mib, DedupRatio: 1,
			Method: "postgres", WAL: app.WALStreaming, Dur: 3 * time.Minute, SegEvery: 6 * time.Hour, SegSize: 2 * mib,
			Ret:           config.Retention{RecoveryWindowDays: 7, MinimumRedundancy: 2},
			DrillSchedule: "weekly sun 05:30", Tags: []string{"staging"}, SystemID: "7421903311840201188",
			ArchiveTimeout: "600", SlotKeep: "4096",
		},
		{
			Name: Legacy9, Label: "Legacy reporting (to retire)", Desc: "Old reporting server, PostgreSQL 15",
			Major: 15, VersionNum: 150010, PGVersion: "15.10", Cluster: 6 * gib, BackupSize: 6 * gib, DedupRatio: 1,
			Method: "rsync", WAL: app.WALArchiveCommand, Dur: 7 * time.Minute, SegEvery: 2 * time.Hour, SegSize: 3 * mib,
			Ret:         config.Retention{RecoveryWindowDays: 7, MinimumRedundancy: 1},
			ScheduleOff: true, Tags: []string{"legacy"}, SystemID: "7312209981223340551",
			ArchiveTimeout: "0", SlotKeep: "-1",
		},
	}
}

// Fixed times of the story, as ages at start. They do not slide: a broken
// staging server stays broken as the instance gets older.
const (
	streamingStoppedAge = 50 * time.Hour
	legacyBackupAge     = 40 * 24 * time.Hour
	analyticsLastAge    = 31 * time.Hour
	lastBackupAge       = 2 * time.Hour
)

// minAnalyticsAge is how old the last good backup of analytics is at least:
// the nights after it were missed, whatever the time of the day.
const minAnalyticsAge = 27 * time.Hour

// plan is the answer to "when did the backups run": the clock of the demo is
// the real one, the daily times of day are chosen at start from it so that the
// ages the visitor sees at start are always the same ones.
type plan struct {
	start time.Time      // when this instance started
	tod   map[string]int // minute of the day at which each daily backup starts
}

func floor5(t time.Time) time.Time { return t.Truncate(5 * time.Minute) }

func minuteOfDay(t time.Time) int { return t.Hour()*60 + t.Minute() }

// newPlan chooses the daily times at the moment of start: erp-prod finished
// about two hours ago, analytics' last good backup is 31 h old, and so on.
func newPlan(now time.Time) plan {
	erp := floor5(now.Add(-lastBackupAge - 11*time.Minute))
	return plan{start: now, tod: map[string]int{
		ErpProd:     minuteOfDay(erp),
		CrmProd:     minuteOfDay(erp.Add(35 * time.Minute)),
		Analytics:   minuteOfDay(floor5(now.Add(-analyticsLastAge - 52*time.Minute))),
		ShopStaging: minuteOfDay(floor5(now.Add(-5*time.Hour - 3*time.Minute))),
		Legacy9:     minuteOfDay(floor5(now.Add(-legacyBackupAge - 7*time.Minute))),
	}}
}

// streamingStopped is when the WAL receiver of shop-staging died.
func (p plan) streamingStopped() time.Time {
	return p.start.Add(-streamingStoppedAge).Truncate(time.Minute)
}

func (p plan) scheduleText(name string) string {
	m := p.tod[name]
	return fmt.Sprintf("daily %02d:%02d", m/60, m%60)
}

// occ is one due time of a daily schedule.
type occ struct {
	Start, End time.Time
	Status     string // DONE | FAILED | SKIPPED (nothing was started)
}

// occurrences lists the daily starts at minute-of-day tod, newest first, that
// have ended before now.
func occurrences(now time.Time, tod int, dur time.Duration, n int) []occ {
	d := time.Date(now.Year(), now.Month(), now.Day(), tod/60, tod%60, 0, 0, time.Local)
	for d.Add(dur).After(now.Add(-30 * time.Second)) {
		d = d.AddDate(0, 0, -1)
	}
	out := make([]occ, 0, n)
	for i := 0; i < n; i++ {
		s := d.AddDate(0, 0, -i)
		out = append(out, occ{Start: s, End: s.Add(dur), Status: "DONE"})
	}
	return out
}

// backups returns the backups of a server that Barman lists, newest first, and
// the due time the scheduler did not start (zero when there is none).
func (p plan) backups(s server, now time.Time) (list []occ, skipped time.Time) {
	switch s.Name {
	case Legacy9:
		b := p.start.Add(-legacyBackupAge).Truncate(time.Minute)
		return []occ{{Start: b, End: b.Add(s.Dur), Status: "DONE"}}, time.Time{}
	case Analytics:
		all := occurrences(now, p.tod[s.Name], s.Dur, 20)
		j := 0
		for j < len(all) && now.Sub(all[j].End) < minAnalyticsAge {
			j++
		}
		if j > 0 {
			skipped = all[0].Start
		}
		return all[j : j+14], skipped
	case ShopStaging:
		all := occurrences(now, p.tod[s.Name], s.Dur, 14)
		all[0].Status = "FAILED"
		return all, time.Time{}
	}
	return occurrences(now, p.tod[s.Name], s.Dur, 14), time.Time{}
}

// ---- identifiers and numbers derived from times ----

func backupID(t time.Time) string { return t.Format("20060102T150405") }

func frac(key string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%10000) / 10000
}

// sizeOf is the size of a backup: the nominal size, a little older = smaller,
// a little noise that does not change from one rebuild to the next.
func sizeOf(s server, b occ, now time.Time) int64 {
	days := now.Sub(b.Start).Hours() / 24
	k := 1 - 0.003*days + 0.04*(frac(s.Name+backupID(b.Start))-0.5)
	return int64(float64(s.BackupSize) * k)
}

func diskOf(s server, size int64, oldest bool) int64 {
	if s.DedupRatio >= 1 || oldest {
		return size
	}
	return int64(math.Round(float64(size) * s.DedupRatio))
}

// ---- the WAL catalogue ----

var walEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const walBase = int64(0x0000240)

// segOf is the number of the WAL segment that is being written at t.
func segOf(s server, t time.Time) int64 {
	return walBase + int64(t.Sub(walEpoch)/s.SegEvery)
}

func segStart(s server, n int64) time.Time {
	return walEpoch.Add(time.Duration(n-walBase) * s.SegEvery)
}

func walName(n int64) string {
	return fmt.Sprintf("%08X%08X%08X", 1, uint32(n/256), uint32(n%256))
}

// segmentSize varies a little around the nominal archived size.
func segmentSize(s server, n int64) int64 {
	return int64(float64(s.SegSize) * (0.55 + 0.9*frac(fmt.Sprint(s.Name, n))))
}
