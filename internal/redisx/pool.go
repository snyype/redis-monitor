// Package redisx wraps go-redis with the few things a key browser needs that the
// client does not give directly: one client per database index, an INFO parser,
// and commands that a server is allowed to refuse.
package redisx

import (
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/config"
)

// ClientName is set on every connection the monitor opens, so its own rows are
// identifiable in CLIENT LIST rather than looking like unexplained traffic.
const ClientName = "redis-monitor"

// ErrDatabaseNotAllowed is returned for a database index outside the configured
// range, so a crafted ?db= cannot reach a database the operator did not list.
type ErrDatabaseNotAllowed struct {
	DB int
}

func (e ErrDatabaseNotAllowed) Error() string {
	return fmt.Sprintf("database %d is not selectable", e.DB)
}

// Pool hands out one client per database index.
//
// A go-redis client is pinned to a single database through Options.DB — SELECT on
// a pooled connection would leak across callers — so a monitor that browses
// several databases needs one client each. They are built lazily and kept, since
// a dashboard on auto-refresh returns to the same database over and over.
type Pool struct {
	cfg     config.Redis
	allowed map[int]bool

	mu      sync.Mutex
	clients map[int]*redis.Client
	closed  bool
}

// NewPool prepares a pool for the given databases. No connection is opened here;
// go-redis dials on first use.
func NewPool(cfg config.Redis, databases []int) *Pool {
	allowed := make(map[int]bool, len(databases))
	for _, db := range databases {
		allowed[db] = true
	}

	return &Pool{
		cfg:     cfg,
		allowed: allowed,
		clients: make(map[int]*redis.Client),
	}
}

// Get returns the client for one database index.
func (p *Pool) Get(db int) (*redis.Client, error) {
	if !p.allowed[db] {
		return nil, ErrDatabaseNotAllowed{DB: db}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("redis pool is closed")
	}

	if client, ok := p.clients[db]; ok {
		return client, nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:     p.cfg.Addr(),
		Username: p.cfg.Username,
		Password: p.cfg.Password,
		DB:       db,
		// Names every connection this monitor opens, so the connected-clients page
		// can tell the operator which rows are the monitor looking at itself.
		// CLIENT SETNAME has existed since 2.6.9, so this is safe on old servers.
		ClientName:   ClientName,
		DialTimeout:  p.cfg.DialTimeout,
		ReadTimeout:  p.cfg.ReadTimeout,
		WriteTimeout: p.cfg.WriteTimeout,
		PoolSize:     p.cfg.PoolSize,
	})

	p.clients[db] = client

	return client, nil
}

// Ping confirms the server answers, so an unreachable Redis surfaces as one clean
// error rather than halfway through building a response.
func (p *Pool) Ping(ctx context.Context, db int) error {
	client, err := p.Get(db)
	if err != nil {
		return err
	}

	return client.Ping(ctx).Err()
}

// Close shuts every client down.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	var firstErr error

	for db, client := range p.clients {
		if err := client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}

		delete(p.clients, db)
	}

	return firstErr
}
