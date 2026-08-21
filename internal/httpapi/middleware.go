package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// requireAuth gates every API route on a bearer credential, of which there are two
// kinds and they are not interchangeable in spirit:
//
//	a session token — what a human gets from POST /login, expiring and revocable
//	the static token — REDIS_MONITOR_TOKEN, for scripts and curl, which never expires
//
// The static token is compared in constant time: a byte-by-byte comparison that
// bails on the first mismatch leaks the token one character at a time to anyone
// willing to measure. Session tokens are 32 random bytes looked up in a map, where
// a wrong guess reveals nothing worth having.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A monitor with neither credential configured only runs at all in dev mode,
		// which config.Validate enforces at boot.
		if s.cfg.Token == "" && !s.cfg.CanLogin() && s.cfg.Dev {
			next(w, r)

			return
		}

		supplied := bearerToken(r)

		if supplied == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="redis-monitor"`)
			s.fail(w, http.StatusUnauthorized, "A bearer credential is required.")

			return
		}

		if _, ok := s.sessions.lookup(supplied, time.Now()); ok {
			next(w, r)

			return
		}

		if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(supplied), []byte(s.cfg.Token)) == 1 {
			next(w, r)

			return
		}

		s.log.Warn("rejected bearer credential", "ip", s.clientIP(r), "path", r.URL.Path)
		s.fail(w, http.StatusUnauthorized, "That credential is not valid, or the session has expired.")
	}
}

// requireEnabled makes every endpoint 404 when the feature flag is off, so a
// disabled monitor does not even admit to existing.
func (s *Server) requireEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Enabled {
			s.fail(w, http.StatusNotFound, "Not found.")

			return
		}

		next(w, r)
	}
}

// bearerToken reads the token from the Authorization header, falling back to a
// query parameter only for the browser-driven EventSource-style cases that cannot
// set headers. Nothing in this UI needs the fallback; it exists so a curl smoke
// test can use either.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")

	if header != "" {
		if scheme, token, found := strings.Cut(header, " "); found && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}

		return ""
	}

	return strings.TrimSpace(r.URL.Query().Get("access_token"))
}

// rateLimiter is a per-IP token bucket, standing in for Laravel's throttle
// middleware: 120 reads and 20 deletes a minute by default.
//
// The point is not to stop an attacker — the token already does that — but to stop
// an open dashboard, or a loop in a script, from turning into a SCAN storm against
// the server being monitored.
type rateLimiter struct {
	perMinute float64

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	lastAt time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute < 1 {
		perMinute = 1
	}

	return &rateLimiter{
		perMinute: float64(perMinute),
		buckets:   make(map[string]*tokenBucket),
	}
}

// allow reports whether this caller may proceed, and how long to wait if not.
func (l *rateLimiter) allow(key string, at time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	perSecond := l.perMinute / 60

	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &tokenBucket{tokens: l.perMinute, lastAt: at}
		l.buckets[key] = bucket
	}

	bucket.tokens += at.Sub(bucket.lastAt).Seconds() * perSecond
	bucket.lastAt = at

	if bucket.tokens > l.perMinute {
		bucket.tokens = l.perMinute
	}

	if bucket.tokens >= 1 {
		bucket.tokens--

		l.pruneLocked(at)

		return true, 0
	}

	wait := time.Duration((1 - bucket.tokens) / perSecond * float64(time.Second))

	return false, wait
}

// pruneLocked drops buckets that have refilled completely, so the map cannot grow
// without bound behind a proxy that presents many source addresses.
func (l *rateLimiter) pruneLocked(at time.Time) {
	if len(l.buckets) < 1024 {
		return
	}

	full := time.Duration(l.perMinute/(l.perMinute/60)) * time.Second

	for key, bucket := range l.buckets {
		if at.Sub(bucket.lastAt) > full {
			delete(l.buckets, key)
		}
	}
}

func (s *Server) rateLimit(limiter *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowed, wait := limiter.allow(s.clientIP(r), time.Now())

		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			s.fail(w, http.StatusTooManyRequests, "Too many requests. Slow down.")

			return
		}

		next(w, r)
	}
}

// recoverPanics keeps one bad request from taking the whole monitor down.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic serving request",
					"path", r.URL.Path,
					"panic", recovered,
				)

				s.fail(w, http.StatusInternalServerError, "Something went wrong.")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// securityHeaders locks the page down to what it actually needs. The UI is a single
// self-contained document with inline CSS and script and no external requests, so
// the policy can be this tight.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; "+
				"style-src 'unsafe-inline'; "+
				"script-src 'unsafe-inline'; "+
				"img-src data:; "+
				"connect-src 'self'; "+
				"base-uri 'none'; "+
				"form-action 'none'")

		next.ServeHTTP(w, r)
	})
}

// clientIP is the rate-limit key. X-Forwarded-For is honoured only when the
// monitor is told it sits behind a proxy, since a client can otherwise forge it
// and hand itself a fresh bucket per request.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxyHeaders {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if first, _, found := strings.Cut(forwarded, ","); found {
				return strings.TrimSpace(first)
			}

			return strings.TrimSpace(forwarded)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
