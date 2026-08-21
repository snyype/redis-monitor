package httpapi

import (
	"testing"
	"time"
)

func TestSessionIssueAndLookup(t *testing.T) {
	store := newSessions(time.Hour)
	now := time.Unix(1_000_000, 0)

	token, expiresAt, err := store.issue("ops", "10.0.0.1", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if token == "" {
		t.Fatal("issue returned an empty token")
	}

	if !expiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("expiresAt = %v, want one hour after now", expiresAt)
	}

	entry, ok := store.lookup(token, now)
	if !ok {
		t.Fatal("a fresh session did not look up")
	}

	if entry.Username != "ops" || entry.IP != "10.0.0.1" {
		t.Errorf("session = %+v, want the username and IP it was issued with", entry)
	}
}

func TestSessionTokensAreDistinct(t *testing.T) {
	store := newSessions(time.Hour)
	now := time.Unix(1_000_000, 0)

	seen := make(map[string]bool)

	for attempt := 0; attempt < 200; attempt++ {
		token, _, err := store.issue("ops", "10.0.0.1", now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}

		if seen[token] {
			t.Fatalf("issued a duplicate token after %d attempts", attempt)
		}

		seen[token] = true

		// 32 random bytes, base64url without padding.
		if len(token) != 43 {
			t.Fatalf("token length = %d, want 43", len(token))
		}
	}
}

func TestSessionExpiresAndSlides(t *testing.T) {
	store := newSessions(30 * time.Minute)
	now := time.Unix(1_000_000, 0)

	token, _, err := store.issue("ops", "10.0.0.1", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Expiry slides on use: a dashboard left open must not log its operator out
	// mid-incident while they are watching it.
	if _, ok := store.lookup(token, now.Add(25*time.Minute)); !ok {
		t.Fatal("session expired early")
	}

	if _, ok := store.lookup(token, now.Add(50*time.Minute)); !ok {
		t.Fatal("the 25-minute lookup should have slid expiry forward")
	}

	// Untouched for longer than the window, it is gone.
	if _, ok := store.lookup(token, now.Add(2*time.Hour)); ok {
		t.Fatal("an idle session outlived its window")
	}
}

func TestSessionRevoke(t *testing.T) {
	store := newSessions(time.Hour)
	now := time.Unix(1_000_000, 0)

	token, _, err := store.issue("ops", "10.0.0.1", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	store.revoke(token)

	if _, ok := store.lookup(token, now); ok {
		t.Error("a revoked session still looks up — signing out must really end it")
	}
}

func TestSessionLookupRejectsGarbage(t *testing.T) {
	store := newSessions(time.Hour)
	now := time.Unix(1_000_000, 0)

	if _, _, err := store.issue("ops", "10.0.0.1", now); err != nil {
		t.Fatalf("issue: %v", err)
	}

	for _, token := range []string{"", "nonsense", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, ok := store.lookup(token, now); ok {
			t.Errorf("lookup(%q) succeeded, want a rejection", token)
		}
	}
}

func TestSessionPruneDropsExpiredEntries(t *testing.T) {
	store := newSessions(time.Minute)
	now := time.Unix(1_000_000, 0)

	for attempt := 0; attempt < 5; attempt++ {
		if _, _, err := store.issue("ops", "10.0.0.1", now); err != nil {
			t.Fatalf("issue: %v", err)
		}
	}

	// Issuing after the window has passed prunes what expired, so an abandoned
	// browser cannot leave an entry behind for the process lifetime.
	if _, _, err := store.issue("ops", "10.0.0.1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("issue: %v", err)
	}

	store.mu.Lock()
	remaining := len(store.entries)
	store.mu.Unlock()

	if remaining != 1 {
		t.Errorf("kept %d sessions, want only the fresh one", remaining)
	}
}

func TestRateLimiterAllowsThenBlocks(t *testing.T) {
	limiter := newRateLimiter(3)
	now := time.Unix(1_000_000, 0)

	for attempt := 1; attempt <= 3; attempt++ {
		if allowed, _ := limiter.allow("10.0.0.1", now); !allowed {
			t.Fatalf("request %d was blocked inside the budget", attempt)
		}
	}

	allowed, wait := limiter.allow("10.0.0.1", now)

	if allowed {
		t.Fatal("the fourth request was allowed past a limit of 3")
	}

	if wait <= 0 {
		t.Error("a blocked request should report how long to wait")
	}

	// A different caller has its own bucket.
	if allowed, _ := limiter.allow("10.0.0.2", now); !allowed {
		t.Error("one caller's limit blocked another")
	}

	// The bucket refills over time.
	if allowed, _ := limiter.allow("10.0.0.1", now.Add(time.Minute)); !allowed {
		t.Error("the bucket did not refill after a minute")
	}
}
