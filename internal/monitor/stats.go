package monitor

import (
	"context"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/redisx"
)

// Stats is the analytics payload, in two halves that answer different questions
// and are labelled as such so neither can be mistaken for the other:
//
//	Sample/Totals/Charts/Tables — what the keyspace looks like RIGHT NOW, from one
//	bounded SCAN that asks TYPE, TTL, MEMORY USAGE and OBJECT IDLETIME per key.
//
//	History — how the keyspace has MOVED, from points this monitor wrote down over
//	time. Redis keeps no history of its own, so the series starts the first time
//	the page was opened and says exactly when that was.
type Stats struct {
	Database    int   `json:"database"`
	GeneratedAt int64 `json:"generated_at"`
	ExpiryDays  int   `json:"expiry_days"`

	Sample StatsSample `json:"sample"`
	Totals StatsTotals `json:"totals"`
	Charts StatsCharts `json:"charts"`
	Tables StatsTables `json:"tables"`

	History *History `json:"history"`
}

// StatsSample says how much of the keyspace the numbers below actually describe.
type StatsSample struct {
	Scanned         int64    `json:"scanned"`
	DBKeys          int64    `json:"db_keys"`
	Truncated       bool     `json:"truncated"`
	CoveragePercent *float64 `json:"coverage_percent"`
	// HasMemory / HasIdle are false when the server refused MEMORY USAGE or
	// OBJECT IDLETIME. The charts that needed them then say why they are empty.
	HasMemory      bool  `json:"has_memory"`
	HasIdle        bool  `json:"has_idle"`
	KeysWithMemory int64 `json:"keys_with_memory"`
}

type StatsTotals struct {
	Volatile           int64   `json:"volatile"`
	Persistent         int64   `json:"persistent"`
	Expiring24h        int64   `json:"expiring_24h"`
	DistinctNamespaces int     `json:"distinct_namespaces"`
	SampledMemory      int64   `json:"sampled_memory"`
	SampledMemoryHuman string  `json:"sampled_memory_human"`
	AvgKeyMemory       *int64  `json:"avg_key_memory"`
	AvgKeyMemoryHuman  *string `json:"avg_key_memory_human"`
	// EstimatedMemory is the average sampled key size times DBSIZE. An estimate,
	// and named one: used_memory on the overview is the figure to trust.
	EstimatedMemory      *int64  `json:"estimated_memory"`
	EstimatedMemoryHuman *string `json:"estimated_memory_human"`
}

type StatsCharts struct {
	ExpiryByDay       ExpiryByDay  `json:"expiry_by_day"`
	ExpiryByHour      ExpiryByHour `json:"expiry_by_hour"`
	KeysByNamespace   []CountRow   `json:"keys_by_namespace"`
	MemoryByNamespace []CountRow   `json:"memory_by_namespace"`
	SizeBuckets       []CountRow   `json:"size_buckets"`
	IdleBuckets       []CountRow   `json:"idle_buckets"`
	TTLBuckets        []CountRow   `json:"ttl_buckets"`
	Types             []CountRow   `json:"types"`
}

// ExpiryByDay keeps the axis purely temporal: every day in the window gets a
// column even when it is empty, and the two counts that have no place on a date
// line travel as separate numbers for the caption rather than as bars pretending
// to be days.
type ExpiryByDay struct {
	Rows         []ExpiryDayRow `json:"rows"`
	NoExpiry     int64          `json:"no_expiry"`
	BeyondWindow int64          `json:"beyond_window"`
	Total        int64          `json:"total"`
}

type ExpiryDayRow struct {
	Date  string `json:"date"`
	Label string `json:"label"`
	Short string `json:"short"`
	Count int64  `json:"count"`
}

type ExpiryByHour struct {
	Rows  []ExpiryHourRow `json:"rows"`
	Total int64           `json:"total"`
}

type ExpiryHourRow struct {
	Hour  string `json:"hour"`
	Label string `json:"label"`
	Short string `json:"short"`
	Count int64  `json:"count"`
}

type StatsTables struct {
	Namespaces  []NamespaceTableRow `json:"namespaces"`
	LargestKeys []LargestKeyRow     `json:"largest_keys"`
}

// NamespaceTableRow is the per-namespace breakdown. Pattern is ready-made and
// glob-escaped so "browse these keys" never builds one in the page; it is null for
// the unnamespaced group, which no glob selects.
type NamespaceTableRow struct {
	Namespace      string   `json:"namespace"`
	Pattern        *string  `json:"pattern"`
	Count          int64    `json:"count"`
	Share          *float64 `json:"share"`
	Memory         int64    `json:"memory"`
	MemoryHuman    string   `json:"memory_human"`
	AvgMemory      *int64   `json:"avg_memory"`
	AvgMemoryHuman *string  `json:"avg_memory_human"`
	Volatile       int64    `json:"volatile"`
	Persistent     int64    `json:"persistent"`
	AvgTTL         *int64   `json:"avg_ttl"`
	AvgTTLLabel    string   `json:"avg_ttl_label"`
	IdleMax        *int64   `json:"idle_max"`
	IdleMaxLabel   *string  `json:"idle_max_label"`
}

type LargestKeyRow struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Memory      int64  `json:"memory"`
	MemoryHuman string `json:"memory_human"`
	TTL         int64  `json:"ttl"`
	TTLLabel    string `json:"ttl_label"`
}

// Stats returns the analytics payload. The sample is cached for
// stats_cache_seconds because it is the most expensive read on the screen; the
// recorded history is read fresh every time, since it is only a file read and the
// window depends on the requested day count.
func (s *Service) Stats(ctx context.Context, db int, days int, fresh bool) (*Stats, error) {
	key := cacheKey("stats", db)
	ttl := time.Duration(s.cfg.StatsCacheSeconds) * time.Second

	var stats *Stats

	if !fresh && ttl > 0 {
		if cached, ok := s.cache.Get(key); ok {
			stats, _ = cached.(*Stats)
		}
	}

	if stats == nil {
		built, err := s.buildStats(ctx, db)
		if err != nil {
			return nil, err
		}

		s.cache.Put(key, built, ttl)
		stats = built
	}

	// Copy before attaching history so a cached payload is never mutated for the
	// next caller, who may ask for a different window.
	withHistory := *stats
	withHistory.History = s.history.series(db, days, s.now())

	return &withHistory, nil
}

func (s *Service) buildStats(ctx context.Context, db int) (*Stats, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	now := s.now()

	serverInfo, err := info(ctx, client)
	if err != nil {
		return nil, err
	}

	keys := dbSize(ctx, client)
	hits := serverInfo.Int("keyspace_hits")
	misses := serverInfo.Int("keyspace_misses")

	// Record before reading the series back, so today's column is not a day behind
	// on a freshly opened page.
	s.history.record(db, historyPoint{
		Keys:     keys,
		Memory:   serverInfo.Int("used_memory"),
		Ops:      serverInfo.Int("instantaneous_ops_per_sec"),
		HitRate:  percent(hits, hits+misses),
		Clients:  serverInfo.Int("connected_clients"),
		Expired:  serverInfo.Int("expired_keys"),
		Evicted:  serverInfo.Int("evicted_keys"),
		RecordAt: now,
	})

	horizon := s.cfg.StatsExpiryDays

	sample, err := s.deepSample(ctx, client, now, horizon)
	if err != nil {
		return nil, err
	}

	var (
		avgMemory      *int64
		avgMemoryHuman *string
		estimated      *int64
		estimatedHuman *string
	)

	if sample.memoryKeys > 0 {
		average := int64(float64(sample.memoryTotal)/float64(sample.memoryKeys) + 0.5)
		avgMemory = &average
		avgMemoryHuman = humanPtr(average)

		total := average * keys
		estimated = &total
		estimatedHuman = humanPtr(total)
	}

	return &Stats{
		Database:    db,
		GeneratedAt: now.Unix(),
		ExpiryDays:  horizon,

		Sample: StatsSample{
			Scanned:         sample.scanned,
			DBKeys:          keys,
			Truncated:       keys > sample.scanned,
			CoveragePercent: coverage(sample.scanned, keys),
			HasMemory:       sample.memoryKeys > 0,
			HasIdle:         sample.idleKeys > 0,
			KeysWithMemory:  sample.memoryKeys,
		},

		Totals: StatsTotals{
			Volatile:             sample.volatile,
			Persistent:           sample.scanned - sample.volatile,
			Expiring24h:          sample.expiring24h,
			DistinctNamespaces:   len(sample.namespaces),
			SampledMemory:        sample.memoryTotal,
			SampledMemoryHuman:   humanBytes(sample.memoryTotal),
			AvgKeyMemory:         avgMemory,
			AvgKeyMemoryHuman:    avgMemoryHuman,
			EstimatedMemory:      estimated,
			EstimatedMemoryHuman: estimatedHuman,
		},

		Charts: StatsCharts{
			ExpiryByDay:       expiryByDayRows(sample, now, horizon),
			ExpiryByHour:      expiryByHourRows(sample, now),
			KeysByNamespace:   namespaceChartRows(sample.namespaces, false, s.cfg.StatsTopNamespaces),
			MemoryByNamespace: namespaceChartRows(sample.namespaces, true, s.cfg.StatsTopNamespaces),
			SizeBuckets:       bucketRows(sample.sizeBuckets, SizeBuckets),
			IdleBuckets:       bucketRows(sample.idleBuckets, IdleBuckets),
			TTLBuckets:        bucketRows(sample.ttlBuckets, TTLBuckets),
			Types:             countRows(sample.types),
		},

		Tables: StatsTables{
			Namespaces:  namespaceTableRows(sample.namespaces, sample.scanned, s.cfg.StatsTopNamespaces),
			LargestKeys: sample.largestKeys(s.cfg.StatsLargestKeys),
		},
	}, nil
}

// namespaceStats is one namespace's accumulated share of the sample.
type namespaceStats struct {
	name       string
	count      int64
	memory     int64
	memoryKeys int64
	volatile   int64
	ttlSum     int64
	idleMax    *int64
}

// deepSampleResult is everything one bounded SCAN collected.
type deepSampleResult struct {
	scanned     int64
	volatile    int64
	expiring24h int64
	beyond      int64
	memoryTotal int64
	memoryKeys  int64
	idleKeys    int64

	types       map[string]int64
	ttlBuckets  map[string]int64
	sizeBuckets map[string]int64
	idleBuckets map[string]int64
	expiryDays  map[string]int64
	expiryHours map[string]int64
	namespaces  map[string]*namespaceStats

	largest []LargestKeyRow
}

func (r *deepSampleResult) largestKeys(n int) []LargestKeyRow {
	sort.Slice(r.largest, func(a, b int) bool {
		if r.largest[a].Memory != r.largest[b].Memory {
			return r.largest[a].Memory > r.largest[b].Memory
		}

		return r.largest[a].Key < r.largest[b].Key
	})

	if len(r.largest) > n {
		return r.largest[:n]
	}

	return r.largest
}

// deepSample is one bounded SCAN that collects everything the analytics charts
// need.
//
// Per key this wants TYPE, TTL, MEMORY USAGE and OBJECT IDLETIME. They go out as
// one pipelined batch per SCAN page rather than four round trips each, so the cost
// tracks the number of batches rather than the number of keys.
//
// MEMORY USAGE and OBJECT IDLETIME are both refusable — MEMORY needs Redis 4 and
// is disabled on some managed offerings, and IDLETIME errors outright under an LFU
// eviction policy. Each is probed once against the first real key and dropped from
// the batch if the server will not answer: an error reply mid-pipeline would cost
// the whole batch its speedup.
func (s *Service) deepSample(ctx context.Context, client *redis.Client, now time.Time, horizonDays int) (*deepSampleResult, error) {
	result := &deepSampleResult{
		types:       make(map[string]int64),
		ttlBuckets:  make(map[string]int64),
		sizeBuckets: make(map[string]int64),
		idleBuckets: make(map[string]int64),
		expiryDays:  make(map[string]int64),
		expiryHours: make(map[string]int64),
		namespaces:  make(map[string]*namespaceStats),
		largest:     []LargestKeyRow{},
	}

	limit := int64(s.cfg.StatsSampleSize)
	if limit <= 0 {
		return result, nil
	}

	withMemory := s.cfg.StatsCollectMemory
	withIdle := s.cfg.StatsCollectIdle
	probed := false

	dayStart := startOfDay(now)
	horizonEnd := dayStart.AddDate(0, 0, horizonDays)
	currentHour := startOfHour(now)

	err := scanBatches(ctx, client, "", int64(s.cfg.ScanCount), s.cfg.MaxScanIterations, func(batch []string) (bool, error) {
		if remaining := limit - result.scanned; int64(len(batch)) > remaining {
			batch = batch[:remaining]
		}

		if len(batch) == 0 {
			return result.scanned < limit, nil
		}

		if !probed {
			probed = true

			if withMemory {
				_, ok := redisx.MemoryUsage(ctx, client, batch[0])
				withMemory = ok
			}

			if withIdle {
				_, ok := redisx.ObjectIdleTime(ctx, client, batch[0])
				withIdle = ok
			}
		}

		types := make([]*redis.StatusCmd, len(batch))
		ttls := make([]*redis.DurationCmd, len(batch))
		memories := make([]*redis.Cmd, len(batch))
		idles := make([]*redis.Cmd, len(batch))

		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for index, key := range batch {
				types[index] = pipe.Type(ctx, key)
				ttls[index] = pipe.TTL(ctx, key)

				if withMemory {
					memories[index] = pipe.Do(ctx, "MEMORY", "USAGE", key, "SAMPLES", 0)
				}

				if withIdle {
					idles[index] = pipe.Do(ctx, "OBJECT", "IDLETIME", key)
				}
			}

			return nil
		})
		if err != nil && !isReplyError(err) {
			return false, err
		}

		for index, key := range batch {
			keyType := ""
			if types[index] != nil && types[index].Err() == nil {
				keyType = types[index].Val()
			}

			// A key that expired between the SCAN and these reads comes back as
			// type "none"; counting it would skew every distribution.
			if keyType == "" || keyType == "none" {
				continue
			}

			ttl := int64(-1)
			if ttls[index] != nil && ttls[index].Err() == nil {
				ttl = durationToTTL(ttls[index].Val())
			}

			var memory, idle *int64

			if withMemory {
				if value, ok := redisx.CmdInt64(memories[index]); ok {
					memory = &value
				}
			}

			if withIdle {
				if value, ok := redisx.CmdInt64(idles[index]); ok {
					idle = &value
				}
			}

			result.observe(key, keyType, ttl, memory, idle, now, currentHour, horizonEnd)
		}

		return result.scanned < limit, nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// observe folds one sampled key into every distribution at once.
func (r *deepSampleResult) observe(
	key, keyType string,
	ttl int64,
	memory, idle *int64,
	now, currentHour, horizonEnd time.Time,
) {
	r.scanned++
	r.types[keyType]++
	r.ttlBuckets[ttlBucketLabel(ttl)]++

	namespace := prefixOf(key)

	stats, ok := r.namespaces[namespace]
	if !ok {
		stats = &namespaceStats{name: namespace}
		r.namespaces[namespace] = stats
	}

	stats.count++

	if memory != nil {
		r.memoryTotal += *memory
		r.memoryKeys++
		r.sizeBuckets[bucketLabel(*memory, SizeBuckets)]++

		stats.memory += *memory
		stats.memoryKeys++

		r.largest = append(r.largest, LargestKeyRow{
			Key:         key,
			Type:        keyType,
			Memory:      *memory,
			MemoryHuman: humanBytes(*memory),
			TTL:         ttl,
			TTLLabel:    ttlLabel(ttl),
		})
	}

	if idle != nil {
		r.idleKeys++
		r.idleBuckets[bucketLabel(*idle, IdleBuckets)]++

		if stats.idleMax == nil || *idle > *stats.idleMax {
			stats.idleMax = int64Ptr(*idle)
		}
	}

	if ttl < 0 {
		return
	}

	r.volatile++
	stats.volatile++
	stats.ttlSum += ttl

	expiresAt := now.Add(time.Duration(ttl) * time.Second)

	if ttl <= 86400 {
		r.expiring24h++

		// A key expiring inside the hour already in progress belongs to that hour's
		// column, not to a slot in the past.
		slot := expiresAt
		if slot.Before(currentHour) {
			slot = currentHour
		}

		r.expiryHours[hourSlot(slot)]++
	}

	if expiresAt.Before(horizonEnd) {
		r.expiryDays[daySlot(expiresAt)]++
	} else {
		r.beyond++
	}
}

func expiryByDayRows(sample *deepSampleResult, now time.Time, horizonDays int) ExpiryByDay {
	dayStart := startOfDay(now)
	rows := make([]ExpiryDayRow, 0, horizonDays)

	for offset := 0; offset < horizonDays; offset++ {
		day := dayStart.AddDate(0, 0, offset)
		date := daySlot(day)

		label := day.Format("Mon 2 Jan")
		short := day.Format("2 Jan")

		switch offset {
		case 0:
			label, short = "Today", "Today"
		case 1:
			label = "Tomorrow"
		}

		rows = append(rows, ExpiryDayRow{
			Date:  date,
			Label: label,
			Short: short,
			Count: sample.expiryDays[date],
		})
	}

	return ExpiryByDay{
		Rows:         rows,
		NoExpiry:     sample.scanned - sample.volatile,
		BeyondWindow: sample.beyond,
		Total:        sample.volatile,
	}
}

func expiryByHourRows(sample *deepSampleResult, now time.Time) ExpiryByHour {
	hourStart := startOfHour(now)
	rows := make([]ExpiryHourRow, 0, 24)

	for offset := 0; offset < 24; offset++ {
		hour := hourStart.Add(time.Duration(offset) * time.Hour)
		slot := hourSlot(hour)

		rows = append(rows, ExpiryHourRow{
			Hour:  slot,
			Label: hour.Format("Mon 2 Jan, 15:00"),
			Short: hour.Format("15"),
			Count: sample.expiryHours[slot],
		})
	}

	return ExpiryByHour{Rows: rows, Total: sample.expiring24h}
}

// namespaceChartRows ranks namespaces by key count or by memory, folding
// everything past the cut into one "Other" bar rather than dropping it.
func namespaceChartRows(namespaces map[string]*namespaceStats, byMemory bool, topN int) []CountRow {
	counts := make(map[string]int64, len(namespaces))

	for name, stats := range namespaces {
		if byMemory {
			counts[name] = stats.memory
		} else {
			counts[name] = stats.count
		}
	}

	rows := topRows(counts, topN)

	for index := range rows {
		if byMemory {
			rows[index].Human = humanPtr(rows[index].Count)
		}

		if stats, ok := namespaces[rows[index].Label]; ok {
			rows[index].Keys = int64Ptr(stats.count)
		}
	}

	return rows
}

// namespaceTableRows is the breakdown table: who owns the keys, the memory and the
// TTLs.
func namespaceTableRows(namespaces map[string]*namespaceStats, scanned int64, topN int) []NamespaceTableRow {
	ranked := make([]*namespaceStats, 0, len(namespaces))
	for _, stats := range namespaces {
		ranked = append(ranked, stats)
	}

	sort.Slice(ranked, func(a, b int) bool {
		if ranked[a].count != ranked[b].count {
			return ranked[a].count > ranked[b].count
		}

		return ranked[a].name < ranked[b].name
	})

	if len(ranked) > topN {
		ranked = ranked[:topN]
	}

	rows := make([]NamespaceTableRow, 0, len(ranked))

	for _, stats := range ranked {
		row := NamespaceTableRow{
			Namespace:   stats.name,
			Count:       stats.count,
			Share:       percent(stats.count, scanned),
			Memory:      stats.memory,
			MemoryHuman: humanBytes(stats.memory),
			Volatile:    stats.volatile,
			Persistent:  stats.count - stats.volatile,
			AvgTTLLabel: "no expiry",
			IdleMax:     stats.idleMax,
		}

		if stats.name != noPrefix {
			pattern := namespacePattern(stats.name)
			row.Pattern = &pattern
		}

		if stats.memoryKeys > 0 {
			average := int64(float64(stats.memory)/float64(stats.memoryKeys) + 0.5)
			row.AvgMemory = &average
			row.AvgMemoryHuman = humanPtr(average)
		}

		if stats.volatile > 0 {
			average := int64(float64(stats.ttlSum)/float64(stats.volatile) + 0.5)
			row.AvgTTL = &average
			row.AvgTTLLabel = ttlLabel(average)
		}

		if stats.idleMax != nil {
			label := ttlLabel(*stats.idleMax)
			row.IdleMaxLabel = &label
		}

		rows = append(rows, row)
	}

	return rows
}

// coverage is the share of the keyspace the sample saw, capped at 100% — a sample
// taken while keys were expiring can otherwise exceed the DBSIZE it is compared
// against.
func coverage(scanned, keys int64) *float64 {
	if keys <= 0 {
		return nil
	}

	value := round1(minFloat(float64(scanned)/float64(keys), 1) * 100)

	return &value
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}

	return b
}

func startOfDay(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
}

// startOfHour is the wall-clock hour boundary.
//
// Deliberately not time.Truncate(time.Hour): Truncate rounds down against the
// zero time in UTC, so in a zone offset by a fraction of an hour (UTC+5:45 here)
// it lands 15 or 30 minutes inside the wrong hour and every hourly slot is
// labelled one hour early.
func startOfHour(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, at.Location())
}

func daySlot(at time.Time) string {
	return at.Format("2006-01-02")
}

func hourSlot(at time.Time) string {
	return at.Format("2006-01-02 15")
}
