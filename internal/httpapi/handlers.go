package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"redismonitor/internal/config"
	"redismonitor/internal/monitor"
	"redismonitor/internal/totp"
)

// maxKeyNameLength matches the PHP validation: a Redis key can technically be
// 512MB, but nothing a browser should be posting.
const maxKeyNameLength = 1024

// maxPatternLength caps the MATCH glob.
const maxPatternLength = 200

// keyTypes is the set the type filter accepts.
var keyTypes = []string{"string", "list", "set", "zset", "hash", "stream"}

// Server serves the API and the embedded UI.
type Server struct {
	cfg      *config.Config
	service  *monitor.Service
	spent    *totp.SpentCodes
	sessions *sessions
	log      *slog.Logger

	readLimiter   *rateLimiter
	deleteLimiter *rateLimiter
	loginLimiter  *rateLimiter

	ui []byte
}

// New builds the HTTP server. ui is the embedded single-page document; a nil ui
// serves the API only.
func New(cfg *config.Config, service *monitor.Service, ui []byte, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		service: service,
		// One blacklist for the whole binary, so a code really is single use: one
		// that just let somebody log in cannot also authorise a delete.
		spent:         totp.NewSpentCodes(time.Duration(cfg.CodeReuseWindow) * time.Second),
		sessions:      newSessions(time.Duration(cfg.SessionTTL) * time.Second),
		log:           log,
		readLimiter:   newRateLimiter(cfg.ReadRateLimit),
		deleteLimiter: newRateLimiter(cfg.DeleteRateLimit),
		loginLimiter:  newRateLimiter(cfg.LoginRateLimit),
		ui:            ui,
	}
}

/* ─────────────────────────────── the page ─────────────────────────────── */

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.fail(w, http.StatusNotFound, "Not found.")

		return
	}

	if len(s.ui) == 0 {
		s.fail(w, http.StatusNotFound, "No UI is bundled in this build.")

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.ui)
}

/* ──────────────────────────────── probes ──────────────────────────────── */

// handleLive is liveness: is this process serving? It deliberately does not touch
// Redis — a liveness probe that fails when Redis is down would restart a perfectly
// healthy monitor exactly when it is most wanted.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady is readiness: can this process reach the Redis it monitors?
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Ping(r.Context(), s.cfg.DefaultDatabase()); err != nil {
		writeJSON(w, s.log, http.StatusServiceUnavailable, map[string]interface{}{
			"status":    "unavailable",
			"reachable": false,
			"message":   err.Error(),
		})

		return
	}

	writeJSON(w, s.log, http.StatusOK, map[string]interface{}{"status": "ok", "reachable": true})
}

/* ──────────────────────────────── config ──────────────────────────────── */

// uiConfig is what the page needs before it can draw anything: which databases it
// may offer, whether delete is possible, and the limits it should enforce in the
// form so the API does not have to reject the obvious cases.
type uiConfig struct {
	Enabled bool `json:"enabled"`
	// LoginRequired tells the page to ask for a username and password rather than
	// a raw bearer token; RequiresLoginCode adds the authenticator field to that
	// form. With neither, the page falls back to the token box.
	LoginRequired     bool `json:"login_required"`
	RequiresLoginCode bool `json:"requires_login_code"`
	SessionTTL        int  `json:"session_ttl"`

	Databases           []int    `json:"databases"`
	DefaultDatabase     int      `json:"default_database"`
	Types               []string `json:"types"`
	CanDelete           bool     `json:"can_delete"`
	DeleteBlockedReason string   `json:"delete_blocked_reason,omitempty"`
	RequiresCode        bool     `json:"requires_code"`
	MaxKeysPerDelete    int      `json:"max_keys_per_delete"`
	PageSize            int      `json:"page_size"`
	MaxPageSize         int      `json:"max_page_size"`
	HistoryEnabled      bool     `json:"history_enabled"`
	HistoryDays         int      `json:"history_days"`
	HistoryHours        int      `json:"history_hours"`
	StatsExpiryDays     int      `json:"stats_expiry_days"`
	MetricsCacheSeconds int      `json:"metrics_cache_seconds"`
	StatsCacheSeconds   int      `json:"stats_cache_seconds"`
	Connection          uiConn   `json:"connection"`
}

type uiConn struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	canDelete, reason := s.cfg.CanDelete()

	s.ok(w, uiConfig{
		Enabled:             s.cfg.Enabled,
		LoginRequired:       s.cfg.CanLogin(),
		RequiresLoginCode:   s.cfg.LoginNeedsCode(),
		SessionTTL:          s.cfg.SessionTTL,
		Databases:           s.cfg.Databases,
		DefaultDatabase:     s.cfg.DefaultDatabase(),
		Types:               keyTypes,
		CanDelete:           canDelete,
		DeleteBlockedReason: reason,
		RequiresCode:        s.cfg.RequireAuthenticator,
		MaxKeysPerDelete:    s.cfg.MaxKeysPerDelete,
		PageSize:            s.cfg.PageSize,
		MaxPageSize:         s.cfg.MaxPageSize,
		HistoryEnabled:      s.cfg.HistoryEnabled,
		HistoryDays:         s.cfg.HistoryDays,
		HistoryHours:        s.cfg.HistoryHours,
		StatsExpiryDays:     s.cfg.StatsExpiryDays,
		MetricsCacheSeconds: s.cfg.MetricsCacheSeconds,
		StatsCacheSeconds:   s.cfg.StatsCacheSeconds,
		Connection: uiConn{
			Name: s.cfg.ConnectionName,
			Host: s.cfg.Redis.Host,
			Port: s.cfg.Redis.Port,
		},
	})
}

/* ──────────────────────────────── login ──────────────────────────────── */

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type loginResponse struct {
	Token     string `json:"token"`
	Username  string `json:"username"`
	ExpiresAt int64  `json:"expires_at"`
	ExpiresIn int    `json:"expires_in"`
}

// handleLogin exchanges a username, password and — when
// REDIS_MONITOR_REQUIRE_AUTHENTICATOR_ON_LOGIN is set — a live authenticator code,
// for a session token to send as the bearer credential.
//
// Every rejection answers with the same message and the same status. Telling a
// caller which half was wrong turns one guess into two cheap ones.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.CanLogin() {
		s.fail(w, http.StatusNotFound,
			"Password login is not configured on this instance. Use the bearer token.")

		return
	}

	var request loginRequest

	if err := decodeJSON(r, &request); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())

		return
	}

	// Both comparisons always run, and both are constant time, so a wrong username
	// takes exactly as long as a wrong password.
	userOK := subtle.ConstantTimeCompare([]byte(request.Username), []byte(s.cfg.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(request.Password), []byte(s.cfg.Password)) == 1

	if !userOK || !passOK {
		s.log.Warn("redis monitor: rejected login", "ip", s.clientIP(r), "username", request.Username)
		s.fail(w, http.StatusUnauthorized, "Those credentials were not accepted.")

		return
	}

	if s.cfg.LoginNeedsCode() {
		if status, message := s.verifyCode(request.Code); status != 0 {
			s.fail(w, status, message)

			return
		}
	}

	ip := s.clientIP(r)

	token, expiresAt, err := s.sessions.issue(s.cfg.Username, ip, time.Now())
	if err != nil {
		s.log.Error("cannot issue a session", "error", err)
		s.fail(w, http.StatusInternalServerError, "Could not start a session.")

		return
	}

	s.log.Info("redis monitor: login", "ip", ip, "username", s.cfg.Username)

	s.ok(w, loginResponse{
		Token:     token,
		Username:  s.cfg.Username,
		ExpiresAt: expiresAt.Unix(),
		ExpiresIn: s.cfg.SessionTTL,
	})
}

// handleLogout ends the session the caller is using. A static-token caller has no
// session to end, and says so rather than silently succeeding.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)

	if _, ok := s.sessions.lookup(token, time.Now()); !ok {
		s.ok(w, map[string]interface{}{"ended": false, "note": "this credential is not a session"})

		return
	}

	s.sessions.revoke(token)
	s.ok(w, map[string]interface{}{"ended": true})
}

// handleSession is whoami: how the page tells a live session from an expired one
// without guessing.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if entry, ok := s.sessions.lookup(bearerToken(r), time.Now()); ok {
		s.ok(w, map[string]interface{}{
			"kind":       "session",
			"username":   entry.Username,
			"expires_at": entry.ExpiresAt.Unix(),
			"created_at": entry.CreatedAt.Unix(),
		})

		return
	}

	// Reaching here means the auth middleware let the request through on the static
	// token, or on nothing at all in dev mode.
	kind := "token"
	if s.cfg.Token == "" && s.cfg.Dev {
		kind = "dev"
	}

	s.ok(w, map[string]interface{}{"kind": kind})
}

/* ─────────────────────────────── clients ──────────────────────────────── */

// handleClients lists the connections to the server.
//
// Server-wide rather than per-database: CLIENT LIST reports every connection to the
// instance. The db parameter only picks which connection asks.
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.service.Clients(r.Context(), s.database(r), boolParam(r, "fresh"))
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, clients)
}

/* ─────────────────────────────── read paths ───────────────────────────── */

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.service.Overview(r.Context(), s.database(r), boolParam(r, "fresh"))
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, overview)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days, err := intParam(r, "days", 0)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "days must be a whole number.")

		return
	}

	if days < 0 || days > s.cfg.HistoryDays {
		s.fail(w, http.StatusBadRequest, "days must be between 1 and "+strconv.Itoa(s.cfg.HistoryDays)+".")

		return
	}

	stats, err := s.service.Stats(r.Context(), s.database(r), days, boolParam(r, "fresh"))
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, stats)
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	namespaces, err := s.service.Namespaces(r.Context(), s.database(r), boolParam(r, "fresh"))
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, namespaces)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	pattern := strings.TrimSpace(query.Get("pattern"))
	if len(pattern) > maxPatternLength {
		s.fail(w, http.StatusBadRequest, "pattern is too long.")

		return
	}

	keyType := strings.TrimSpace(query.Get("type"))
	if keyType != "" && !isKeyType(keyType) {
		s.fail(w, http.StatusBadRequest, "type must be one of "+strings.Join(keyTypes, ", ")+".")

		return
	}

	cursor := strings.TrimSpace(query.Get("cursor"))
	if !isDigits(cursor) {
		s.fail(w, http.StatusBadRequest, "cursor must be the value the previous page returned.")

		return
	}

	perPage, err := intParam(r, "per_page", s.cfg.PageSize)
	if err != nil || perPage < 1 || perPage > s.cfg.MaxPageSize {
		s.fail(w, http.StatusBadRequest, "per_page must be between 1 and "+strconv.Itoa(s.cfg.MaxPageSize)+".")

		return
	}

	page, err := s.service.Keys(r.Context(), s.database(r), monitor.KeyQuery{
		Pattern: pattern,
		Type:    keyType,
		Cursor:  cursor,
		PerPage: perPage,
	})
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, page)
}

// handleKey takes the key from the query string rather than the path: Redis key
// names routinely contain ':' and '/', which a path segment would mangle.
func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")

	if key == "" {
		s.fail(w, http.StatusBadRequest, "key is required.")

		return
	}

	if len(key) > maxKeyNameLength {
		s.fail(w, http.StatusBadRequest, "key is too long.")

		return
	}

	detail, err := s.service.KeyDetail(r.Context(), s.database(r), key)
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	s.ok(w, detail)
}

/* ──────────────────────────────── delete ──────────────────────────────── */

type deleteRequest struct {
	DB   *int     `json:"db"`
	Keys []string `json:"keys"`
	Code string   `json:"code"`
}

// handleDelete removes keys the caller has named, authorised by a live
// authenticator code.
//
// There is no pattern delete on purpose: the caller has to have seen and named
// every key it removes.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	canDelete, reason := s.cfg.CanDelete()
	if !canDelete {
		s.fail(w, http.StatusForbidden, reason)

		return
	}

	var request deleteRequest

	if err := decodeJSON(r, &request); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())

		return
	}

	if len(request.Keys) == 0 {
		s.fail(w, http.StatusBadRequest, "Name at least one key to delete.")

		return
	}

	if len(request.Keys) > s.cfg.MaxKeysPerDelete {
		s.fail(w, http.StatusBadRequest,
			"At most "+strconv.Itoa(s.cfg.MaxKeysPerDelete)+" keys can be deleted in one request.")

		return
	}

	for _, key := range request.Keys {
		if key == "" || len(key) > maxKeyNameLength {
			s.fail(w, http.StatusBadRequest, "Every key must be a non-empty name of at most 1024 characters.")

			return
		}
	}

	db := s.cfg.DefaultDatabase()
	if request.DB != nil && s.cfg.AllowsDatabase(*request.DB) {
		db = *request.DB
	}

	if s.cfg.RequireAuthenticator {
		if status, message := s.verifyCode(request.Code); status != 0 {
			s.fail(w, status, message)

			return
		}
	}

	result, err := s.service.DeleteKeys(r.Context(), db, request.Keys)
	if err != nil {
		s.unreachable(w, r, err)

		return
	}

	// Destructive and rare — worth a line naming who removed what.
	s.log.Warn("redis monitor: keys deleted",
		"ip", s.clientIP(r),
		"database", db,
		"deleted", result.Deleted,
		"missing", result.Missing,
		"keys", request.Keys,
	)

	s.ok(w, result)
}

// verifyCode checks the authenticator code and burns it. It returns 0 on success,
// or the status and message to answer with.
//
// A code stays valid for its whole window, which is long enough to replay, so a
// spent code is blacklisted for code_reuse_window seconds: the same six digits
// cannot authorise a second batch.
func (s *Server) verifyCode(code string) (int, string) {
	code = strings.TrimSpace(code)

	if code == "" {
		return http.StatusBadRequest, "An authenticator code is required to delete keys."
	}

	now := time.Now()

	if s.spent.IsSpent(code, now) {
		return http.StatusBadRequest, "That code has already been used. Wait for your authenticator to show the next one."
	}

	if !totp.Verify(s.cfg.TOTPSecret, code, 1, now) {
		s.log.Warn("redis monitor: invalid authenticator code on delete")

		return http.StatusBadRequest, "The authenticator code you entered is invalid or has expired."
	}

	s.spent.Burn(code, now)

	return 0, ""
}

/* ──────────────────────────────── helpers ─────────────────────────────── */

// database resolves ?db=, falling back to the first allowed database rather than
// erroring — a stale bookmark pointing at a database that is no longer selectable
// should still render a page.
func (s *Server) database(r *http.Request) int {
	raw := r.URL.Query().Get("db")
	if raw == "" {
		return s.cfg.DefaultDatabase()
	}

	db, err := strconv.Atoi(raw)
	if err != nil || !s.cfg.AllowsDatabase(db) {
		return s.cfg.DefaultDatabase()
	}

	return db
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intParam(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}

	return strconv.Atoi(raw)
}

func isKeyType(candidate string) bool {
	for _, keyType := range keyTypes {
		if candidate == keyType {
			return true
		}
	}

	return false
}

// isDigits accepts an empty cursor (the first page) or a decimal one.
func isDigits(value string) bool {
	if value == "" {
		return true
	}

	if len(value) > 64 {
		return false
	}

	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}

	return true
}

// decodeJSON reads a request body strictly: unknown fields and trailing content
// are errors, so a typo in a delete payload cannot pass silently.
func decodeJSON(r *http.Request, target interface{}) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return errors.New("the request body is not valid JSON for this endpoint")
	}

	if decoder.More() {
		return errors.New("the request body must be a single JSON object")
	}

	return nil
}
