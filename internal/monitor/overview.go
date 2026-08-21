package monitor

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/redisx"
)

// Overview is the header tiles, the meters and the four bar charts.
type Overview struct {
	Connection ConnectionInfo `json:"connection"`
	Server     ServerInfo     `json:"server"`
	Memory     MemoryInfo     `json:"memory"`
	Clients    ClientsInfo    `json:"clients"`
	Throughput ThroughputInfo `json:"throughput"`
	Keyspace   KeyspaceInfo   `json:"keyspace"`
	Sample     KeyspaceSample `json:"sample"`
}

type ConnectionInfo struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Database  int    `json:"database"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
}

type ServerInfo struct {
	Version       string `json:"version"`
	Mode          string `json:"mode"`
	Role          string `json:"role"`
	OS            string `json:"os"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	LastSaveAt    *int64 `json:"last_save_at"`
	AOFEnabled    bool   `json:"aof_enabled"`
}

type MemoryInfo struct {
	Used               int64    `json:"used"`
	UsedHuman          string   `json:"used_human"`
	Peak               int64    `json:"peak"`
	PeakHuman          string   `json:"peak_human"`
	RSS                int64    `json:"rss"`
	RSSHuman           string   `json:"rss_human"`
	Max                int64    `json:"max"`
	MaxHuman           string   `json:"max_human"`
	UsedPercent        *float64 `json:"used_percent"`
	Policy             string   `json:"policy"`
	FragmentationRatio float64  `json:"fragmentation_ratio"`
}

type ClientsInfo struct {
	Connected int64 `json:"connected"`
	Blocked   int64 `json:"blocked"`
}

type ThroughputInfo struct {
	OpsPerSec        int64    `json:"ops_per_sec"`
	TotalCommands    int64    `json:"total_commands"`
	TotalConnections int64    `json:"total_connections"`
	Rejected         int64    `json:"rejected"`
	ExpiredKeys      int64    `json:"expired_keys"`
	EvictedKeys      int64    `json:"evicted_keys"`
	KeyspaceHits     int64    `json:"keyspace_hits"`
	KeyspaceMisses   int64    `json:"keyspace_misses"`
	HitRate          *float64 `json:"hit_rate"`
}

type KeyspaceInfo struct {
	CurrentDBKeys int64            `json:"current_db_keys"`
	Databases     []KeyspaceDBInfo `json:"databases"`
}

type KeyspaceDBInfo struct {
	DB      int    `json:"db"`
	Label   string `json:"label"`
	Keys    int64  `json:"keys"`
	Expires int64  `json:"expires"`
	AvgTTL  int64  `json:"avg_ttl"`
}

// KeyspaceSample is the estimated half of the overview: TTL, type and prefix
// breakdowns, which Redis cannot report and so have to be sampled.
//
// Truncated says the sample is smaller than the keyspace. Every consumer must say
// so on screen — an estimate presented as a count is a bug, not a rounding error.
type KeyspaceSample struct {
	Scanned          int64      `json:"scanned"`
	DBKeys           int64      `json:"db_keys"`
	Truncated        bool       `json:"truncated"`
	DistinctPrefixes int        `json:"distinct_prefixes"`
	VolatileKeys     int64      `json:"volatile_keys"`
	TTLBuckets       []CountRow `json:"ttl_buckets"`
	Types            []CountRow `json:"types"`
	Prefixes         []CountRow `json:"prefixes"`
}

// Overview returns the metrics payload, cached for metrics_cache_seconds so an
// open dashboard on auto-refresh cannot turn into a SCAN loop against production.
func (s *Service) Overview(ctx context.Context, db int, fresh bool) (*Overview, error) {
	key := cacheKey("overview", db)
	ttl := time.Duration(s.cfg.MetricsCacheSeconds) * time.Second

	if !fresh && ttl > 0 {
		if cached, ok := s.cache.Get(key); ok {
			if overview, ok := cached.(*Overview); ok {
				return overview, nil
			}
		}
	}

	overview, err := s.buildOverview(ctx, db)
	if err != nil {
		return nil, err
	}

	s.cache.Put(key, overview, ttl)

	return overview, nil
}

func (s *Service) buildOverview(ctx context.Context, db int) (*Overview, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	serverInfo, err := info(ctx, client)
	if err != nil {
		return nil, err
	}

	hits := serverInfo.Int("keyspace_hits")
	misses := serverInfo.Int("keyspace_misses")
	used := serverInfo.Int("used_memory")
	maxMemory := serverInfo.Int("maxmemory")
	peak := serverInfo.Int("used_memory_peak")
	rss := serverInfo.Int("used_memory_rss")

	keys := dbSize(ctx, client)

	sample, err := s.sampleKeyspace(ctx, client, keys)
	if err != nil {
		return nil, err
	}

	// The overview is the endpoint the page polls, so it is also where the trend
	// series gets its points. Redis remembers nothing between calls; without a
	// point recorded here there is no "per day" to chart later.
	s.history.record(db, historyPoint{
		Keys:     keys,
		Memory:   used,
		Ops:      serverInfo.Int("instantaneous_ops_per_sec"),
		HitRate:  percent(hits, hits+misses),
		Clients:  serverInfo.Int("connected_clients"),
		Expired:  serverInfo.Int("expired_keys"),
		Evicted:  serverInfo.Int("evicted_keys"),
		RecordAt: s.now(),
	})

	maxHuman := "unlimited"
	if maxMemory > 0 {
		maxHuman = humanBytes(maxMemory)
	}

	var lastSave *int64
	if raw, ok := serverInfo.Flat["rdb_last_save_time"]; ok {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			lastSave = &parsed
		}
	}

	return &Overview{
		Connection: ConnectionInfo{
			Name:      s.cfg.ConnectionName,
			Driver:    "go-redis/v9",
			Database:  db,
			Host:      s.cfg.Redis.Host,
			Port:      s.cfg.Redis.Port,
			Reachable: true,
		},
		Server: ServerInfo{
			Version:       serverInfo.Str("redis_version", "—"),
			Mode:          serverInfo.Str("redis_mode", "—"),
			Role:          serverInfo.Str("role", "—"),
			OS:            serverInfo.Str("os", "—"),
			UptimeSeconds: serverInfo.Int("uptime_in_seconds"),
			LastSaveAt:    lastSave,
			AOFEnabled:    serverInfo.Bool("aof_enabled"),
		},
		Memory: MemoryInfo{
			Used:               used,
			UsedHuman:          humanBytes(used),
			Peak:               peak,
			PeakHuman:          humanBytes(peak),
			RSS:                rss,
			RSSHuman:           humanBytes(rss),
			Max:                maxMemory,
			MaxHuman:           maxHuman,
			UsedPercent:        percent(used, maxMemory),
			Policy:             serverInfo.Str("maxmemory_policy", "—"),
			FragmentationRatio: serverInfo.Float("mem_fragmentation_ratio"),
		},
		Clients: ClientsInfo{
			Connected: serverInfo.Int("connected_clients"),
			Blocked:   serverInfo.Int("blocked_clients"),
		},
		Throughput: ThroughputInfo{
			OpsPerSec:        serverInfo.Int("instantaneous_ops_per_sec"),
			TotalCommands:    serverInfo.Int("total_commands_processed"),
			TotalConnections: serverInfo.Int("total_connections_received"),
			Rejected:         serverInfo.Int("rejected_connections"),
			ExpiredKeys:      serverInfo.Int("expired_keys"),
			EvictedKeys:      serverInfo.Int("evicted_keys"),
			KeyspaceHits:     hits,
			KeyspaceMisses:   misses,
			HitRate:          percent(hits, hits+misses),
		},
		Keyspace: KeyspaceInfo{
			CurrentDBKeys: keys,
			Databases:     keyspacePerDatabase(serverInfo),
		},
		Sample: *sample,
	}, nil
}

func keyspacePerDatabase(serverInfo *redisx.Info) []KeyspaceDBInfo {
	rows := make([]KeyspaceDBInfo, 0, len(serverInfo.Keyspace))

	for _, entry := range serverInfo.Keyspace {
		rows = append(rows, KeyspaceDBInfo{
			DB:      entry.DB,
			Label:   "db" + strconv.Itoa(entry.DB),
			Keys:    entry.Keys,
			Expires: entry.Expires,
			AvgTTL:  entry.AvgTTL,
		})
	}

	return rows
}

// sampleKeyspace is the bounded SCAN behind the TTL, type and prefix charts.
//
// TYPE and TTL are asked for every sampled key. The PHP original spends two round
// trips per key; here the whole batch goes out as one pipelined round trip, so the
// cost tracks the number of SCAN batches rather than the number of keys. The
// sample is still capped, and the payload still reports what fraction it saw.
func (s *Service) sampleKeyspace(ctx context.Context, client *redis.Client, keys int64) (*KeyspaceSample, error) {
	limit := int64(s.cfg.MetricsSampleSize)

	sample := &KeyspaceSample{
		DBKeys:     keys,
		TTLBuckets: bucketRows(nil, TTLBuckets),
		Types:      []CountRow{},
		Prefixes:   []CountRow{},
	}

	if limit <= 0 {
		sample.Truncated = keys > 0

		return sample, nil
	}

	ttlBuckets := make(map[string]int64)
	types := make(map[string]int64)
	prefixes := make(map[string]int64)

	var scanned, volatile int64

	err := scanBatches(ctx, client, "", int64(s.cfg.ScanCount), s.cfg.MaxScanIterations, func(batch []string) (bool, error) {
		if remaining := limit - scanned; int64(len(batch)) > remaining {
			batch = batch[:remaining]
		}

		if len(batch) == 0 {
			return scanned < limit, nil
		}

		rows, err := describeForSample(ctx, client, batch)
		if err != nil {
			return false, err
		}

		for _, row := range rows {
			// A key that expired between the SCAN and these reads comes back as
			// type "none"; counting it would skew every distribution.
			if row.Type == "" || row.Type == "none" {
				continue
			}

			scanned++
			ttlBuckets[ttlBucketLabel(row.TTL)]++
			types[row.Type]++
			prefixes[prefixOf(row.Key)]++

			if row.TTL >= 0 {
				volatile++
			}
		}

		return scanned < limit, nil
	})
	if err != nil {
		return nil, err
	}

	sample.Scanned = scanned
	sample.Truncated = keys > scanned
	sample.DistinctPrefixes = len(prefixes)
	sample.VolatileKeys = volatile
	sample.TTLBuckets = bucketRows(ttlBuckets, TTLBuckets)
	sample.Types = countRows(types)
	sample.Prefixes = topRows(prefixes, s.cfg.TopPrefixes)

	return sample, nil
}

// sampleRow is the minimum a distribution needs about one key.
type sampleRow struct {
	Key  string
	Type string
	TTL  int64
}

// describeForSample asks TYPE and TTL for a whole SCAN batch in one round trip.
func describeForSample(ctx context.Context, client *redis.Client, batch []string) ([]sampleRow, error) {
	types := make([]*redis.StatusCmd, len(batch))
	ttls := make([]*redis.DurationCmd, len(batch))

	_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, key := range batch {
			types[index] = pipe.Type(ctx, key)
			ttls[index] = pipe.TTL(ctx, key)
		}

		return nil
	})
	// A pipeline reports the first command error through the return value even when
	// every other reply is fine, so individual replies are inspected below and only
	// a transport failure aborts the batch.
	if err != nil && !isReplyError(err) {
		return nil, err
	}

	rows := make([]sampleRow, 0, len(batch))

	for index, key := range batch {
		row := sampleRow{Key: key, TTL: -1}

		if types[index] != nil && types[index].Err() == nil {
			row.Type = types[index].Val()
		}

		if ttls[index] != nil && ttls[index].Err() == nil {
			row.TTL = durationToTTL(ttls[index].Val())
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// durationToTTL converts go-redis's TTL duration back to the raw Redis answer:
// -1 for a key with no expiry, -2 for a key that is gone.
func durationToTTL(value time.Duration) int64 {
	switch value {
	case -1:
		return -1
	case -2:
		return -2
	}

	if value < 0 {
		return -1
	}

	return int64(value / time.Second)
}
