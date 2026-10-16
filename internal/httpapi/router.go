// Package httpapi exposes the HTTP surface of rc_vistart: the callback
// registry API used by other services, and the event endpoint whose incoming
// requests are forwarded to the registered callbacks.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/vistart/rc_vistart/internal/callback"
)

// Dependencies wires the HTTP handlers to their collaborators.
type Dependencies struct {
	Registry   callback.Registry
	Dispatcher *callback.Dispatcher
	Logger     *slog.Logger
	// MaxBodyBytes bounds request bodies; zero selects a 1 MiB default.
	MaxBodyBytes int64
}

// NewRouter returns the fully wired http.Handler of the service.
func NewRouter(deps Dependencies) http.Handler {
	h := newHandlers(deps)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /api/v1/callbacks", h.registerCallback)
	mux.HandleFunc("GET /api/v1/callbacks", h.listCallbacks)
	mux.HandleFunc("DELETE /api/v1/callbacks/{id}", h.deleteCallback)
	mux.HandleFunc("POST /api/v1/events/{topic}", h.handleEvent)

	return mux
}

type handlers struct {
	registry     callback.Registry
	dispatcher   *callback.Dispatcher
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
		dispatcher:   deps.Dispatcher,
		logger:       logger,
		maxBodyBytes: maxBody,
	}
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// registerRequest is the body accepted by POST /api/v1/callbacks.
type registerRequest struct {
	Topic   string `json:"topic"`
	URL     string `json:"url"`
	Secret  string `json:"secret"`
	Timeout string `json:"timeout"`
}

func (h *handlers) registerCallback(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := h.decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	timeout, err := parseOptionalDuration(req.Timeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "field \"timeout\": "+err.Error())
		return
	}

	cb, err := h.registry.Register(callback.Callback{
		Topic:   req.Topic,
		URL:     req.URL,
		Secret:  req.Secret,
		Timeout: timeout,
	})
	if err != nil {
		if errors.Is(err, callback.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		h.logger.Error("register callback failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not register callback")
		return
	}

	writeJSON(w, http.StatusCreated, cb)
}

func (h *handlers) listCallbacks(w http.ResponseWriter, r *http.Request) {
	cbs, err := h.registry.List(r.URL.Query().Get("topic"))
	if err != nil {
		h.logger.Error("list callbacks failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list callbacks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"callbacks": cbs, "count": len(cbs)})
}

func (h *handlers) deleteCallback(w http.ResponseWriter, r *http.Request) {
	if err := h.registry.Delete(r.PathValue("id")); err != nil {
		if errors.Is(err, callback.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		h.logger.Error("delete callback failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not delete callback")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) handleEvent(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body: "+err.Error())
		return
	}
	if len(bytesTrimSpace(payload)) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must not be empty")
		return
	}
	if !json.Valid(payload) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}

	res, err := h.dispatcher.Dispatch(r.Context(), topic, payload)
	if err != nil {
		h.logger.Error("dispatch failed", slog.String("topic", topic), slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not dispatch event")
		return
	}

	// 202: the event was accepted; individual deliveries are reported in the
	// body because a failing callback must not fail the publisher's request.
	writeJSON(w, http.StatusAccepted, res)
}

func (h *handlers) decode(w http.ResponseWriter, r *http.Request, dst any) error {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		return err
	}
	if len(bytesTrimSpace(payload)) == 0 {
		return errors.New("request body must not be empty")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}
