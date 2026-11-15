package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type healthzPayload struct {
	OK          bool   `json:"ok"`
	Ready       bool   `json:"ready"`
	Status      string `json:"status"`
	Initialized *bool  `json:"initialized,omitempty"`
	Database    string `json:"database,omitempty"`
	UptimeSec   *int64 `json:"uptime_sec,omitempty"`
}

func (s *AppServer) registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
}

// withHealthzBypass intercepts `/healthz` at the outermost HTTP boundary so liveness/readiness
// probes remain 100% exempt from theme error rendering, CSRF checks, WAF bans, and Setup Wizard redirects.
func (s *AppServer) withHealthzBypass(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			s.handleHealthz(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *AppServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	if s == nil || s.deps.Store == nil || s.deps.Store.Ping(pingCtx) != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(healthzPayload{
			OK:       false,
			Ready:    false,
			Status:   "degraded",
			Database: "unavailable",
		})
		return
	}

	initialized := false
	if s.deps.Settings != nil {
		initialized = !s.isSetupUnlocked(pingCtx)
	}
	uptime := int64(0)
	if !s.deps.StartedAt.IsZero() {
		uptime = int64(time.Since(s.deps.StartedAt).Seconds())
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(healthzPayload{
		OK:          true,
		Ready:       true,
		Status:      "ok",
		Initialized: &initialized,
		Database:    "ok",
		UptimeSec:   &uptime,
	})
}
