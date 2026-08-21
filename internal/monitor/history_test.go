package monitor

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"redismonitor/internal/config"
)

func testHistoryStore(t *testing.T, mutate func(*config.Config)) *historyStore {
	t.Helper()

	t.Setenv("REDIS_MONITOR_DEV", "true")
	t.Setenv("REDIS_MONITOR_DATA_DIR", t.TempDir())

	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	if mutate != nil {
		mutate(cfg)
	}

	store, err := newHistoryStore(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHistoryStore: %v", err)
	}

	return store
}

func at(spec string) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", spec, time.Local)
	if err != nil {
		panic(err)
	}

	return parsed
}

func point(keys int64, when time.Time) historyPoint {
	return historyPoint{Keys: keys, Memory: keys * 100, Ops: 5, Clients: 1, RecordAt: when}
}

func TestHistoryRecordsAndRendersASlot(t *testing.T) {
	store := testHistoryStore(t, nil)
	now := at("2026-08-22 01:30:00")

	store.record(0, point(35, now))

	series := store.series(0, 3, now)

	if len(series.ByDay) != 3 {
		t.Fatalf("by_day = %d rows, want 3", len(series.ByDay))
	}

	today := series.ByDay[len(series.ByDay)-1]

	if today.Label != "Today" || !today.Recorded {
		t.Fatalf("today = %+v, want a recorded row labelled Today", today)
	}

	if today.Keys == nil || *today.Keys != 35 {
		t.Errorf("today.keys = %v, want 35", today.Keys)
	}

	// The hour axis must line up with the wall clock, not with a UTC-truncated
	// hour: in a zone offset by a fraction of an hour the two differ, and the
	// recorded point would land in a slot the renderer never draws.
	current := series.ByHour[len(series.ByHour)-1]

	if current.Slot != "2026-08-22 01" {
		t.Errorf("last hour slot = %q, want 2026-08-22 01", current.Slot)
	}

	if !current.Recorded {
		t.Error("the hour the point was recorded in is not marked recorded")
	}

	if series.ByDay[0].Recorded {
		t.Error("a day nobody looked at must not be marked recorded")
	}

	if series.ByDay[0].Keys != nil {
		t.Error("an unrecorded slot must report null keys, never zero")
	}
}

func TestHistoryRespectsTheMinimumInterval(t *testing.T) {
	store := testHistoryStore(t, nil)
	now := at("2026-08-22 01:30:00")

	store.record(0, point(35, now))
	// A page on auto-refresh would otherwise rewrite the series every 30 seconds.
	store.record(0, point(99, now.Add(30*time.Second)))

	current := store.series(0, 1, now).ByHour[47]

	if current.Samples != 1 {
		t.Errorf("samples = %d, want 1 — the second point was inside the interval", current.Samples)
	}

	if current.Keys == nil || *current.Keys != 35 {
		t.Errorf("keys = %v, want the first reading to stand", current.Keys)
	}
}

func TestHistoryKeepsMinAndMaxInsideASlot(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) { cfg.HistoryMinInterval = 0 })
	now := at("2026-08-22 01:00:00")

	store.record(0, point(35, now))
	store.record(0, point(12, now.Add(5*time.Minute)))
	store.record(0, point(80, now.Add(10*time.Minute)))

	current := store.series(0, 1, now.Add(10*time.Minute)).ByHour[47]

	if current.Samples != 3 {
		t.Fatalf("samples = %d, want 3", current.Samples)
	}

	// The slot keeps the LAST reading plus the range seen, so "keys per hour" stays
	// a real reading taken at a known time.
	if *current.Keys != 80 || *current.KeysMin != 12 || *current.KeysMax != 80 {
		t.Errorf("keys/min/max = %d/%d/%d, want 80/12/80", *current.Keys, *current.KeysMin, *current.KeysMax)
	}
}

func TestHistoryCounterDeltas(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) { cfg.HistoryMinInterval = 0 })

	yesterday := at("2026-08-21 10:00:00")
	today := at("2026-08-22 10:00:00")

	first := point(10, yesterday)
	first.Expired = 100
	first.Evicted = 5
	store.record(0, first)

	second := point(10, today)
	second.Expired = 140
	second.Evicted = 5
	store.record(0, second)

	rows := store.series(0, 2, today).ByDay

	if rows[0].Expired != nil {
		t.Error("the first recorded slot has no earlier point, so its delta must be null")
	}

	if rows[1].Expired == nil || *rows[1].Expired != 40 {
		t.Errorf("expired delta = %v, want 40", rows[1].Expired)
	}

	if rows[1].Evicted == nil || *rows[1].Evicted != 0 {
		t.Errorf("evicted delta = %v, want 0", rows[1].Evicted)
	}
}

func TestHistoryCounterResetReportsNullNotANegativeNumber(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) { cfg.HistoryMinInterval = 0 })

	yesterday := at("2026-08-21 10:00:00")
	today := at("2026-08-22 10:00:00")

	first := point(10, yesterday)
	first.Expired = 5000
	store.record(0, first)

	// INFO counters reset on restart, so the later reading can be lower.
	second := point(10, today)
	second.Expired = 12
	store.record(0, second)

	rows := store.series(0, 2, today).ByDay

	if rows[1].Expired != nil {
		t.Errorf("expired delta = %v, want null after a counter reset", *rows[1].Expired)
	}
}

func TestHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("REDIS_MONITOR_DEV", "true")
	t.Setenv("REDIS_MONITOR_DATA_DIR", dir)

	cfg, err := config.Load(filepath.Join(dir, "absent.env"))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := at("2026-08-22 01:30:00")

	first, err := newHistoryStore(cfg, log)
	if err != nil {
		t.Fatalf("newHistoryStore: %v", err)
	}

	first.record(0, point(35, now))

	// A second store over the same directory is what a restart looks like. Keeping
	// the series in memory instead would silently lose it on every deploy.
	second, err := newHistoryStore(cfg, log)
	if err != nil {
		t.Fatalf("newHistoryStore: %v", err)
	}

	today := second.series(0, 1, now).ByDay[0]

	if !today.Recorded || today.Keys == nil || *today.Keys != 35 {
		t.Errorf("today after restart = %+v, want the recorded reading of 35", today)
	}
}

func TestHistoryTrimsToTheRetentionWindow(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) {
		cfg.HistoryMinInterval = 0
		cfg.HistoryDays = 3
		cfg.HistoryHours = 2
	})

	base := at("2026-08-01 00:00:00")

	for day := 0; day < 10; day++ {
		store.record(0, point(int64(day), base.AddDate(0, 0, day)))
	}

	store.mu.Lock()
	file := store.load(0)
	days, hours := len(file.Daily), len(file.Hourly)
	store.mu.Unlock()

	if days != 3 {
		t.Errorf("kept %d day buckets, want 3", days)
	}

	if hours != 2 {
		t.Errorf("kept %d hour buckets, want 2", hours)
	}
}

func TestHistoryDisabledRecordsNothing(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) { cfg.HistoryEnabled = false })
	now := at("2026-08-22 01:30:00")

	store.record(0, point(35, now))

	series := store.series(0, 1, now)

	if series.Enabled {
		t.Error("series reports enabled while history is off")
	}

	if len(series.ByDay) != 0 || len(series.ByHour) != 0 {
		t.Error("a disabled history must return no rows at all")
	}
}

func TestHistoryClampsTheRequestedWindow(t *testing.T) {
	store := testHistoryStore(t, func(cfg *config.Config) { cfg.HistoryDays = 7 })
	now := at("2026-08-22 01:30:00")

	if got := store.series(0, 0, now).Days; got != 7 {
		t.Errorf("days=0 gave %d, want the configured 7", got)
	}

	if got := store.series(0, 99, now).Days; got != 7 {
		t.Errorf("days=99 gave %d, want it clamped to 7", got)
	}

	if got := store.series(0, 3, now).Days; got != 3 {
		t.Errorf("days=3 gave %d, want 3", got)
	}
}

func TestStartOfHourUsesWallClockNotUTC(t *testing.T) {
	// The bug this pins: time.Truncate rounds down against the zero time in UTC,
	// so in a zone offset by 45 minutes it lands inside the previous hour.
	zone := time.FixedZone("UTC+5:45", 5*3600+45*60)
	moment := time.Date(2026, 8, 22, 1, 11, 0, 0, zone)

	if got := startOfHour(moment).Format("2006-01-02 15"); got != "2026-08-22 01" {
		t.Errorf("startOfHour = %q, want 2026-08-22 01", got)
	}

	if got := moment.Truncate(time.Hour).Format("2006-01-02 15"); got == "2026-08-22 01" {
		t.Skip("this platform's Truncate agrees, so the distinction cannot be demonstrated here")
	}
}
