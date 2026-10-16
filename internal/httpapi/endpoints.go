package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/vistart/rc_vistart/internal/notify"
)

// endpointRequest is the body accepted by POST /api/v1/endpoints. Durations
// arrive as strings so a registration reads like the documentation.
type endpointRequest struct {
	Service string `json:"service"`
	Action  string `json:"action"`

	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers []notify.Header `json:"headers,omitempty"`
	Body    string          `json:"body,omitempty"`

	ExpectStatus  []int  `json:"expect_status,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
	ResultTimeout string `json:"result_timeout,omitempty"`
	Secret        string `json:"secret,omitempty"`
	MaxAttempts   int    `json:"max_attempts,omitempty"`
	BackoffBase   string `json:"backoff_base,omitempty"`
	BackoffMax    string `json:"backoff_max,omitempty"`
}

func (req *endpointRequest) toEndpoint() (notify.Endpoint, error) {
	timeout, err := parseOptionalDuration(req.Timeout)
	if err != nil {
		return notify.Endpoint{}, fieldError("timeout", err)
	}
	resultTimeout, err := parseOptionalDuration(req.ResultTimeout)
	if err != nil {
		return notify.Endpoint{}, fieldError("result_timeout", err)
	}
	backoffBase, err := parseOptionalDuration(req.BackoffBase)
	if err != nil {
		return notify.Endpoint{}, fieldError("backoff_base", err)
	}
	backoffMax, err := parseOptionalDuration(req.BackoffMax)
	if err != nil {
		return notify.Endpoint{}, fieldError("backoff_max", err)
	}

	return notify.Endpoint{
		Service:       req.Service,
		Action:        req.Action,
		Method:        req.Method,
		URL:           req.URL,
		Headers:       req.Headers,
		Body:          req.Body,
		ExpectStatus:  req.ExpectStatus,
		Timeout:       timeout,
		ResultTimeout: resultTimeout,
		Secret:        req.Secret,
		MaxAttempts:   req.MaxAttempts,
		BackoffBase:   backoffBase,
		BackoffMax:    backoffMax,
	}, nil
}

func (h *handlers) registerEndpoint(w http.ResponseWriter, r *http.Request) {
	var req endpointRequest
	if err := h.decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ep, err := req.toEndpoint()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	stored, err := h.registry.Register(ep)
	if err != nil {
		if errors.Is(err, notify.ErrInvalid) {
			// Validation failures are the registrant's problem, so they come
			// back with enough detail to fix.
			writeError(w, http.StatusBadRequest, "invalid_registration", err.Error())
			return
		}
		h.logger.Error("register endpoint failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not register endpoint")
		return
	}

	writeJSON(w, http.StatusCreated, stored)
}

func (h *handlers) listEndpoints(w http.ResponseWriter, r *http.Request) {
	endpoints := h.registry.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoints": endpoints,
		"count":     len(endpoints),
	})
}

func (h *handlers) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")

	if err := h.registry.Unregister(topic); err != nil {
		if errors.Is(err, notify.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		h.logger.Error("unregister endpoint failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not remove endpoint")
		return
	}

	// Orders already queued for this topic will escalate with a clear
	// "no endpoint registered" error, which is the honest outcome: the
	// destination is gone and nothing can be delivered to it.
	h.logger.Warn("endpoint removed, queued tasks for this topic will fail",
		slog.String("topic", topic),
	)
	writeJSON(w, http.StatusOK, map[string]string{
		"topic":  topic,
		"status": "removed",
	})
}

// fieldError names the offending request field.
func fieldError(name string, err error) error {
	return fmt.Errorf("field %q: %w", name, err)
}
