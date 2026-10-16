package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// endpointFor points a compiled endpoint at a test server.
func endpointFor(t *testing.T, url string, mutate ...func(*Endpoint)) Endpoint {
	t.Helper()

	ep := Endpoint{
		Service:      "svc",
		Action:       "act",
		Method:       http.MethodPost,
		URL:          url,
		Timeout:      2 * time.Second,
		MaxAttempts:  3,
		BackoffBase:  time.Millisecond,
		BackoffMax:   10 * time.Millisecond,
		ExpectStatus: []int{200, 202, 204},
	}
	for _, m := range mutate {
		m(&ep)
	}
	return registerForTest(t, ep)
}

func TestInvokeSendsRenderedRequest(t *testing.T) {
	type received struct {
		method string
		body   string
		header http.Header
	}
	got := make(chan received, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{method: r.Method, body: string(body), header: r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL, func(e *Endpoint) {
		e.Method = http.MethodPut
		e.Headers = []Header{{Name: "X-Order", Value: "{{ .order.id }}"}}
		e.Body = `{"order":"{{ .order.id }}"}`
	})

	in := sampleInput()
	res, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, in)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if res.Latency <= 0 {
		t.Errorf("Latency = %v, want a positive duration", res.Latency)
	}

	r := <-got
	if r.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", r.method)
	}
	if r.body != `{"order":"ord_123"}` {
		t.Errorf("body = %q, want the rendered body", r.body)
	}
	if r.header.Get("X-Order") != "ord_123" {
		t.Errorf("X-Order = %q, want ord_123", r.header.Get("X-Order"))
	}

	// The identity headers describe the task and must be present on every
	// delivery so the receiver can deduplicate.
	for header, want := range map[string]string{
		HeaderOrder:          "ord_123",
		HeaderStep:           "verify.stock",
		HeaderIdempotencyKey: "ord_123/verify.stock",
		HeaderDelivery:       "dlv-1",
		HeaderAttempt:        "2",
	} {
		if got := r.header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestInvokeIdempotencyKeyIsStableAcrossAttempts(t *testing.T) {
	keys := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get(HeaderIdempotencyKey)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL)
	inv := NewHTTPInvoker(srv.Client(), 1024)

	// Same task, two attempts: only the delivery id may change.
	in := sampleInput()
	if _, err := inv.Invoke(context.Background(), ep, in); err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
	in.Attempt = 3
	in.DeliveryID = "dlv-2"
	if _, err := inv.Invoke(context.Background(), ep, in); err != nil {
		t.Fatalf("second Invoke() error = %v", err)
	}

	first, second := <-keys, <-keys
	if first != second {
		t.Errorf("idempotency key changed across attempts: %q then %q", first, second)
	}
	if first != "ord_123/verify.stock" {
		t.Errorf("idempotency key = %q, want the stable task key", first)
	}
}

func TestInvokeRejectsUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("stock mismatch"))
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL)
	_, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, sampleInput())

	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Invoke() error = %v, want a *Failure", err)
	}
	if failure.Kind != FailureStatus {
		t.Errorf("Kind = %q, want %q", failure.Kind, FailureStatus)
	}
	if failure.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want 409", failure.StatusCode)
	}
	if !strings.Contains(failure.Snippet, "stock mismatch") {
		t.Errorf("Snippet = %q, want the response body", failure.Snippet)
	}
	if failure.Retryable() {
		t.Error("Retryable() = true for a 409, want false: the request itself is wrong")
	}
}

func TestInvokeRetryabilityByStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusMovedPermanently, false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := &Failure{Kind: FailureStatus, StatusCode: tc.status}
			if got := f.Retryable(); got != tc.want {
				t.Errorf("Retryable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInvokeHonoursExpectedStatusList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL, func(e *Endpoint) {
		e.ExpectStatus = []int{204}
	})
	if _, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, sampleInput()); err != nil {
		t.Fatalf("Invoke() error = %v, want 204 to be accepted", err)
	}

	ep = endpointFor(t, srv.URL, func(e *Endpoint) {
		e.ExpectStatus = []int{201}
	})
	if _, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, sampleInput()); err == nil {
		t.Fatal("Invoke() error = nil, want 204 to be rejected when only 201 is expected")
	}
}

func TestInvokeAppliesEndpointTimeout(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	// Release the handler before Close waits for it.
	defer srv.Close()
	defer close(released)

	ep := endpointFor(t, srv.URL, func(e *Endpoint) {
		e.Timeout = 60 * time.Millisecond
	})

	start := time.Now()
	_, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, sampleInput())
	elapsed := time.Since(start)

	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureTimeout {
		t.Fatalf("Invoke() error = %v, want a timeout failure", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Invoke() took %v, want it to honour the 60ms timeout", elapsed)
	}
}

func TestInvokeNetworkFailure(t *testing.T) {
	ep := endpointFor(t, "http://127.0.0.1:1/unreachable")

	_, err := NewHTTPInvoker(&http.Client{}, 1024).Invoke(context.Background(), ep, sampleInput())
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Invoke() error = %v, want a *Failure", err)
	}
	if failure.Kind != FailureNetwork {
		t.Errorf("Kind = %q, want %q", failure.Kind, FailureNetwork)
	}
	if !failure.Retryable() {
		t.Error("Retryable() = false for a network error, want true")
	}
}

func TestInvokeRenderFailureIsNotRetryable(t *testing.T) {
	ep := endpointFor(t, "http://127.0.0.1:1/h", func(e *Endpoint) {
		e.Body = `{{ .payload.does_not_exist }}`
	})

	_, err := NewHTTPInvoker(&http.Client{}, 1024).Invoke(context.Background(), ep, sampleInput())
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Invoke() error = %v, want a *Failure", err)
	}
	if failure.Kind != FailureRender {
		t.Errorf("Kind = %q, want %q", failure.Kind, FailureRender)
	}
	if failure.Retryable() {
		t.Error("Retryable() = true for a render error, want false: the same input fails again")
	}
}

func TestInvokeOmitsHeaderThatRendersEmpty(t *testing.T) {
	header := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present := r.Header["X-Api-Key"]
		if present {
			header <- r.Header.Get("X-Api-Key")
		} else {
			header <- ""
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL, func(e *Endpoint) {
		// An optional credential expressed as "omit when empty".
		e.Headers = []Header{{Name: "X-Api-Key", Value: "{{ .payload.api_key }}"}}
	})

	in := sampleInput()
	// The key exists but resolves to an empty string, which is how a caller
	// says "this deployment has no API key configured".
	in.Payload = map[string]any{"api_key": ""}
	if _, err := NewHTTPInvoker(srv.Client(), 1024).Invoke(context.Background(), ep, in); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	if got := <-header; got != "" {
		t.Errorf("X-Api-Key = %q, want the header omitted entirely", got)
	}
}

func TestInvokeConcurrentDeliveries(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get(HeaderIdempotencyKey)]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := endpointFor(t, srv.URL)
	inv := NewHTTPInvoker(srv.Client(), 1024)

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := inv.Invoke(context.Background(), ep, sampleInput()); err != nil {
				t.Errorf("Invoke() error = %v", err)
			}
		}()
	}
	wg.Wait()
	close(done)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Errorf("received %d distinct task keys, want 1", len(seen))
	}
}

func TestNewHTTPInvokerDefaults(t *testing.T) {
	inv := NewHTTPInvoker(nil, 0)
	if inv.Client == nil {
		t.Error("Client = nil, want a default client")
	}
	if inv.MaxResponseBytes != 64<<10 {
		t.Errorf("MaxResponseBytes = %d, want %d", inv.MaxResponseBytes, 64<<10)
	}
}
