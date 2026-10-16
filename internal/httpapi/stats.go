package httpapi

import (
	"net/http"

	"github.com/vistart/rc_vistart/internal/order"
)

// statsResponse adds the derived latency figure that a dashboard wants, so the
// client does not have to recompute it from the raw counters.
type statsResponse struct {
	Topic            string  `json:"topic"`
	Service          string  `json:"service"`
	Action           string  `json:"action"`
	URL              string  `json:"url"`
	Attempts         int64   `json:"attempts"`
	Delivered        int64   `json:"delivered"`
	Failed           int64   `json:"failed"`
	Escalated        int64   `json:"escalated"`
	AverageLatencyMs float64 `json:"average_latency_ms"`
	LastError        string  `json:"last_error,omitempty"`
}

// stats serves the per-endpoint delivery statistics. It exists so that
// "we never heard back" is visible as a number, not only as a log line.
func (h *handlers) statsView(w http.ResponseWriter, r *http.Request) {
	out := []statsResponse{}
	if h.stats != nil {
		for _, ts := range h.stats.Snapshot() {
			out = append(out, statsResponse{
				Topic:            ts.Topic,
				Service:          ts.Service,
				Action:           ts.Action,
				URL:              ts.URL,
				Attempts:         ts.Attempts,
				Delivered:        ts.Delivered,
				Failed:           ts.Failed,
				Escalated:        ts.Escalated,
				AverageLatencyMs: ts.AverageLatencyMs(),
				LastError:        ts.LastError,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"topics": out, "count": len(out)})
}

// escalationView is one task that exhausted its attempt budget.
type escalationView struct {
	OrderID  string `json:"order_id"`
	TaskKey  string `json:"task_key"`
	Step     string `json:"step"`
	Topic    string `json:"topic"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

// escalations lists the tasks that need a human. This is the queue an operator
// works through.
func (h *handlers) escalations(w http.ResponseWriter, r *http.Request) {
	list := h.engine.Escalations()
	out := make([]escalationView, 0, len(list))
	for _, esc := range list {
		out = append(out, escalationView{
			OrderID:  esc.OrderID,
			TaskKey:  esc.TaskKey,
			Step:     esc.Step,
			Topic:    esc.Topic,
			Attempts: esc.Attempts,
			Error:    errString(esc.Err),
			At:       esc.At.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"escalations": out, "count": len(out)})
}

// presetStepView is one step of a built-in scenario.
type presetStepView struct {
	Name      string   `json:"name"`
	Phase     string   `json:"phase"`
	Kind      string   `json:"kind"`
	Topic     string   `json:"topic"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type presetView struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Steps       []presetStepView `json:"steps"`
}

// listPresets serves the built-in scenario step graphs.
func (h *handlers) listPresets(w http.ResponseWriter, r *http.Request) {
	available := order.ListPresets()
	out := make([]presetView, 0, len(available))
	for _, p := range available {
		steps := make([]presetStepView, 0, len(p.Steps))
		for _, s := range p.Steps {
			steps = append(steps, presetStepView{
				Name:      s.Name,
				Phase:     string(s.Phase),
				Kind:      string(s.Kind),
				Topic:     s.Topic,
				DependsOn: s.DependsOn,
			})
		}
		out = append(out, presetView{Name: p.Name, Description: p.Description, Steps: steps})
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": out, "count": len(out)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
