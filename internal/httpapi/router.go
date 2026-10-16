// Package httpapi exposes the HTTP surface of rc_vistart: endpoint
// registration, order submission, the step-result intake used by receivers,
// and the monitoring views.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
)

// Dependencies wires the handlers to their collaborators.
type Dependencies struct {
	Registry notify.Registry
	Stats    *notify.Stats
	Engine   *order.Engine
	Logger   *slog.Logger
	// MaxBodyBytes bounds request bodies; zero selects a 1 MiB default.
	MaxBodyBytes int64
}

// NewRouter returns the fully wired http.Handler of the service.
func NewRouter(deps Dependencies) http.Handler {
	h := newHandlers(deps)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.health)

	mux.HandleFunc("POST /api/v1/endpoints", h.registerEndpoint)
	mux.HandleFunc("GET /api/v1/endpoints", h.listEndpoints)
	mux.HandleFunc("DELETE /api/v1/endpoints/{topic}", h.deleteEndpoint)

	mux.HandleFunc("GET /api/v1/presets", h.listPresets)

	mux.HandleFunc("POST /api/v1/orders", h.submitOrder)
	mux.HandleFunc("GET /api/v1/orders", h.listOrders)
	mux.HandleFunc("GET /api/v1/orders/{id}", h.getOrder)
	mux.HandleFunc("POST /api/v1/orders/{id}/steps/{step}/result", h.submitResult)

	mux.HandleFunc("GET /api/v1/stats", h.statsView)
	mux.HandleFunc("GET /api/v1/escalations", h.escalations)

	return mux
}

type handlers struct {
	registry     notify.Registry
	stats        *notify.Stats
	engine       *order.Engine
	logger       *slog.Logger
	maxBodyBytes int64
}

func newHandlers(deps Dependencies) *handlers {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBody := deps.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	return &handlers{
		registry:     deps.Registry,
		stats:        deps.Stats,
		engine:       deps.Engine,
		logger:       logger,
		maxBodyBytes: maxBody,
	}
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
