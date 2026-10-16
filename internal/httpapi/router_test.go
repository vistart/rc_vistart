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

	"github.com/vistart/rc_vistart/internal/callback"
)

type testServer struct {
	*httptest.Server
	registry   *callback.MemoryRegistry
	dispatcher *callback.Dispatcher
	invoker    callback.Invoker
}

func newTestServer(t *testing.T, invoker callback.Invoker) *testServer {
	t.Helper()

	registry := callback.NewMemoryRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dispatcher := callback.NewDispatcher(registry, invoker, time.Second, 4, logger)

	srv := httptest.NewServer(NewRouter(Dependencies{
		Registry:     registry,
		Dispatcher:   dispatcher,
		Logger:       logger,
		MaxBodyBytes: 1 << 20,
	}))
	t.Cleanup(srv.Close)

	return &testServer{Server: srv, registry: registry, dispatcher: dispatcher, invoker: invoker}
}

// stubInvoker records the callbacks it is handed and fails those listed in
// failFor.
type stubInvoker struct {
	mu      sync.Mutex
	calls   []callback.Callback
	failFor map[string]error
}

func (s *stubInvoker) Invoke(_ context.Context, cb callback.Callback, _ string, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cb)
	return s.failFor[cb.ID]
}

func (s *stubInvoker) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, cb := range s.calls {
		out = append(out, cb.ID)
	}
	return out
}

func doJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body error = %v", err)
	}
	return out
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp := doJSON(t, http.MethodGet, srv.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	if body := decodeBody[map[string]string](t, resp); body["status"] != "ok" {
		t.Errorf("body = %v, want status ok", body)
	}
}

func TestRegisterAndListCallback(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic":   "order.created",
		"url":     "https://billing.example.com/hooks",
		"secret":  "s3cr3t",
		"timeout": "3s",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/callbacks status = %d, want 201: %s", resp.StatusCode, readAll(t, resp))
	}

	created := decodeBody[callback.Callback](t, resp)
	if created.ID == "" {
		t.Error("registered callback has no ID")
	}
	if created.Topic != "order.created" {
		t.Errorf("Topic = %q, want order.created", created.Topic)
	}
	if created.Timeout != 3*time.Second {
		t.Errorf("Timeout = %v, want 3s", created.Timeout)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a timestamp")
	}

	list := doJSON(t, http.MethodGet, srv.URL+"/api/v1/callbacks", nil)
	if list.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/callbacks status = %d, want 200", list.StatusCode)
	}
	body := decodeBody[struct {
		Callbacks []callback.Callback `json:"callbacks"`
		Count     int                 `json:"count"`
	}](t, list)
	if body.Count != 1 || len(body.Callbacks) != 1 {
		t.Fatalf("list body = %+v, want exactly one callback", body)
	}
	if body.Callbacks[0].ID != created.ID {
		t.Errorf("listed ID = %q, want %q", body.Callbacks[0].ID, created.ID)
	}
}

func TestListCallbacksFilteredByTopic(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.created", "url": "https://a.example.com/hooks",
	})
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.paid", "url": "https://b.example.com/hooks",
	})

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/callbacks?topic=order.paid", nil)
	body := decodeBody[struct {
		Callbacks []callback.Callback `json:"callbacks"`
		Count     int                 `json:"count"`
	}](t, resp)
	if body.Count != 1 {
		t.Fatalf("count = %d, want 1", body.Count)
	}
	if body.Callbacks[0].Topic != "order.paid" {
		t.Errorf("topic = %q, want order.paid", body.Callbacks[0].Topic)
	}
}

func TestRegisterCallbackValidation(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	tests := []struct {
		name string
		body any
		want int
	}{
		{"missing topic", map[string]any{"url": "https://a.example.com/hooks"}, http.StatusBadRequest},
		{"missing url", map[string]any{"topic": "order.created"}, http.StatusBadRequest},
		{"non http url", map[string]any{"topic": "order.created", "url": "ftp://a.example.com/hooks"}, http.StatusBadRequest},
		{"bad timeout", map[string]any{"topic": "order.created", "url": "https://a.example.com/hooks", "timeout": "soon"}, http.StatusBadRequest},
		{"negative timeout", map[string]any{"topic": "order.created", "url": "https://a.example.com/hooks", "timeout": "-1s"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"topic": "order.created", "url": "https://a.example.com/hooks", "nope": true}, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, readAll(t, resp))
			}
			if code := decodeBody[errorResponse](t, resp).Error; code != "invalid_request" {
				t.Errorf("error code = %q, want invalid_request", code)
			}
		})
	}
}

func TestRegisterCallbackRejectsMalformedJSON(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp, err := http.Post(srv.URL+"/api/v1/callbacks", "application/json", bytes.NewReader([]byte("{")))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteCallback(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	created := decodeBody[callback.Callback](t, doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.created", "url": "https://a.example.com/hooks",
	}))

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/callbacks/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204: %s", resp.StatusCode, readAll(t, resp))
	}
	if body, _ := io.ReadAll(resp.Body); len(body) != 0 {
		t.Errorf("DELETE body = %q, want it empty", body)
	}

	again := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/callbacks/"+created.ID, nil)
	if again.StatusCode != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", again.StatusCode)
	}
}

func TestDeleteUnknownCallback(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/callbacks/does-not-exist", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleEventInvokesRegisteredCallbacks(t *testing.T) {
	inv := &stubInvoker{failFor: map[string]error{}}
	srv := newTestServer(t, inv)

	a := decodeBody[callback.Callback](t, doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.created", "url": "https://a.example.com/hooks",
	}))
	b := decodeBody[callback.Callback](t, doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.created", "url": "https://b.example.com/hooks",
	}))
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.paid", "url": "https://c.example.com/hooks",
	})

	payload := map[string]any{"id": "order-1", "total": 4200}
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/events/order.created", payload)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/v1/events/order.created status = %d, want 202: %s", resp.StatusCode, readAll(t, resp))
	}

	res := decodeBody[callback.Result](t, resp)
	if res.Topic != "order.created" {
		t.Errorf("Topic = %q, want order.created", res.Topic)
	}
	if res.Delivered != 2 || res.Failed != 0 {
		t.Errorf("result = delivered:%d failed:%d, want 2/0", res.Delivered, res.Failed)
	}
	if res.DeliveryID == "" {
		t.Error("DeliveryID is empty, want a generated id")
	}

	got := inv.ids()
	if len(got) != 2 {
		t.Fatalf("invoked %d callbacks (%v), want 2", len(got), got)
	}
	for _, want := range []string{a.ID, b.ID} {
		found := false
		for _, id := range got {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("callback %q was not invoked (invoked %v)", want, got)
		}
	}
}

func TestHandleEventWithoutSubscribersStillAccepts(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/events/order.created", map[string]any{"id": "order-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	res := decodeBody[callback.Result](t, resp)
	if res.Delivered != 0 || len(res.Deliveries) != 0 {
		t.Errorf("result = %+v, want no deliveries", res)
	}
}

func TestHandleEventReportsDeliveryFailureInBody(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{failFor: map[string]error{}})

	cb := decodeBody[callback.Callback](t, doJSON(t, http.MethodPost, srv.URL+"/api/v1/callbacks", map[string]any{
		"topic": "order.created", "url": "https://a.example.com/hooks",
	}))

	inv, ok := srv.dispatcherInvoker()
	if !ok {
		t.Fatal("could not reach the stub invoker")
	}
	inv.failFor[cb.ID] = context.DeadlineExceeded

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/events/order.created", map[string]any{"id": "order-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: a failing callback must not fail the publisher", resp.StatusCode)
	}
	res := decodeBody[callback.Result](t, resp)
	if res.Failed != 1 || res.Delivered != 0 {
		t.Errorf("result = failed:%d delivered:%d, want 1/0", res.Failed, res.Delivered)
	}
	if res.Deliveries[0].Error == "" {
		t.Error("failed delivery has no error message")
	}
}

func TestHandleEventRejectsBadPayloads(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"whitespace body", "   \n "},
		{"invalid json", "{not json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/events/order.created", bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestHandleEventRejectsOversizedBody(t *testing.T) {
	registry := callback.NewMemoryRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dispatcher := callback.NewDispatcher(registry, &stubInvoker{}, time.Second, 4, logger)

	srv := httptest.NewServer(NewRouter(Dependencies{
		Registry:     registry,
		Dispatcher:   dispatcher,
		Logger:       logger,
		MaxBodyBytes: 64,
	}))
	defer srv.Close()

	body, err := json.Marshal(map[string]string{"pad": string(make([]byte, 256))})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/events/order.created", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a body above MaxBodyBytes", resp.StatusCode)
	}
}

func TestMethodAndRouteNotAllowed(t *testing.T) {
	srv := newTestServer(t, &stubInvoker{})

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/events/order.created", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the event route status = %d, want 405", resp.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	return string(b)
}

// dispatcherInvoker exposes the stub invoker used by the test server.
func (ts *testServer) dispatcherInvoker() (*stubInvoker, bool) {
	inv, ok := ts.invoker.(*stubInvoker)
	return inv, ok
}
