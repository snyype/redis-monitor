package monitor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"redismonitor/internal/config"
)

// History is the recorded trend: keys and memory per day and per hour.
//
// Every slot in the window gets a row whether or not anything was recorded in it,
// so the chart keeps an even time axis and a gap reads as "nobody looked then"
// rather than as a drop to zero — which is what the explicit Recorded flag on each
// row is for.
type History struct {
	Enabled  bool `json:"enabled"`
	Database int  `json:"database"`
	Days     int  `json:"days"`
	// RecordedFrom is when this series actually begins. The UI says "recording
	// since …" rather than implying older data exists.
	RecordedFrom *int64       `json:"recorded_from"`
	RecordedAt   *int64       `json:"recorded_at"`
	DayPoints    int          `json:"day_points"`
	HourPoints   int          `json:"hour_points"`
	Interval     int          `json:"interval"`
	ByDay        []HistoryRow `json:"by_day"`
	ByHour       []HistoryRow `json:"by_hour"`
}

// HistoryRow is one slot of a recorded series.
//
// Expired and Evicted are DELTAS between consecutive recorded slots. INFO reports
// them as counters since server start, so a negative delta means the server
// restarted in between and the row reports null instead of a nonsense number.
type HistoryRow struct {
	Slot        string   `json:"slot"`
	Label       string   `json:"label"`
	Short       string   `json:"short"`
	Recorded    bool     `json:"recorded"`
	Keys        *int64   `json:"keys"`
	KeysMax     *int64   `json:"keys_max"`
	KeysMin     *int64   `json:"keys_min"`
	Memory      *int64   `json:"memory"`
	MemoryHuman *string  `json:"memory_human"`
	MemoryMax   *int64   `json:"memory_max"`
	Ops         *int64   `json:"ops"`
	HitRate     *float64 `json:"hit_rate"`
	Clients     *int64   `json:"clients"`
	Expired     *int64   `json:"expired"`
	Evicted     *int64   `json:"evicted"`
	Samples     int64    `json:"samples"`
	RecordedAt  *int64   `json:"recorded_at"`
}

// historyPoint is one observation on its way into the series.
type historyPoint struct {
	Keys     int64
	Memory   int64
	Ops      int64
	HitRate  *float64
	Clients  int64
	Expired  int64
	Evicted  int64
	RecordAt time.Time
}

// historyBucket is what one hour or one day slot remembers.
//
// The last reading plus the min and max seen inside the slot, so "keys per day"
// stays a real reading taken at a known time rather than an average of nothing.
type historyBucket struct {
	Keys      int64    `json:"keys"`
	KeysMax   int64    `json:"keys_max"`
	KeysMin   int64    `json:"keys_min"`
	Memory    int64    `json:"memory"`
	MemoryMax int64    `json:"memory_max"`
	Ops       int64    `json:"ops"`
	HitRate   *float64 `json:"hit_rate"`
	Clients   int64    `json:"clients"`
	Expired   int64    `json:"expired"`
	Evicted   int64    `json:"evicted"`
	Samples   int64    `json:"samples"`
	FirstAt   int64    `json:"first_at"`
	LastAt    int64    `json:"last_at"`
}

type historyFile struct {
	Daily   map[string]*historyBucket `json:"daily"`
	Hourly  map[string]*historyBucket `json:"hourly"`
	FirstAt int64                     `json:"first_at"`
	LastAt  int64                     `json:"last_at"`
}

// historyStore keeps the recorded series.
//
// Redis keeps no history of its own: INFO and DBSIZE only ever describe right now,
// so "keys per day" cannot be read out of Redis — it has to be written down. It is
// deliberately NOT written into Redis: a monitor that stored keys in the keyspace
// it reports on would turn up in its own charts. A JSON file per database under the
// data directory means the series also survives a restart of this binary.
type historyStore struct {
	cfg *config.Config
	log *slog.Logger

	mu    sync.Mutex
	files map[int]*historyFile
}

func newHistoryStore(cfg *config.Config, log *slog.Logger) (*historyStore, error) {
	store := &historyStore{cfg: cfg, log: log, files: make(map[int]*historyFile)}

	if !cfg.HistoryEnabled {
		return store, nil
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("history data directory %s: %w", cfg.DataDir, err)
	}

	return store, nil
}

func (h *historyStore) path(db int) string {
	return filepath.Join(h.cfg.DataDir, fmt.Sprintf("history-db%d.json", db))
}

// record appends one observation, rate limited to one point per
// history_min_interval_seconds so a page left open on auto-refresh cannot rewrite
// the series every 30 seconds for nothing — the buckets are hourly at their finest.
//
// Silent on failure by design: losing a trend point must never break the page the
// point was collected from.
func (h *historyStore) record(db int, point historyPoint) {
	if !h.cfg.HistoryEnabled {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	file := h.load(db)
	at := point.RecordAt.Unix()

	if h.cfg.HistoryMinInterval > 0 && at-file.LastAt < int64(h.cfg.HistoryMinInterval) {
		return
	}

	for _, slot := range []struct {
		buckets map[string]*historyBucket
		key     string
	}{
		{file.Hourly, hourSlot(point.RecordAt)},
		{file.Daily, daySlot(point.RecordAt)},
	} {
		existing := slot.buckets[slot.key]

		bucket := &historyBucket{
			Keys:      point.Keys,
			KeysMax:   point.Keys,
			KeysMin:   point.Keys,
			Memory:    point.Memory,
			MemoryMax: point.Memory,
			Ops:       point.Ops,
			HitRate:   point.HitRate,
			Clients:   point.Clients,
			Expired:   point.Expired,
			Evicted:   point.Evicted,
			Samples:   1,
			FirstAt:   at,
			LastAt:    at,
		}

		if existing != nil {
			bucket.KeysMax = maxInt64(point.Keys, existing.KeysMax)
			bucket.KeysMin = minInt64(point.Keys, existing.KeysMin)
			bucket.MemoryMax = maxInt64(point.Memory, existing.MemoryMax)
			bucket.Samples = existing.Samples + 1
			bucket.FirstAt = existing.FirstAt
		}

		slot.buckets[slot.key] = bucket
	}

	file.Hourly = trimSeries(file.Hourly, h.cfg.HistoryHours)
	file.Daily = trimSeries(file.Daily, h.cfg.HistoryDays)

	if file.FirstAt == 0 {
		file.FirstAt = at
	}

	file.LastAt = at

	h.save(db, file)
}

// series renders the recorded window.
func (h *historyStore) series(db, days int, now time.Time) *History {
	days = h.clampDays(days)

	result := &History{
		Enabled:  h.cfg.HistoryEnabled,
		Database: db,
		Days:     days,
		Interval: h.cfg.HistoryMinInterval,
		ByDay:    []HistoryRow{},
		ByHour:   []HistoryRow{},
	}

	if !h.cfg.HistoryEnabled {
		return result
	}

	h.mu.Lock()
	file := h.load(db)

	// Snapshot under the lock; rendering touches no shared state afterwards.
	daily := copyBuckets(file.Daily)
	hourly := copyBuckets(file.Hourly)
	firstAt, lastAt := file.FirstAt, file.LastAt
	h.mu.Unlock()

	if firstAt > 0 {
		result.RecordedFrom = int64Ptr(firstAt)
	}

	if lastAt > 0 {
		result.RecordedAt = int64Ptr(lastAt)
	}

	result.DayPoints = len(daily)
	result.HourPoints = len(hourly)
	result.ByDay = historyDayRows(daily, days, now)
	result.ByHour = historyHourRows(hourly, h.cfg.HistoryHours, now)

	return result
}

func historyDayRows(daily map[string]*historyBucket, days int, now time.Time) []HistoryRow {
	today := startOfDay(now)
	rows := make([]HistoryRow, 0, days)

	var previous *historyBucket

	for offset := days - 1; offset >= 0; offset-- {
		day := today.AddDate(0, 0, -offset)
		slot := daySlot(day)

		label := day.Format("Mon 2 Jan")

		switch offset {
		case 0:
			label = "Today"
		case 1:
			label = "Yesterday"
		}

		bucket := daily[slot]
		rows = append(rows, historyRow(slot, label, day.Format("2 Jan"), bucket, previous))

		if bucket != nil {
			previous = bucket
		}
	}

	return rows
}

func historyHourRows(hourly map[string]*historyBucket, hours int, now time.Time) []HistoryRow {
	thisHour := startOfHour(now)
	rows := make([]HistoryRow, 0, hours)

	var previous *historyBucket

	for offset := hours - 1; offset >= 0; offset-- {
		hour := thisHour.Add(-time.Duration(offset) * time.Hour)
		slot := hourSlot(hour)

		bucket := hourly[slot]
		rows = append(rows, historyRow(slot, hour.Format("Mon 2 Jan, 15:00"), hour.Format("15"), bucket, previous))

		if bucket != nil {
			previous = bucket
		}
	}

	return rows
}

func historyRow(slot, label, short string, bucket, previous *historyBucket) HistoryRow {
	row := HistoryRow{Slot: slot, Label: label, Short: short}

	if bucket == nil {
		return row
	}

	row.Recorded = true
	row.Keys = int64Ptr(bucket.Keys)
	row.KeysMax = int64Ptr(bucket.KeysMax)
	row.KeysMin = int64Ptr(bucket.KeysMin)
	row.Memory = int64Ptr(bucket.Memory)
	row.MemoryHuman = humanPtr(bucket.Memory)
	row.MemoryMax = int64Ptr(bucket.MemoryMax)
	row.Ops = int64Ptr(bucket.Ops)
	row.HitRate = bucket.HitRate
	row.Clients = int64Ptr(bucket.Clients)
	row.Expired = counterDelta(bucket.Expired, previous, func(b *historyBucket) int64 { return b.Expired })
	row.Evicted = counterDelta(bucket.Evicted, previous, func(b *historyBucket) int64 { return b.Evicted })
	row.Samples = bucket.Samples
	row.RecordedAt = int64Ptr(bucket.LastAt)

	return row
}

// counterDelta is the difference between two cumulative INFO counters, or nil when
// it cannot be trusted: no earlier point, or the counter went backwards because
// the server restarted in between.
func counterDelta(current int64, previous *historyBucket, field func(*historyBucket) int64) *int64 {
	if previous == nil {
		return nil
	}

	delta := current - field(previous)
	if delta < 0 {
		return nil
	}

	return &delta
}

func (h *historyStore) clampDays(days int) int {
	if days <= 0 {
		return h.cfg.HistoryDays
	}

	return minInt(days, h.cfg.HistoryDays)
}

// load returns the in-memory series for one database, reading it off disk the first
// time. Caller holds the lock.
func (h *historyStore) load(db int) *historyFile {
	if file, ok := h.files[db]; ok {
		return file
	}

	file := &historyFile{
		Daily:  make(map[string]*historyBucket),
		Hourly: make(map[string]*historyBucket),
	}

	raw, err := os.ReadFile(h.path(db))
	if err == nil {
		if err := json.Unmarshal(raw, file); err != nil {
			// A corrupt file is not worth failing a page load over; start a new
			// series and say so once.
			h.log.Warn("discarding unreadable history file", "path", h.path(db), "error", err)

			file = &historyFile{
				Daily:  make(map[string]*historyBucket),
				Hourly: make(map[string]*historyBucket),
			}
		}
	} else if !os.IsNotExist(err) {
		h.log.Warn("cannot read history file", "path", h.path(db), "error", err)
	}

	if file.Daily == nil {
		file.Daily = make(map[string]*historyBucket)
	}

	if file.Hourly == nil {
		file.Hourly = make(map[string]*historyBucket)
	}

	h.files[db] = file

	return file
}

// save writes the series out through a temp file and a rename, so a crash mid-write
// cannot leave a half-written series behind. Caller holds the lock.
func (h *historyStore) save(db int, file *historyFile) {
	encoded, err := json.Marshal(file)
	if err != nil {
		h.log.Warn("cannot encode history", "db", db, "error", err)

		return
	}

	final := h.path(db)
	temp := final + ".tmp"

	if err := os.WriteFile(temp, encoded, 0o640); err != nil {
		h.log.Warn("cannot write history file", "path", temp, "error", err)

		return
	}

	if err := os.Rename(temp, final); err != nil {
		h.log.Warn("cannot replace history file", "path", final, "error", err)
		_ = os.Remove(temp)
	}
}

// trimSeries keeps the newest slots and drops the rest.
func trimSeries(series map[string]*historyBucket, keep int) map[string]*historyBucket {
	if len(series) <= keep {
		return series
	}

	slots := make([]string, 0, len(series))
	for slot := range series {
		slots = append(slots, slot)
	}

	// Slot keys are zero-padded timestamps, so lexical order is chronological.
	sort.Strings(slots)

	for _, slot := range slots[:len(series)-keep] {
		delete(series, slot)
	}

	return series
}

func copyBuckets(source map[string]*historyBucket) map[string]*historyBucket {
	copied := make(map[string]*historyBucket, len(source))

	for slot, bucket := range source {
		clone := *bucket
		copied[slot] = &clone
	}

	return copied
}
