package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// session is one logged-in browser.
type session struct {
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	IP        string
}

// sessions holds the logged-in browsers.
//
// In memory, deliberately: a session is worth less than the credentials behind it,
// and a restart of an ops tool asking everyone to log in again is the right
// trade for not persisting bearer material to disk. The static
// REDIS_MONITOR_TOKEN is the credential that survives restarts, and it exists for
// scripts rather than people.
//
// Expiry slides on use, because a dashboard is meant to be left open — an absolute
// deadline would log an operator out mid-incident while they were watching it.
type sessions struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*session
}

func newSessions(ttl time.Duration) *sessions {
	return &sessions{ttl: ttl, entries: make(map[string]*session)}
}

// issue mints a session and returns its opaque token.
func (s *sessions) issue(username, ip string, now time.Time) (string, time.Time, error) {
	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}

	expiresAt := now.Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)

	s.entries[token] = &session{
		Username:  username,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		LastSeen:  now,
		IP:        ip,
	}

	return token, expiresAt, nil
}

// lookup validates a token and slides its expiry forward.
func (s *sessions) lookup(token string, now time.Time) (*session, bool) {
	if token == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[token]
	if !ok {
		return nil, false
	}

	if now.After(entry.ExpiresAt) {
		delete(s.entries, token)

		return nil, false
	}

	entry.LastSeen = now
	entry.ExpiresAt = now.Add(s.ttl)

	copied := *entry

	return &copied, true
}

// revoke ends one session — what the sign-out button does.
func (s *sessions) revoke(token string) {
	s.mu.Lock()
	delete(s.entries, token)
	s.mu.Unlock()
}

func (s *sessions) pruneLocked(now time.Time) {
	for token, entry := range s.entries {
		if now.After(entry.ExpiresAt) {
			delete(s.entries, token)
		}
	}
}

// randomToken is 32 bytes of crypto/rand, base64url encoded. Long enough that a
// plain map lookup leaks nothing useful about a wrong guess.
func randomToken() (string, error) {
	buffer := make([]byte, 32)

	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
