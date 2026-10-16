package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingInvoker captures invocations instead of performing HTTP calls.
type recordingInvoker struct {
	mu       sync.Mutex
	calls    []string // callback ids, in completion order
	payloads [][]byte
	// failFor maps a callback id to the error Invoke should return.
	failFor map[string]error
	// delay is slept before returning, to exercise concurrency limits.
	delay time.Duration
}

func (r *recordingInvoker) Invoke(_ context.Context, cb Callback, _ string, payload []byte) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, cb.ID)
	r.payloads = append(r.payloads, payload)
	if err, ok := r.failFor[cb.ID]; ok {
		return err
	}
	return nil
}

func (r *recordingInvoker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

type failingRegistry struct{ err error }

func (f failingRegistry) Register(Callback) (Callback, error) { return Callback{}, nil }
func (f failingRegistry) List(string) ([]Callback, error)     { return nil, f.err }
func (f failingRegistry) Get(string) (Callback, error)        { return Callback{}, ErrNotFound }
func (f failingRegistry) Delete(string) error                 { return nil }

func TestDispatchInvokesEveryCallbackOfTheTopic(t *testing.T) {
	registry := NewMemoryRegistry()
	var created, paid []string
	for _, tc := range []struct{ topic, url string }{
		{"order.created", "https://a.example.com/1"},
		{"order.paid", "https://b.example.com/2"},
		{"order.created", "https://c.example.com/3"},
	} {
		cb, err := registry.Register(Callback{Topic: tc.topic, URL: tc.url})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if tc.topic == "order.created" {
			created = append(created, cb.ID)
		} else {
			paid = append(paid, cb.ID)
		}
	}

	inv := &recordingInvoker{failFor: map[string]error{}}
	d := NewDispatcher(registry, inv, time.Second, 4, discardLogger())

	payload := []byte(`{"id":"order-1"}`)
	res, err := d.Dispatch(context.Background(), "order.created", payload)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	if res.Topic != "order.created" {
		t.Errorf("Topic = %q, want order.created", res.Topic)
	}
	if res.DeliveryID == "" {
		t.Error("DeliveryID is empty, want a generated id")
	}
	if res.Delivered != 2 || res.Failed != 0 || res.Skipped != 0 {
		t.Errorf("counts = delivered:%d failed:%d skipped:%d, want 2/0/0", res.Delivered, res.Failed, res.Skipped)
	}
	if len(res.Deliveries) != 2 {
		t.Fatalf("len(Deliveries) = %d, want 2", len(res.Deliveries))
	}

	// Callbacks are invoked concurrently, so compare as sets rather than
	// assuming a completion order.
	inv.mu.Lock()
	gotIDs := make([]string, len(inv.calls))
	copy(gotIDs, inv.calls)
	gotPayloads := make(map[string]string, len(inv.payloads))
	for i, id := range inv.calls {
		gotPayloads[id] = string(inv.payloads[i])
	}
	inv.mu.Unlock()

	for _, want := range created {
		if !slices.Contains(gotIDs, want) {
			t.Errorf("callback %q of order.created was not invoked (invoked %v)", want, gotIDs)
		}
		if gotPayloads[want] != string(payload) {
			t.Errorf("payload for %q = %q, want %q", want, gotPayloads[want], payload)
		}
	}
	for _, id := range paid {
		if slices.Contains(gotIDs, id) {
			t.Errorf("callback %q of another topic was invoked", id)
		}
	}
}

func TestDispatchWithoutSubscribers(t *testing.T) {
	inv := &recordingInvoker{failFor: map[string]error{}}
	d := NewDispatcher(NewMemoryRegistry(), inv, time.Second, 4, discardLogger())

	res, err := d.Dispatch(context.Background(), "order.created", []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if res.Delivered != 0 || len(res.Deliveries) != 0 {
		t.Errorf("result = %+v, want no deliveries", res)
	}
	if inv.count() != 0 {
		t.Errorf("invoker was called %d times, want 0", inv.count())
	}
}

func TestDispatchReportsPerCallbackFailures(t *testing.T) {
	registry := NewMemoryRegistry()
	failing := callbackFor(t, registry, "order.created")
	succeeding := callbackFor(t, registry, "order.created")

	inv := &recordingInvoker{failFor: map[string]error{
		failing.ID: errors.New("connection refused"),
	}}
	d := NewDispatcher(registry, inv, time.Second, 4, discardLogger())

	res, err := d.Dispatch(context.Background(), "order.created", []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch() error = %v, want a per-callback failure not to fail the dispatch", err)
	}
	if res.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", res.Delivered)
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}

	byID := make(map[string]Delivery, len(res.Deliveries))
	for _, delivery := range res.Deliveries {
		byID[delivery.CallbackID] = delivery
	}
	if got := byID[failing.ID]; got.Status != StatusFailed || got.Error == "" {
		t.Errorf("failing delivery = %+v, want status %q with an error message", got, StatusFailed)
	}
	if got := byID[succeeding.ID]; got.Status != StatusDelivered || got.Error != "" {
		t.Errorf("successful delivery = %+v, want status %q and no error", got, StatusDelivered)
	}
	if got := byID[succeeding.ID].URL; got != succeeding.URL {
		t.Errorf("delivery URL = %q, want %q", got, succeeding.URL)
	}
}

func TestDispatchSurfacesRegistryErrors(t *testing.T) {
	wantErr := errors.New("registry is down")
	inv := &recordingInvoker{failFor: map[string]error{}}
	d := NewDispatcher(failingRegistry{err: wantErr}, inv, time.Second, 4, discardLogger())

	if _, err := d.Dispatch(context.Background(), "order.created", []byte(`{}`)); !errors.Is(err, wantErr) {
		t.Fatalf("Dispatch() error = %v, want it to wrap %v", err, wantErr)
	}
	if inv.count() != 0 {
		t.Errorf("invoker was called %d times, want 0", inv.count())
	}
}

func TestDispatchSkipsCallbacksAfterContextCancellation(t *testing.T) {
	registry := NewMemoryRegistry()
	for i := 0; i < 4; i++ {
		callbackFor(t, registry, "order.created")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inv := &recordingInvoker{failFor: map[string]error{}}
	d := NewDispatcher(registry, inv, time.Second, 1, discardLogger())

	res, err := d.Dispatch(ctx, "order.created", []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if res.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0 for a cancelled context", res.Delivered)
	}
	if res.Skipped != 4 {
		t.Errorf("Skipped = %d, want 4", res.Skipped)
	}
	if inv.count() != 0 {
		t.Errorf("invoker was called %d times, want 0", inv.count())
	}
}

func TestDispatchAppliesDefaultTimeout(t *testing.T) {
	registry := NewMemoryRegistry()
	withoutTimeout := callbackFor(t, registry, "order.created")

	explicit, err := registry.Register(Callback{
		Topic:   "order.created",
		URL:     "https://svc.example.com/slow",
		Timeout: 42 * time.Second,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	inv := &capturingInvoker{}
	d := NewDispatcher(registry, inv, 30*time.Millisecond, 2, discardLogger())

	if _, err := d.Dispatch(context.Background(), "order.created", []byte(`{}`)); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	byID := make(map[string]Callback, len(inv.seen))
	for _, cb := range inv.seen {
		byID[cb.ID] = cb
	}
	if got := byID[withoutTimeout.ID].Timeout; got != 30*time.Millisecond {
		t.Errorf("callback without a timeout got %v, want the dispatcher default 30ms", got)
	}
	if got := byID[explicit.ID].Timeout; got != 42*time.Second {
		t.Errorf("callback with its own timeout got %v, want it preserved at 42s", got)
	}
}

func TestDispatcherClampsMaxParallel(t *testing.T) {
	registry := NewMemoryRegistry()
	for i := 0; i < 6; i++ {
		callbackFor(t, registry, "order.created")
	}

	var inFlight, peak int64
	inv := &countingInvoker{onInvoke: func() {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
	}}

	d := NewDispatcher(registry, inv, time.Second, 2, discardLogger())
	if _, err := d.Dispatch(context.Background(), "order.created", []byte(`{}`)); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	if got := atomic.LoadInt64(&peak); got > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", got)
	}
}

func TestNewDispatcherDefaults(t *testing.T) {
	inv := &recordingInvoker{failFor: map[string]error{}}
	d := NewDispatcher(NewMemoryRegistry(), inv, time.Second, 0, nil)
	if d.maxParallel != 1 {
		t.Errorf("maxParallel = %d, want it clamped to 1", d.maxParallel)
	}
	if d.logger == nil {
		t.Error("logger = nil, want a default logger")
	}
}

func callbackFor(t *testing.T, registry Registry, topic string) Callback {
	t.Helper()
	cb, err := registry.Register(Callback{Topic: topic, URL: "https://svc.example.com/hooks"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return cb
}

// countingInvoker reports concurrency to onInvoke and always succeeds.
type countingInvoker struct{ onInvoke func() }

func (c *countingInvoker) Invoke(context.Context, Callback, string, []byte) error {
	c.onInvoke()
	return nil
}

// capturingInvoker records the Callback it was handed, which lets a test
// assert on values the dispatcher fills in before invoking.
type capturingInvoker struct {
	mu   sync.Mutex
	seen []Callback
}

func (c *capturingInvoker) Invoke(_ context.Context, cb Callback, _ string, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, cb)
	return nil
}
