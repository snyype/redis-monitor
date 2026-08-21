// Package monitor is the read model behind the Redis monitor: bounded SCAN
// samples, INFO counters, capped value previews, a recorded trend, and the single
// write — deleting keys the caller has named.
//
// Two properties here are load-bearing and easy to lose in a refactor:
//
//	Everything is bounded. Every SCAN has a key limit AND an iteration cap, every
//	value preview is truncated, and every expensive payload is cached. A monitor
//	that can hang the server it monitors is worse than no monitor.
//
//	Exact and sampled figures are never mixed. Counters from INFO/DBSIZE are exact
//	and labelled so; TTL, type, prefix and memory breakdowns cannot be read out of
//	Redis at all, so they come from a bounded sample that always reports how much
//	of the keyspace it actually saw.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/config"
	"redismonitor/internal/redisx"
)

// Service answers every monitor endpoint.
type Service struct {
	cfg     *config.Config
	pool    *redisx.Pool
	cache   *memoryCache
	history *historyStore
	redact  redactor
	log     *slog.Logger

	// now is injectable so the day/hour bucketing can be tested without waiting
	// for a calendar to turn over.
	now func() time.Time
}

// New builds the service. The history store is opened eagerly so a bad data
// directory fails at boot rather than on the first page load.
func New(cfg *config.Config, pool *redisx.Pool, log *slog.Logger) (*Service, error) {
	history, err := newHistoryStore(cfg, log)
	if err != nil {
		return nil, err
	}

	return &Service{
		cfg:     cfg,
		pool:    pool,
		cache:   newMemoryCache(),
		history: history,
		redact:  newRedactor(cfg.RedactKeyPatterns, cfg.RedactValuePatterns),
		log:     log,
		now:     time.Now,
	}, nil
}

// Config exposes the configuration to the HTTP layer, which needs the same limits
// to validate a request before it reaches Redis.
func (s *Service) Config() *config.Config {
	return s.cfg
}

// Ping reports whether the server answers at all — the offline banner's source.
func (s *Service) Ping(ctx context.Context, db int) error {
	return s.pool.Ping(ctx, db)
}

func (s *Service) client(db int) (*redis.Client, error) {
	return s.pool.Get(db)
}

// dbSize is the exact key count for one database, straight from DBSIZE.
func dbSize(ctx context.Context, client *redis.Client) int64 {
	size, err := client.DBSize(ctx).Result()
	if err != nil {
		return 0
	}

	return size
}

// info reads and parses INFO.
func info(ctx context.Context, client *redis.Client) (*redisx.Info, error) {
	raw, err := client.Info(ctx).Result()
	if err != nil {
		return nil, err
	}

	return redisx.ParseInfo(raw), nil
}

// isReplyError distinguishes "the server answered with an error" from "the
// connection failed".
//
// A pipeline surfaces the first command error through its return value even when
// every other reply arrived intact. For refusable commands that is expected — one
// MEMORY USAGE the server declines must not throw away a whole batch of good TTLs
// — so callers inspect the individual replies and only abort on a transport
// failure. A server-side error implements redis.Error; a network error does not.
func isReplyError(err error) bool {
	if err == nil {
		return false
	}

	var replyErr redis.Error

	return errors.As(err, &replyErr)
}

// scanBatches walks the keyspace with SCAN, handing each batch to fn.
//
// Bounded twice over: fn stops the walk when it has seen enough keys, and
// maxIterations stops it regardless, so a sparse MATCH over a huge keyspace can
// never turn into an unbounded loop. Returning cursor 0 means the iteration
// completed; anything else means it was cut short.
func scanBatches(
	ctx context.Context,
	client *redis.Client,
	match string,
	count int64,
	maxIterations int,
	fn func(keys []string) (bool, error),
) error {
	var cursor uint64

	for iteration := 0; iteration < maxIterations; iteration++ {
		keys, next, err := client.Scan(ctx, cursor, match, count).Result()
		if err != nil {
			return err
		}

		cursor = next

		keepGoing, err := fn(keys)
		if err != nil {
			return err
		}

		if !keepGoing || cursor == 0 {
			return nil
		}
	}

	return nil
}
