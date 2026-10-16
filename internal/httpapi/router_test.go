package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
)

type testStack struct {
	server   *httptest.Server
	registry *notify.MemoryRegistry
	engine   *order.Engine
	stats    *notify.Stats
}

func newTestStack(t *testing.T, invoker notify.Invoker) *testStack {
	t.Helper()

	registry := notify.NewMemoryRegistry()
	stats := notify.NewStats()
	engine := order.NewEngine(registry, invoker, stats, order.Config{
		Workers:      2,
		PollInterval: 2 * time.Millisecond,
		PublicURL:    "http://rc.test",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.Start()
	t.Cleanup(engine.Stop)

	srv := httptest.NewServer(NewRouter(Dependencies{
		Registry:     registry,
		Stats:        stats,
		Engine:       engine,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: 1 << 20,
	}))
	t.Cleanup(srv.Close)

	return &testStack{server: srv, registry: registry, engine: engine, stats: stats}
}

// recordingInvoker accepts every delivery and does nothing else.
type recordingInvoker struct {
	mu    sync.Mutex
	calls []notify.ContextInput
}

func (r *recordingInvoker) Invoke(_ context.Context, ep notify.Endpoint, in notify.ContextInput) (notify.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, in)
	return notify.Result{StatusCode: 200}, nil
}

func (s *testStack) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeInto[T any](t *testing.T, resp *http.Response) T {
	t.Helper()

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	return out
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	return decodeInto[errorResponse](t, resp).Error
}

func endpointBody(url string) map[string]any {
	return map[string]any{
		"service": "inventory",
		"action":  "stock.prehold",
		"url":     url,
	}
}

func TestHealth(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodGet, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := decodeInto[map[string]string](t, resp); body["status"] != "ok" {
		t.Errorf("body = %v, want status ok", body)
	}
}

func TestRegisterEndpoint(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	body := endpointBody("https://inv.example.com/prehold")
	body["method"] = "POST"
	body["headers"] = []map[string]string{{"name": "X-Token", "value": "{{ .secret }}"}}
	body["body"] = `{"order":"{{ .order.id }}"}`
	body["timeout"] = "4s"
	body["max_attempts"] = 5

	resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	ep := decodeInto[notify.Endpoint](t, resp)
	if ep.Topic() != "inventory.stock.prehold" {
		t.Errorf("Topic() = %q, want inventory.stock.prehold", ep.Topic())
	}
	if ep.Timeout != 4*time.Second {
		t.Errorf("Timeout = %v, want 4s", ep.Timeout)
	}
	if ep.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", ep.MaxAttempts)
	}
	if ep.ID == "" {
		t.Error("ID is empty")
	}
}

func TestRegisterEndpointRejectsBadInput(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	// Failures raised while reading or converting the request are reported as
	// invalid_request; failures raised by the registry's own validation, such
	// as a template that cannot work, are invalid_registration.
	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no topic", map[string]any{"url": "https://inv.example.com/h"}, "invalid_registration"},
		{"bad url scheme", map[string]any{"service": "s", "action": "a", "url": "ftp://inv/h"}, "invalid_registration"},
		{"bad method", map[string]any{"service": "s", "action": "a", "url": "https://inv/h", "method": "GET"}, "invalid_registration"},
		{"bad template", map[string]any{"service": "s", "action": "a", "url": "https://inv/{{ .order.id "}, "invalid_registration"},
		{"bad duration", map[string]any{"service": "s", "action": "a", "url": "https://inv/h", "timeout": "soon"}, "invalid_request"},
		{"unknown field", map[string]any{"service": "s", "action": "a", "url": "https://inv/h", "surprise": 1}, "invalid_request"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if code := errorCode(t, resp); code != tc.want {
				t.Errorf("error code = %q, want %q", code, tc.want)
			}
		})
	}
}

func TestRegisterEndpointRejectsEmptyBody(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp, err := http.Post(stack.server.URL+"/api/v1/endpoints", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty body", resp.StatusCode)
	}
}

func TestRegisterEndpointNamesTheBadField(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", map[string]any{
		"service": "s", "action": "a", "url": "https://inv/h", "timeout": "nope",
	})
	body := decodeInto[errorResponse](t, resp)
	if !bytes.Contains([]byte(body.Message), []byte("timeout")) {
		t.Errorf("message = %q, want it to name the timeout field", body.Message)
	}
}

func TestListAndDeleteEndpoints(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	if resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", endpointBody("https://inv/prehold")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}

	list := stack.do(t, http.MethodGet, "/api/v1/endpoints", nil)
	listed := decodeInto[struct {
		Endpoints []notify.Endpoint `json:"endpoints"`
		Count     int               `json:"count"`
	}](t, list)
	if listed.Count != 1 || len(listed.Endpoints) != 1 {
		t.Fatalf("list = %+v, want one endpoint", listed)
	}

	del := stack.do(t, http.MethodDelete, "/api/v1/endpoints/inventory.stock.prehold", nil)
	if del.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", del.StatusCode)
	}

	after := stack.do(t, http.MethodGet, "/api/v1/endpoints", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, after); got.Count != 0 {
		t.Errorf("count after delete = %d, want 0", got.Count)
	}

	missing := stack.do(t, http.MethodDelete, "/api/v1/endpoints/inventory.stock.prehold", nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", missing.StatusCode)
	}
}

func TestSubmitOrderWithDefaultPreset(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"external_ref": "ref-1",
		"subject":      "user-9",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	o := decodeInto[order.Order](t, resp)
	if o.Preset != order.PresetOrder {
		t.Errorf("Preset = %q, want the standard order by default", o.Preset)
	}
	if len(o.Steps) != len(order.StandardSteps()) {
		t.Errorf("len(Steps) = %d, want %d", len(o.Steps), len(order.StandardSteps()))
	}
	if o.ID == "" {
		t.Error("ID is empty")
	}
}

func TestSubmitOrderWithNamedPreset(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"preset":  order.PresetContactUpdate,
		"subject": "user-9",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	o := decodeInto[order.Order](t, resp)
	if o.Preset != order.PresetContactUpdate {
		t.Errorf("Preset = %q, want %q", o.Preset, order.PresetContactUpdate)
	}
	if len(o.Steps) != 1 || o.Steps[0].Spec.Topic != order.TopicContactUpdate {
		t.Errorf("steps = %+v, want a single CRM notification", o.Steps)
	}
}

func TestSubmitOrderWithCustomSteps(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"id":      "ord_custom",
		"payload": map[string]any{"channel": "app"},
		"steps": []map[string]any{
			{"name": "first", "topic": "ads.registration", "kind": "notify"},
			{"name": "second", "topic": "crm.contact.update", "kind": "callback", "depends_on": []string{"first"}},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	o := decodeInto[order.Order](t, resp)
	if o.ID != "ord_custom" {
		t.Errorf("ID = %q, want ord_custom", o.ID)
	}
	if len(o.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(o.Steps))
	}
	if o.Steps[1].Spec.Kind != order.KindCallback {
		t.Errorf("second step kind = %q, want callback", o.Steps[1].Spec.Kind)
	}
	if o.Steps[0].TaskKey != "ord_custom/first" {
		t.Errorf("TaskKey = %q, want ord_custom/first", o.Steps[0].TaskKey)
	}
}

func TestSubmitOrderValidation(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"unknown preset", map[string]any{"preset": "nope"}, http.StatusBadRequest},
		{"step without a name", map[string]any{
			"steps": []map[string]any{{"topic": "a.b"}},
		}, http.StatusBadRequest},
		{"step with a bad kind", map[string]any{
			"steps": []map[string]any{{"name": "s", "topic": "a.b", "kind": "magic"}},
		}, http.StatusBadRequest},
		{"step with a bad phase", map[string]any{
			"steps": []map[string]any{{"name": "s", "topic": "a.b", "phase": "ship"}},
		}, http.StatusBadRequest},
		{"step without a topic", map[string]any{
			"steps": []map[string]any{{"name": "s"}},
		}, http.StatusBadRequest},
		{"dependency on an unknown step", map[string]any{
			"steps": []map[string]any{{"name": "s", "topic": "a.b", "depends_on": []string{"ghost"}}},
		}, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := stack.do(t, http.MethodPost, "/api/v1/orders", tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestSubmitDuplicateOrderIsConflict(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	body := map[string]any{"id": "ord_dup", "steps": []map[string]any{{"name": "s", "topic": "a.b"}}}
	if resp := stack.do(t, http.MethodPost, "/api/v1/orders", body); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first submit status = %d, want 202", resp.StatusCode)
	}

	resp := stack.do(t, http.MethodPost, "/api/v1/orders", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "conflict" {
		t.Errorf("error code = %q, want conflict", code)
	}
}

func TestGetAndListOrders(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	if resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"id":    "ord_1",
		"steps": []map[string]any{{"name": "s", "topic": "a.b"}},
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202", resp.StatusCode)
	}

	got := stack.do(t, http.MethodGet, "/api/v1/orders/ord_1", nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", got.StatusCode)
	}
	if o := decodeInto[order.Order](t, got); o.ID != "ord_1" {
		t.Errorf("ID = %q, want ord_1", o.ID)
	}

	missing := stack.do(t, http.MethodGet, "/api/v1/orders/ghost", nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("get(ghost) status = %d, want 404", missing.StatusCode)
	}

	list := stack.do(t, http.MethodGet, "/api/v1/orders", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, list); got.Count != 1 {
		t.Errorf("count = %d, want 1", got.Count)
	}
}

func TestListOrdersFilteredByState(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	// A working endpoint, so the order settles into a known terminal state.
	if resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", map[string]any{
		"service": "svc", "action": "act", "url": "https://svc/act",
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}

	if resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"id":    "ord_1",
		"steps": []map[string]any{{"name": "s", "topic": "svc.act"}},
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if o, err := stack.engine.Get("ord_1"); err == nil && o.State == order.OrderSucceeded {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Filtering is what makes the list usable as a work queue: only the orders
	// that still need attention.
	succeeded := stack.do(t, http.MethodGet, "/api/v1/orders?state=succeeded", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, succeeded); got.Count != 1 {
		t.Errorf("succeeded count = %d, want 1", got.Count)
	}

	failed := stack.do(t, http.MethodGet, "/api/v1/orders?state=failed", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, failed); got.Count != 0 {
		t.Errorf("failed count = %d, want 0", got.Count)
	}

	unfiltered := stack.do(t, http.MethodGet, "/api/v1/orders", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, unfiltered); got.Count != 1 {
		t.Errorf("unfiltered count = %d, want 1", got.Count)
	}
}

func TestSubmitResult(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	// The step only waits for a result if something is registered to receive
	// the notification.
	if resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", map[string]any{
		"service": "svc", "action": "act", "url": "https://svc/act",
		"result_timeout": "30s",
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}

	if resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"id":    "ord_1",
		"steps": []map[string]any{{"name": "s", "topic": "svc.act", "kind": "callback"}},
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202", resp.StatusCode)
	}

	var deliveryID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		o, err := stack.engine.Get("ord_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		s, _ := o.Step("s")
		if s.State == order.StateWaiting {
			deliveryID = s.PendingDeliveryID
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if deliveryID == "" {
		t.Fatal("the step never waited for a result")
	}

	resp := stack.do(t, http.MethodPost, "/api/v1/orders/ord_1/steps/s/result", map[string]any{
		"delivery_id": deliveryID,
		"success":     true,
		"data":        map[string]any{"reserved": 2},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if o := decodeInto[order.Order](t, resp); o.State != order.OrderSucceeded {
		t.Errorf("State = %q, want succeeded", o.State)
	}
}

func TestSubmitResultRequiresDeliveryID(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/orders/ord_1/steps/s/result", map[string]any{
		"success": true,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSubmitResultForUnknownOrder(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodPost, "/api/v1/orders/ghost/steps/s/result", map[string]any{
		"delivery_id": "d", "success": true,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStatsAndEscalations(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	if resp := stack.do(t, http.MethodPost, "/api/v1/endpoints", endpointBody("https://inv/prehold")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}
	if resp := stack.do(t, http.MethodPost, "/api/v1/orders", map[string]any{
		"id":    "ord_1",
		"steps": []map[string]any{{"name": "s", "topic": "inventory.stock.prehold"}},
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(stack.stats.Snapshot()) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	stats := stack.do(t, http.MethodGet, "/api/v1/stats", nil)
	body := decodeInto[struct {
		Topics []statsResponse `json:"topics"`
		Count  int             `json:"count"`
	}](t, stats)
	if body.Count == 0 {
		t.Fatal("no statistics were recorded for a completed delivery")
	}
	if body.Topics[0].Topic != "inventory.stock.prehold" {
		t.Errorf("topic = %q, want inventory.stock.prehold", body.Topics[0].Topic)
	}
	if body.Topics[0].Delivered == 0 {
		t.Error("Delivered = 0, want at least one acknowledged delivery")
	}

	esc := stack.do(t, http.MethodGet, "/api/v1/escalations", nil)
	if got := decodeInto[struct {
		Count int `json:"count"`
	}](t, esc); got.Count != 0 {
		t.Errorf("escalations = %d, want 0 for a healthy run", got.Count)
	}
}

func TestListPresets(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodGet, "/api/v1/presets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeInto[struct {
		Presets []presetView `json:"presets"`
		Count   int          `json:"count"`
	}](t, resp)
	if body.Count != len(order.ListPresets()) {
		t.Errorf("count = %d, want %d", body.Count, len(order.ListPresets()))
	}
	for _, p := range body.Presets {
		if p.Name == "" || len(p.Steps) == 0 {
			t.Errorf("preset %+v is incomplete", p)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	stack := newTestStack(t, &recordingInvoker{})

	resp := stack.do(t, http.MethodGet, "/api/v1/orders/ord_1/steps/s/result", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
