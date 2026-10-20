package app

import (
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
)

func TestStatFSOnTempDir(t *testing.T) {
	r, err := statFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.free <= 0 || r.total < r.free || r.used < 0 {
		t.Fatalf("statfs: %+v", r)
	}
	u := usageOf("/x", r)
	if u.UsedPercent < 0 || u.UsedPercent > 100 {
		t.Errorf("used percent %v", u.UsedPercent)
	}
}

func TestFSLevel(t *testing.T) {
	const gib = int64(1) << 30
	fs := func(freeGiB, totalGiB int64) FSUsage {
		return *usageOf("/var/lib/barman", fsRaw{total: totalGiB * gib, free: freeGiB * gib, used: (totalGiB - freeGiB) * gib})
	}
	for _, c := range []struct {
		name  string
		f     FSUsage
		pct   float64
		bytes int64
		want  string
	}{
		{"plenty", fs(50, 100), 10, 0, LevelOK},
		{"just above", fs(11, 100), 10, 0, LevelOK},
		{"just below", fs(9, 100), 10, 0, LevelWarn},
		{"exactly half", fs(5, 100), 10, 0, LevelWarn},
		{"below half", fs(4, 100), 10, 0, LevelFail},
		{"percent off", fs(1, 100), 0, 0, LevelOK},
		{"bytes warn", fs(40, 100), 0, 50 * gib, LevelWarn},
		{"bytes fail", fs(20, 100), 0, 50 * gib, LevelFail},
		{"bytes ok", fs(60, 100), 10, 50 * gib, LevelOK},
		{"worst of both", fs(30, 100), 10, 50 * gib, LevelWarn},
		{"worst of both fail", fs(4, 100), 10, 5 * gib, LevelFail},
	} {
		if got := fsLevel(c.f, c.pct, c.bytes); got != c.want {
			t.Errorf("%s: level %s, want %s", c.name, got, c.want)
		}
	}
}

func TestGrowthAndProjection(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	wal := func(ago time.Duration, size int64) barman.WALFile {
		return barman.WALFile{Size: size, Time: now.Add(-ago)}
	}
	bk := func(ago time.Duration, size, dedup int64, status string) barman.BackupInfo {
		return barman.BackupInfo{Status: status, EndTime: now.Add(-ago), BeginTime: now.Add(-ago - time.Minute), Size: size, DeduplicatedSize: dedup}
	}

	// Ten days of history: only the last seven count, divided by seven.
	wals := []barman.WALFile{wal(10*day, 1000), wal(8*day, 1000), wal(6*day, 700), wal(1*day, 700)}
	infos := []barman.BackupInfo{bk(9*day, 5000, 0, "DONE"), bk(3*day, 7000, 3500, "DONE"), bk(2*day, 9000, 0, "FAILED")}
	// 700 + 700 (wal) + 3500 (backup, on disk) = 4900 over 7 days.
	if got := growthPerDay(now, infos, wals); got != 700 {
		t.Errorf("growth = %d, want 700", got)
	}

	// Young server: three days of data, divided by three, not seven.
	young := []barman.WALFile{wal(3*day, 300), wal(1*day, 300)}
	if got := growthPerDay(now, nil, young); got != 200 {
		t.Errorf("young growth = %d, want 200", got)
	}
	// Less than a day of data counts as one day.
	if got := growthPerDay(now, nil, []barman.WALFile{wal(time.Hour, 500)}); got != 500 {
		t.Errorf("one-day floor = %d, want 500", got)
	}
	if got := growthPerDay(now, nil, nil); got != 0 {
		t.Errorf("no data = %d", got)
	}

	for _, c := range []struct {
		free, per int64
		days      int
		ok        bool
	}{
		{1000, 100, 10, true},
		{8900, 100, 89, true},
		{9000, 100, 90, false},
		{1000, 0, 0, false},
	} {
		d, ok := daysUntilFull(c.free, c.per)
		if ok != c.ok || (ok && d != c.days) {
			t.Errorf("daysUntilFull(%d, %d) = %d, %v; want %d, %v", c.free, c.per, d, ok, c.days, c.ok)
		}
	}
}

func TestBarSegments(t *testing.T) {
	f := usageOf("/b", fsRaw{total: 1000, free: 400, used: 600})
	b := bar(f, 100)
	if b.Other != 50 || b.Mine != 10 {
		t.Errorf("bar = %+v", b)
	}
	if b := bar(f, 1); b.Mine != 0.5 {
		t.Errorf("tiny share must stay visible: %+v", b)
	}
	if b := bar(nil, 5); b != (StorageBar{}) {
		t.Errorf("no fs: %+v", b)
	}
}

func TestFormatSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 500: "500 B", 115712: "113.0 KiB", 13212057: "12.6 MiB"} {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
	if !strings.Contains(FormatSize(5<<40), "TiB") {
		t.Error("TiB")
	}
}
