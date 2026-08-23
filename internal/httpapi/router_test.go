package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"redismonitor/internal/config"
	"redismonitor/internal/monitor"
	"redismonitor/internal/redisx"
)

// unreachableAddr reserves a real port and immediately closes it, so a dial
// against it fails fast with "connection refused" instead of hanging on a
// timeout — the routes this file tests never need Redis to actually answer,
// only to be unreachable in a realistic way.
func unreachableAddr(t *testing.T) (string, int) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}

	addr := listener.Addr().(*net.TCPAddr)
	listener.Close()

	return addr.IP.String(), addr.Port
}

// testServer builds a real Server against a real Service, wired to Redis that is
// deliberately unreachable. That is enough for every route exercised here —
// routing, the feature flag, auth, rate limiting, login, and the
// delete-without-authenticator refusal all resolve before (or without ever)
// touching Redis. Endpoints that need real keyspace data (/overview, /stats,
// /namespaces, /keys, /key, /clients) are exercised end to end by the CI smoke
// job against a real Redis instead — duplicating that here would need either a
// live Redis or a hand-rolled fake, for coverage that already exists.
func testServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()

	host, port := unreachableAddr(t)

	cfg := &config.Config{
		Enabled:              true,
		DataDir:              t.TempDir(),
		Token:                "test-token",
		SessionTTL:           3600,
		ReadRateLimit:        120,
		DeleteRateLimit:      20,
		LoginRateLimit:       10,
		MaxKeysPerDelete:     200,
		Databases:            []int{0},
		RequireAuthenticator: true,
		Redis: config.Redis{
			Host:         host,
			Port:         port,
			DialTimeout:  200 * time.Millisecond,
			ReadTimeout:  200 * time.Millisecond,
			WriteTimeout: 200 * time.Millisecond,
			PoolSize:     1,
		},
	}

	if mutate != nil {
		mutate(cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := redisx.NewPool(cfg.Redis, cfg.Databases)

	service, err := monitor.New(cfg, pool, log)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	return New(cfg, service, nil, log)
}

func doRequest(s *Server, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a valid envelope: %v (body: %s)", err, rec.Body.String())
	}

	return env
}

func TestHealthzIsAlwaysOKRegardlessOfRedis(t *testing.T) {
	s := testServer(t, nil)

	rec := doRequest(s, "GET", "/healthz", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Liveness intentionally answers a bare {"status":"ok"}, not the envelope
	// shape — a probe body, not an API body.
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s, want status ok", rec.Body.String())
	}
}

func TestReadyzReportsUnreachableRedis(t *testing.T) {
	s := testServer(t, nil)

	rec := doRequest(s, "GET", "/readyz", "", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), `"reachable":false`) {
		t.Errorf("body = %s, want reachable:false", rec.Body.String())
	}
}

func TestRequireEnabledGatesTheAPIButNotProbesOrUI(t *testing.T) {
	s := testServer(t, func(cfg *config.Config) { cfg.Enabled = false })

	if rec := doRequest(s, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200 even when disabled", rec.Code)
	}

	if rec := doRequest(s, "GET", "/api/redis-monitor/config", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/config status = %d, want 404 when disabled", rec.Code)
	}

	headers := map[string]string{"Authorization": "Bearer test-token"}
	if rec := doRequest(s, "GET", "/api/redis-monitor/session", "", headers); rec.Code != http.StatusNotFound {
		t.Errorf("/session status = %d, want 404 when disabled", rec.Code)
	}
}

func TestRequireAuth(t *testing.T) {
	s := testServer(t, nil)
	const path = "/api/redis-monitor/session"

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no credential", nil, http.StatusUnauthorized},
		{"wrong static token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"correct static token", map[string]string{"Authorization": "Bearer test-token"}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(s, "GET", path, "", tc.headers)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// A rejected request must invite a bearer credential, not leave the caller
	// guessing how to authenticate.
	rec := doRequest(s, "GET", path, "", nil)
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 with no credential should set WWW-Authenticate")
	}
}

func TestRequireAuthPassesThroughInDevModeWithNoCredential(t *testing.T) {
	s := testServer(t, func(cfg *config.Config) {
		cfg.Token = ""
		cfg.Dev = true
	})

	rec := doRequest(s, "GET", "/api/redis-monitor/session", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in dev mode with no credential configured", rec.Code)
	}
}

func TestRateLimitBlocksThenReportsRetryAfter(t *testing.T) {
	s := testServer(t, func(cfg *config.Config) { cfg.ReadRateLimit = 1 })
	headers := map[string]string{"Authorization": "Bearer test-token"}

	first := doRequest(s, "GET", "/api/redis-monitor/session", "", headers)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}

	second := doRequest(s, "GET", "/api/redis-monitor/session", "", headers)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}

	if second.Header().Get("Retry-After") == "" {
		t.Error("a 429 should set Retry-After")
	}
}

func TestLoginRejectsWrongCredentialsIdentically(t *testing.T) {
	s := testServer(t, func(cfg *config.Config) {
		cfg.Username = "ops"
		cfg.Password = "correct-password"
	})

	wrongUser := doRequest(s, "POST", "/api/redis-monitor/login",
		`{"username":"nope","password":"correct-password"}`, nil)
	wrongPass := doRequest(s, "POST", "/api/redis-monitor/login",
		`{"username":"ops","password":"nope"}`, nil)

	if wrongUser.Code != http.StatusUnauthorized || wrongPass.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d / %d, want 401 / 401", wrongUser.Code, wrongPass.Code)
	}

	// Telling a caller which half was wrong turns one guess into two cheap ones,
	// so both rejections must read identically.
	wrongUserEnv := decodeEnvelope(t, wrongUser)
	wrongPassEnv := decodeEnvelope(t, wrongPass)

	if wrongUserEnv.Message != wrongPassEnv.Message {
		t.Errorf("messages differ: %q vs %q", wrongUserEnv.Message, wrongPassEnv.Message)
	}
}

func TestLoginIssuesASessionThatThenAuthenticates(t *testing.T) {
	s := testServer(t, func(cfg *config.Config) {
		cfg.Username = "ops"
		cfg.Password = "correct-password"
	})

	login := doRequest(s, "POST", "/api/redis-monitor/login",
		`{"username":"ops","password":"correct-password"}`, nil)

	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body: %s)", login.Code, login.Body.String())
	}

	env := decodeEnvelope(t, login)

	data, ok := env.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("login data = %#v, want an object with a token", env.Data)
	}

	token, _ := data["token"].(string)
	if token == "" {
		t.Fatal("login did not return a token")
	}

	session := doRequest(s, "GET", "/api/redis-monitor/session", "",
		map[string]string{"Authorization": "Bearer " + token})

	if session.Code != http.StatusOK {
		t.Fatalf("session status with the issued token = %d, want 200", session.Code)
	}
}

func TestDeleteIsRefusedWithoutAnAuthenticatorSecretBeforeTouchingRedis(t *testing.T) {
	// RequireAuthenticator defaults on in testServer, and no TOTPSecret is set,
	// so this must be a 403 that never reaches the unreachable Redis configured
	// above — CanDelete() short-circuits before any service.DeleteKeys call.
	s := testServer(t, nil)
	headers := map[string]string{"Authorization": "Bearer test-token"}

	rec := doRequest(s, "POST", "/api/redis-monitor/keys/delete",
		`{"keys":["some:key"]}`, headers)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestResponseEnvelopeShape(t *testing.T) {
	s := testServer(t, nil)

	success := doRequest(s, "GET", "/api/redis-monitor/config", "", nil)
	successEnv := decodeEnvelope(t, success)

	if !successEnv.Success || successEnv.Data == nil {
		t.Errorf("success envelope = %+v, want success=true with data", successEnv)
	}

	failure := doRequest(s, "GET", "/api/redis-monitor/session", "", nil)
	failureEnv := decodeEnvelope(t, failure)

	if failureEnv.Success || failureEnv.Message == "" {
		t.Errorf("failure envelope = %+v, want success=false with a message", failureEnv)
	}
}
