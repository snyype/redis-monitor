package httpapi

import "net/http"

// Handler wires every route.
//
// Layered outside in: the feature flag first (a disabled monitor 404s rather than
// admitting it exists), then the credential, then the throttle — reads at
// read_rate_limit a minute, the single write at a twentieth of that.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Probes stay open: an orchestrator cannot carry a bearer token.
	mux.HandleFunc("GET /healthz", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)

	mux.HandleFunc("GET /", s.handleUI)

	// Login is the one API route outside the auth layer — it is how a credential is
	// obtained — so it carries its own, much tighter throttle. /config is also open,
	// because the page needs to know which login form to draw before it can log in.
	mux.HandleFunc("POST /api/redis-monitor/login",
		s.requireEnabled(s.rateLimit(s.loginLimiter, s.handleLogin)))

	mux.HandleFunc("GET /api/redis-monitor/config",
		s.requireEnabled(s.rateLimit(s.loginLimiter, s.handleConfig)))

	for path, handler := range map[string]http.HandlerFunc{
		"GET /api/redis-monitor/session":    s.handleSession,
		"GET /api/redis-monitor/overview":   s.handleOverview,
		"GET /api/redis-monitor/stats":      s.handleStats,
		"GET /api/redis-monitor/namespaces": s.handleNamespaces,
		"GET /api/redis-monitor/keys":       s.handleKeys,
		"GET /api/redis-monitor/key":        s.handleKey,
		"GET /api/redis-monitor/clients":    s.handleClients,
	} {
		mux.HandleFunc(path, s.requireEnabled(s.requireAuth(s.rateLimit(s.readLimiter, handler))))
	}

	mux.HandleFunc("POST /api/redis-monitor/logout",
		s.requireEnabled(s.requireAuth(s.rateLimit(s.readLimiter, s.handleLogout))))

	mux.HandleFunc("POST /api/redis-monitor/keys/delete",
		s.requireEnabled(s.requireAuth(s.rateLimit(s.deleteLimiter, s.handleDelete))))

	return s.recoverPanics(securityHeaders(mux))
}
