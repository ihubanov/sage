package metrics

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// HealthChecker tracks the health status of dependencies.
type HealthChecker struct {
	postgresOK atomic.Bool
	cometbftOK atomic.Bool
	Version    string
}

// NewHealthChecker creates a new health checker.
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{}
}

// SetPostgresHealth updates the PostgreSQL health status.
func (h *HealthChecker) SetPostgresHealth(ok bool) {
	h.postgresOK.Store(ok)
}

// SetCometBFTHealth updates the CometBFT health status.
func (h *HealthChecker) SetCometBFTHealth(ok bool) {
	h.cometbftOK.Store(ok)
}

// IsHealthy returns true if all dependencies are healthy.
func (h *HealthChecker) IsHealthy() bool {
	return h.postgresOK.Load() && h.cometbftOK.Load()
}

// HealthHandler handles GET /health requests.
func (h *HealthChecker) HealthHandler(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	httpStatus := http.StatusOK

	if !h.IsHealthy() {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	// /health is reachable through the wizard's tunnel allowlist; we keep
	// it minimal so internet visitors can't easily fingerprint a SAGE node
	// to a specific version.
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": status,
	})
}

// ReadinessHandler handles GET /ready requests.
func (h *HealthChecker) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	pgOK := h.postgresOK.Load()
	cmtOK := h.cometbftOK.Load()

	status := "ready"
	httpStatus := http.StatusOK

	if !pgOK || !cmtOK {
		status = "not_ready"
		httpStatus = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   status,
		"postgres": pgOK,
		"cometbft": cmtOK,
	})
}
