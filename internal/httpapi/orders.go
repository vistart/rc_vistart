package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
)

// stepRequest is one step of a caller-supplied step graph.
type stepRequest struct {
	Name      string   `json:"name"`
	Phase     string   `json:"phase,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Topic     string   `json:"topic"`
	DependsOn []string `json:"depends_on,omitempty"`
	Optional  bool     `json:"optional,omitempty"`
}

func (s stepRequest) toSpec() (order.StepSpec, error) {
	kind := order.StepKind(s.Kind)
	switch kind {
	case "":
		kind = order.KindNotify
	case order.KindNotify, order.KindCallback:
	default:
		return order.StepSpec{}, fieldError("steps.kind", errors.New("must be notify or callback"))
	}

	phase := order.Phase(s.Phase)
	switch phase {
	case "":
		phase = order.PhasePrepare
	case order.PhasePrepare, order.PhaseProduce:
	default:
		return order.StepSpec{}, fieldError("steps.phase", errors.New("must be prepare or produce"))
	}

	return order.StepSpec{
		Name:      s.Name,
		Phase:     phase,
		Kind:      kind,
		Topic:     s.Topic,
		DependsOn: s.DependsOn,
		Optional:  s.Optional,
	}, nil
}

// orderRequest is the body accepted by POST /api/v1/orders.
type orderRequest struct {
	Preset      string         `json:"preset,omitempty"`
	ID          string         `json:"id,omitempty"`
	ExternalRef string         `json:"external_ref,omitempty"`
	Subject     string         `json:"subject,omitempty"`
	Items       []notify.Item  `json:"items,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	// Steps overrides the preset with a caller-supplied graph.
	Steps []stepRequest `json:"steps,omitempty"`
}

func (h *handlers) submitOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if err := h.decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	spec := order.Spec{
		ID:          req.ID,
		Preset:      req.Preset,
		ExternalRef: req.ExternalRef,
		Subject:     req.Subject,
		Items:       req.Items,
		Payload:     req.Payload,
	}

	switch {
	case len(req.Steps) > 0:
		steps := make([]order.StepSpec, 0, len(req.Steps))
		for i, sr := range req.Steps {
			s, err := sr.toSpec()
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", fmtField(i, err).Error())
				return
			}
			steps = append(steps, s)
		}
		spec.Steps = steps

	case req.Preset != "":
		preset, err := order.LookupPreset(req.Preset)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		spec.Steps = preset.StepsFor()

	default:
		// The canonical order is the common case, so it is the default.
		spec.Preset = order.PresetOrder
		spec.Steps = order.StandardSteps()
	}

	created, err := h.engine.Submit(spec)
	if err != nil {
		switch {
		case errors.Is(err, order.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid_order", err.Error())
		case errors.Is(err, order.ErrConflict):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			h.logger.Error("submit order failed", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "internal_error", "could not accept order")
		}
		return
	}

	// Accepted rather than created: the order is durable only in memory and
	// its deliveries are still outstanding.
	writeJSON(w, http.StatusAccepted, created)
}

func (h *handlers) listOrders(w http.ResponseWriter, r *http.Request) {
	orders := h.engine.List()
	if state := r.URL.Query().Get("state"); state != "" {
		filtered := orders[:0:0]
		for _, o := range orders {
			if string(o.State) == state {
				filtered = append(filtered, o)
			}
		}
		orders = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orders": orders,
		"count":  len(orders),
	})
}

func (h *handlers) getOrder(w http.ResponseWriter, r *http.Request) {
	o, err := h.engine.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, order.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		h.logger.Error("get order failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not read order")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// resultRequest is the body a receiver posts to report a step outcome.
type resultRequest struct {
	// DeliveryID must echo the X-Rc-Vistart-Delivery header of the delivery
	// being answered, which is how a late or forged result is rejected.
	DeliveryID string          `json:"delivery_id"`
	Success    bool            `json:"success"`
	Data       json.RawMessage `json:"data,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func (h *handlers) submitResult(w http.ResponseWriter, r *http.Request) {
	var req resultRequest
	if err := h.decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.DeliveryID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "delivery_id must not be empty")
		return
	}

	updated, err := h.engine.SubmitResult(
		r.PathValue("id"),
		r.PathValue("step"),
		req.DeliveryID,
		req.Success,
		req.Data,
	)
	if err != nil {
		switch {
		case errors.Is(err, order.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, order.ErrConflict):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			h.logger.Error("submit result failed", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "internal_error", "could not apply result")
		}
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

func fmtField(index int, err error) error {
	return fieldError("steps["+strconv.Itoa(index)+"]", err)
}
