package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// envelope is the one response shape every endpoint answers with, so a client has
// exactly one thing to parse and one place to look for a failure message.
type envelope struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already on the wire, so there is nothing left to tell
		// the client. Log it and move on.
		log.Error("cannot encode response", "error", err)
	}
}

func (s *Server) ok(w http.ResponseWriter, data interface{}) {
	writeJSON(w, s.log, http.StatusOK, envelope{Success: true, Data: data})
}

func (s *Server) fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, s.log, status, envelope{Success: false, Message: message})
}

// unreachable is the answer when Redis itself is down.
//
// Redis being unavailable is an operational state this screen exists to report,
// not a 500 to swallow: the page renders an offline banner off the back of this
// rather than an empty dashboard.
func (s *Server) unreachable(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("redis monitor request failed",
		"path", r.URL.Path,
		"error", err,
	)

	writeJSON(w, s.log, http.StatusServiceUnavailable, envelope{
		Success: false,
		Message: err.Error(),
		Data:    map[string]interface{}{"reachable": false},
	})
}
