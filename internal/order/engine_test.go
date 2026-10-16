package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vistart/rc_vistart/internal/notify"
)

// stubInvoker records deliveries and can be told to fail.
type stubInvoker struct {
	mu    sync.Mutex
	calls []notify.ContextInput
	// failFor maps a topic to the number of attempts that should fail.
	failRemaining map[string]int
	// failAlways maps a topic to an error returned every time.
	failAlways map[string]error
	delay      time.Duration
}

func (s *stubInvoker) Invoke(_ context.Context, ep notify.Endpoint, in notify.ContextInput) (notify.Result, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, in)

	if err, ok := s.failAlways[in.Topic]; ok {
		return notify.Result{}, &notify.Failure{Kind: notify.FailureStatus, StatusCode: 503, Err: err}
	}
	if remaining, ok := s.failRemaining[in.Topic]; ok && remaining > 0 {
		s.failRemaining[in.Topic] = remaining - 1
		return notify.Result{}, &notify.Failure{
			Kind:       notify.FailureNetwork,
			StatusCode: 0,
			Err:        errors.New("temporary network failure"),
		}
	}
	return notify.Result{StatusCode: 200, Latency: time.Millisecond}, nil
}

func (s *stubInvoker) countFor(topic string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, c := range s.calls {
		if c.Topic == topic {
			n++
		}
	}
	return n
}

func (s *stubInvoker) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	engine   *Engine
	registry *notify.MemoryRegistry
	invoker  *stubInvoker
	stats    *notify.Stats
}

func newHarness(t *testing.T, invoker *stubInvoker) *harness {
	t.Helper()

	registry := notify.NewMemoryRegistry()
	stats := notify.NewStats()
	engine := NewEngine(registry, invoker, stats, Config{
		Workers:      4,
		PollInterval: 2 * time.Millisecond,
		PublicURL:    "http://rc.test",
	}, quietLogger())
	engine.Start()
	t.Cleanup(engine.Stop)

	return &harness{engine: engine, registry: registry, invoker: invoker, stats: stats}
}

// register adds a working endpoint with a tiny backoff so retries are quick.
func (h *harness) register(t *testing.T, service, action string, maxAttempts int) {
	t.Helper()

	if _, err := h.registry.Register(notify.Endpoint{
		Service:      service,
		Action:       action,
		URL:          "http://" + service + ".test/" + action,
		Timeout:      time.Second,
		MaxAttempts:  maxAttempts,
		BackoffBase:  time.Millisecond,
		BackoffMax:   2 * time.Millisecond,
		ExpectStatus: []int{200, 204},
	}); err != nil {
		t.Fatalf("Register(%s.%s) error = %v", service, action, err)
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) stepState(t *testing.T, orderID, step string) StepState {
	t.Helper()

	o, err := h.engine.Get(orderID)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", orderID, err)
	}
	s, err := o.Step(step)
	if err != nil {
		t.Fatalf("Step(%q) error = %v", step, err)
	}
	return s.State
}

func singleStepSpec(kind StepKind, topic string) []StepSpec {
	return []StepSpec{{Name: "only", Phase: PhasePrepare, Kind: kind, Topic: topic}}
}

func TestSubmitDeliversNotifyStep(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "crm", "contact.update", 3)

	submitted, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "crm.contact.update")})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if submitted.State == OrderSucceeded {
		t.Error("State = succeeded immediately, want the delivery to still be outstanding")
	}

	waitFor(t, "the step to succeed", func() bool {
		return h.stepState(t, "ord_1", "only") == StateSucceeded
	})

	o, err := h.engine.Get("ord_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if o.State != OrderSucceeded {
		t.Errorf("State = %q, want succeeded", o.State)
	}
	if o.CompletedAt == nil {
		t.Error("CompletedAt is nil on a finished order")
	}
}

func TestSubmitRejectsDuplicateOrderID(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "crm", "contact.update", 3)

	spec := Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "crm.contact.update")}
	if _, err := h.engine.Submit(spec); err != nil {
		t.Fatalf("first Submit() error = %v", err)
	}
	if _, err := h.engine.Submit(spec); !errors.Is(err, ErrConflict) {
		t.Errorf("second Submit() error = %v, want ErrConflict", err)
	}
}

func TestSubmitGeneratesOrderID(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "crm", "contact.update", 3)

	o, err := h.engine.Submit(Spec{Steps: singleStepSpec(KindNotify, "crm.contact.update")})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if o.ID == "" {
		t.Fatal("Submit() left the order without an id")
	}

	if _, err := h.engine.Get(o.ID); err != nil {
		t.Errorf("Get(generated id) error = %v", err)
	}
}

func TestRetryUntilAcknowledged(t *testing.T) {
	inv := &stubInvoker{failRemaining: map[string]int{"svc.act": 2}}
	h := newHarness(t, inv)
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitFor(t, "the step to succeed after retries", func() bool {
		return h.stepState(t, "ord_1", "only") == StateSucceeded
	})

	if got := inv.countFor("svc.act"); got != 3 {
		t.Errorf("attempts = %d, want 3 (two failures then a success)", got)
	}

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	if s.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", s.Attempts)
	}

	// The ledger answers "did we send it, and how often": two failures and one
	// success, with no double counting.
	var failed, delivered int
	for _, d := range s.Deliveries {
		switch d.Outcome {
		case "failed":
			failed++
		case "delivered":
			delivered++
		}
	}
	if failed != 2 {
		t.Errorf("ledger has %d failed entries, want 2", failed)
	}
	if delivered != 1 {
		t.Errorf("ledger has %d delivered entries, want 1", delivered)
	}
}

func TestIdempotencyKeyIsStableAcrossRetries(t *testing.T) {
	inv := &stubInvoker{failRemaining: map[string]int{"svc.act": 2}}
	h := newHarness(t, inv)
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to succeed", func() bool {
		return h.stepState(t, "ord_1", "only") == StateSucceeded
	})

	inv.mu.Lock()
	defer inv.mu.Unlock()

	// The whole anti-duplication contract rests on this: every attempt for one
	// task carries the same key while the delivery id differs.
	want := "ord_1/only"
	for i, call := range inv.calls {
		if call.IdempotencyKey != want {
			t.Errorf("call %d IdempotencyKey = %q, want %q", i, call.IdempotencyKey, want)
		}
		if call.DeliveryID == "" {
			t.Errorf("call %d has no DeliveryID", i)
		}
	}
}

func TestEscalatesAfterBudgetExhausted(t *testing.T) {
	inv := &stubInvoker{failAlways: map[string]error{"svc.act": errors.New("downstream is down")}}
	h := newHarness(t, inv)
	h.register(t, "svc", "act", 3)

	var escalated atomic.Int32
	var mu sync.Mutex
	var seen []Escalation
	h.engine.OnEscalate(func(esc Escalation) {
		escalated.Add(1)
		mu.Lock()
		seen = append(seen, esc)
		mu.Unlock()
	})

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitFor(t, "the step to escalate", func() bool {
		return h.stepState(t, "ord_1", "only") == StateEscalated
	})

	if got := inv.countFor("svc.act"); got != 3 {
		t.Errorf("attempts = %d, want exactly the budget of 3", got)
	}
	if escalated.Load() != 1 {
		t.Errorf("escalation hook fired %d times, want 1", escalated.Load())
	}

	mu.Lock()
	esc := seen[0]
	mu.Unlock()
	if esc.TaskKey != "ord_1/only" {
		t.Errorf("escalated TaskKey = %q, want ord_1/only", esc.TaskKey)
	}
	if esc.OrderID != "ord_1" {
		t.Errorf("escalated OrderID = %q, want ord_1", esc.OrderID)
	}

	// The order must fail rather than sit in a half-delivered state.
	waitFor(t, "the order to fail", func() bool {
		o, _ := h.engine.Get("ord_1")
		return o.State == OrderFailed
	})

	// Escalations are queryable, which is the operator's work queue.
	if got := len(h.engine.Escalations()); got != 1 {
		t.Errorf("len(Escalations()) = %d, want 1", got)
	}
}

func TestPermanentFailureDoesNotBurnTheBudget(t *testing.T) {
	// A render failure means the request can never be built, so the task must
	// escalate after one attempt rather than exhausting the retry budget.
	inv := &stubUnrenderableInvoker{}
	registry := notify.NewMemoryRegistry()
	engine := NewEngine(registry, inv, notify.NewStats(), Config{
		Workers:      2,
		PollInterval: 2 * time.Millisecond,
		PublicURL:    "http://rc.test",
	}, quietLogger())
	engine.Start()
	t.Cleanup(engine.Stop)

	if _, err := registry.Register(notify.Endpoint{
		Service:     "svc",
		Action:      "act",
		URL:         "http://svc.test/act",
		MaxAttempts: 5,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if o, err := engine.Get("ord_1"); err == nil {
			if s, err := o.Step("only"); err == nil && s.State == StateEscalated {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}

	o, err := engine.Get("ord_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	s, _ := o.Step("only")
	if s.State != StateEscalated {
		t.Fatalf("state = %q, want escalated", s.State)
	}
	if got := inv.count(); got != 1 {
		t.Errorf("deliveries = %d, want exactly 1 for a permanent failure", got)
	}
}

// stubUnrenderableInvoker always reports a render failure, which is the
// permanent-failure class.
type stubUnrenderableInvoker struct {
	mu    sync.Mutex
	calls int
}

func (s *stubUnrenderableInvoker) Invoke(context.Context, notify.Endpoint, notify.ContextInput) (notify.Result, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return notify.Result{}, &notify.Failure{Kind: notify.FailureRender, Err: errors.New("bad template")}
}

func (s *stubUnrenderableInvoker) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestUnregisteredTopicEscalates(t *testing.T) {
	h := newHarness(t, &stubInvoker{})

	// Nobody registered svc.act, so the task cannot be delivered.
	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to escalate", func() bool {
		return h.stepState(t, "ord_1", "only") == StateEscalated
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	if s.LastError == "" {
		t.Error("LastError is empty, want it to explain the missing endpoint")
	}
}

func TestCallbackStepWaitsForResult(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "inventory", "stock.prehold", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// An acknowledged delivery is not yet a finished step.
	waitFor(t, "the step to wait for a result", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	if s.PendingDeliveryID == "" {
		t.Error("PendingDeliveryID is empty while waiting")
	}
	if s.ResultDeadline == nil {
		t.Error("ResultDeadline is nil while waiting")
	}
	if o.State == OrderSucceeded {
		t.Error("the order completed before the result arrived")
	}
}

func TestCallbackStepCompletesOnResult(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "inventory", "stock.prehold", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to wait", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	deliveryID := s.PendingDeliveryID

	updated, err := h.engine.SubmitResult("ord_1", "only", deliveryID, true, json.RawMessage(`{"reserved":3}`))
	if err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}
	if updated.State != OrderSucceeded {
		t.Errorf("State = %q, want succeeded", updated.State)
	}

	done, _ := updated.Step("only")
	if string(done.Result) != `{"reserved":3}` {
		t.Errorf("Result = %s, want the receiver's report", done.Result)
	}
	if done.PendingDeliveryID != "" {
		t.Error("PendingDeliveryID is still set after completion")
	}
}

func TestDuplicateResultIsIgnored(t *testing.T) {
	inv := &stubInvoker{}
	h := newHarness(t, inv)
	h.register(t, "inventory", "stock.prehold", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to wait", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	deliveryID := s.PendingDeliveryID

	if _, err := h.engine.SubmitResult("ord_1", "only", deliveryID, true, nil); err != nil {
		t.Fatalf("first SubmitResult() error = %v", err)
	}

	// The receiver retries its own answer. That must be harmless, not an error
	// and not a second transition.
	if _, err := h.engine.SubmitResult("ord_1", "only", deliveryID, true, nil); err != nil {
		t.Fatalf("duplicate SubmitResult() error = %v, want it tolerated", err)
	}

	if got := inv.total(); got != 1 {
		t.Errorf("deliveries = %d, want 1: a duplicate result must not re-deliver", got)
	}
	if got := h.stepState(t, "ord_1", "only"); got != StateSucceeded {
		t.Errorf("state = %q, want it still succeeded", got)
	}
}

func TestResultForWrongDeliveryIsRejected(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "inventory", "stock.prehold", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to wait", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	if _, err := h.engine.SubmitResult("ord_1", "only", "not-the-delivery", true, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("SubmitResult() error = %v, want ErrConflict", err)
	}
}

func TestNegativeResultEscalatesWithoutRetrying(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "inventory", "stock.prehold", 5)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to wait", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	_, err := h.engine.SubmitResult("ord_1", "only", s.PendingDeliveryID, false, json.RawMessage(`{"reason":"out of stock"}`))
	if err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}

	// "No stock" is a business answer, not a transport failure: repeating the
	// notification cannot change it.
	if got := h.stepState(t, "ord_1", "only"); got != StateEscalated {
		t.Errorf("state = %q, want escalated", got)
	}
	if got := h.invoker.total(); got != 1 {
		t.Errorf("deliveries = %d, want 1", got)
	}
}

func TestResultForUnknownOrderOrStep(t *testing.T) {
	h := newHarness(t, &stubInvoker{})

	if _, err := h.engine.SubmitResult("ghost", "only", "d", true, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SubmitResult(unknown order) error = %v, want ErrNotFound", err)
	}

	h.register(t, "svc", "act", 3)
	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := h.engine.SubmitResult("ord_1", "ghost", "d", true, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SubmitResult(unknown step) error = %v, want ErrNotFound", err)
	}
}

func TestMissingResultIsRetriedThenEscalated(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	// A short result timeout keeps the test quick.
	if _, err := h.registry.Register(notify.Endpoint{
		Service:       "inventory",
		Action:        "stock.prehold",
		URL:           "http://inv.test/prehold",
		MaxAttempts:   2,
		ResultTimeout: 40 * time.Millisecond,
		BackoffBase:   time.Millisecond,
		BackoffMax:    2 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "inventory.stock.prehold")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The receiver acknowledged but never reported a result, which is the
	// "we never heard back" case: it must be retried, then escalated.
	waitFor(t, "the silent step to escalate", func() bool {
		return h.stepState(t, "ord_1", "only") == StateEscalated
	})

	if got := h.invoker.countFor("inventory.stock.prehold"); got != 2 {
		t.Errorf("deliveries = %d, want 2 (one delivery, one retry after the missed deadline)", got)
	}

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	if s.LastError == "" {
		t.Error("LastError is empty, want it to name the missed deadline")
	}
}

func TestStandardOrderRunsPhasesInOrder(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	for _, ep := range []struct{ service, action string }{
		{"identity", "verify"},
		{"inventory", "stock.prehold"},
		{"payment", "verify"},
		{"fulfilment", "delivery"},
		{"inventory", "stock.outbound"},
	} {
		h.register(t, ep.service, ep.action, 3)
	}

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Preset: PresetOrder, Steps: StandardSteps()}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The preset chains the checks deliberately: identity before reserving
	// stock, and the reservation before confirming payment, so an unpaid order
	// never consumes a reservation and a paid one is never taken on trust.
	// Only the first check runs.
	waitFor(t, "the identity check", func() bool {
		return h.invoker.countFor("identity.verify") == 1
	})
	if got := h.invoker.countFor("inventory.stock.prehold"); got != 0 {
		t.Errorf("stock deliveries = %d, want 0 before identity is confirmed", got)
	}

	answerSteps(t, h, "ord_1", "verify.identity", "identity.verify")
	waitFor(t, "the stock check", func() bool {
		return h.invoker.countFor("inventory.stock.prehold") == 1
	})
	if got := h.invoker.countFor("payment.verify"); got != 0 {
		t.Errorf("payment deliveries = %d, want 0 before stock is reserved", got)
	}

	answerSteps(t, h, "ord_1", "verify.stock", "inventory.stock.prehold")
	waitFor(t, "the payment check", func() bool {
		return h.invoker.countFor("payment.verify") == 1
	})

	answerSteps(t, h, "ord_1", "verify.payment", "payment.verify")

	waitFor(t, "the order to reach produce", func() bool {
		o, _ := h.engine.Get("ord_1")
		return o.Phase == PhaseProduce
	})
	waitFor(t, "the order to succeed", func() bool {
		o, _ := h.engine.Get("ord_1")
		return o.State == OrderSucceeded
	})

	// Stock was reserved during prepare and handed over during produce.
	if got := h.invoker.countFor("inventory.stock.outbound"); got != 1 {
		t.Errorf("outbound deliveries = %d, want exactly 1", got)
	}
}

// answerSteps submits a successful result for every waiting step of a topic.
func answerSteps(t *testing.T, h *harness, orderID, stepName, topic string) {
	t.Helper()

	waitFor(t, "the step to await a result", func() bool {
		return h.stepState(t, orderID, stepName) == StateWaiting
	})
	o, err := h.engine.Get(orderID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	s, err := o.Step(stepName)
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if _, err := h.engine.SubmitResult(orderID, stepName, s.PendingDeliveryID, true, nil); err != nil {
		t.Fatalf("SubmitResult(%s) error = %v", stepName, err)
	}
}

func TestFailedPrepareStepSkipsTheRest(t *testing.T) {
	inv := &stubInvoker{failAlways: map[string]error{"identity.verify": errors.New("unavailable")}}
	h := newHarness(t, inv)
	for _, ep := range []struct{ service, action string }{
		{"identity", "verify"},
		{"inventory", "stock.prehold"},
	} {
		h.register(t, ep.service, ep.action, 2)
	}

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Preset: PresetOrder, Steps: StandardSteps()}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitFor(t, "the order to fail", func() bool {
		o, _ := h.engine.Get("ord_1")
		return o.State == OrderFailed
	})

	o, _ := h.engine.Get("ord_1")
	stock, _ := o.Step(StepVerifyStock)
	if stock.State != StateSkipped {
		t.Errorf("stock state = %q, want skipped: its dependency failed", stock.State)
	}
	// Fulfilment never runs, because the order never reached produce.
	if got := inv.countFor("fulfilment.delivery"); got != 0 {
		t.Errorf("fulfilment deliveries = %d, want 0", got)
	}
	if o.Phase != PhasePrepare {
		t.Errorf("Phase = %q, want prepare", o.Phase)
	}
}

// eagerInvoker posts the step result before returning the HTTP response, which
// is legal and is what a fast receiver does. It reproduces the interleaving
// where the result beats the response.
type eagerInvoker struct {
	engine  *Engine
	orderID string
	step    string
}

func (e *eagerInvoker) Invoke(_ context.Context, _ notify.Endpoint, in notify.ContextInput) (notify.Result, error) {
	if _, err := e.engine.SubmitResult(e.orderID, e.step, in.DeliveryID, true, nil); err != nil {
		return notify.Result{}, &notify.Failure{Kind: notify.FailureNetwork, Err: err}
	}
	return notify.Result{StatusCode: 200}, nil
}

func TestResultArrivingBeforeTheResponseWins(t *testing.T) {
	// If the response were applied after the result, the step would be dragged
	// back into waiting, never complete, and be retried once its deadline
	// passed. The result is the stronger signal, so it must win.
	registry := notify.NewMemoryRegistry()
	// The invoker is created before the engine and back-references it, then is
	// completed before any worker starts. Assigning engine.invoker after Start
	// would itself be a race with the workers reading it.
	inv := &eagerInvoker{orderID: "ord_eager", step: "reserve"}
	engine := NewEngine(registry, inv, notify.NewStats(), Config{
		Workers:      2,
		PollInterval: 5 * time.Millisecond,
		PublicURL:    "http://rc.test",
	}, quietLogger())
	inv.engine = engine
	engine.Start()
	defer engine.Stop()

	if _, err := registry.Register(notify.Endpoint{
		Service:       "inventory",
		Action:        "stock.prehold",
		URL:           "http://inv.test/prehold",
		MaxAttempts:   3,
		ResultTimeout: 200 * time.Millisecond,
		BackoffBase:   time.Millisecond,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := engine.Submit(Spec{ID: "ord_eager", Steps: []StepSpec{{
		Name:  "reserve",
		Phase: PhasePrepare,
		Kind:  KindCallback,
		Topic: "inventory.stock.prehold",
	}}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if o, err := engine.Get("ord_eager"); err == nil && o.State.Terminal() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	o, err := engine.Get("ord_eager")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	s, err := o.Step("reserve")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	if s.State != StateSucceeded {
		t.Errorf("step state = %q, want succeeded: the result must not be undone by the response", s.State)
	}
	if s.PendingDeliveryID != "" {
		t.Errorf("PendingDeliveryID = %q, want it cleared once the result was applied", s.PendingDeliveryID)
	}
	if s.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a completed step must not be retried", s.Attempts)
	}
	if o.State != OrderSucceeded {
		t.Errorf("order state = %q, want succeeded", o.State)
	}
	if len(engine.Escalations()) != 0 {
		t.Errorf("escalations = %d, want 0", len(engine.Escalations()))
	}
}

func TestResultBeforeResponseStillRecordsTheAttempt(t *testing.T) {
	// The audit trail must account for every attempt, whichever of the two
	// signals arrives first. When the result wins the race, the ledger entry
	// has to be written by the result path.
	h := newHarness(t, &stubInvoker{})
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: []StepSpec{{
		Name:  "only",
		Phase: PhasePrepare,
		Kind:  KindCallback,
		Topic: "svc.act",
	}}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the step to await a result", func() bool {
		return h.stepState(t, "ord_1", "only") == StateWaiting
	})

	o, _ := h.engine.Get("ord_1")
	s, _ := o.Step("only")
	deliveryID := s.PendingDeliveryID

	// The ordinary ordering: the response has already been recorded.
	if len(s.Deliveries) != 1 {
		t.Fatalf("ledger has %d entries before the result, want 1", len(s.Deliveries))
	}

	if _, err := h.engine.SubmitResult("ord_1", "only", deliveryID, true, nil); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}

	after, _ := h.engine.Get("ord_1")
	done, _ := after.Step("only")
	if len(done.Deliveries) != 1 {
		t.Errorf("ledger has %d entries, want 1: a result must not double-record its delivery", len(done.Deliveries))
	}

	// The reversed ordering: the result arrives before the response is applied.
	registry := notify.NewMemoryRegistry()
	inv := &eagerInvoker{orderID: "ord_2", step: "only"}
	engine := NewEngine(registry, inv, notify.NewStats(), Config{
		Workers:      2,
		PollInterval: 5 * time.Millisecond,
		PublicURL:    "http://rc.test",
	}, quietLogger())
	inv.engine = engine
	engine.Start()
	defer engine.Stop()

	if _, err := registry.Register(notify.Endpoint{
		Service:     "svc",
		Action:      "act",
		URL:         "http://svc.test/act",
		MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := engine.Submit(Spec{ID: "ord_2", Steps: []StepSpec{{
		Name:  "only",
		Phase: PhasePrepare,
		Kind:  KindCallback,
		Topic: "svc.act",
	}}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the eager order to finish", func() bool {
		o, err := engine.Get("ord_2")
		if err != nil {
			return false
		}
		s, err := o.Step("only")
		return err == nil && s.State == StateSucceeded
	})

	eager, _ := engine.Get("ord_2")
	step, _ := eager.Step("only")
	if len(step.Deliveries) != 1 {
		t.Fatalf("ledger has %d entries, want 1 recorded by the result path", len(step.Deliveries))
	}
	if !step.Deliveries[0].FromResult {
		t.Error("the ledger entry should be marked as coming from the result")
	}
	if step.Deliveries[0].DeliveryID == "" {
		t.Error("the ledger entry has no delivery id")
	}
}

func TestSnapshotLookupIsIsolatedFromTheEngine(t *testing.T) {
	// A snapshot must be a real snapshot. Reaching it through Step() used to
	// hand back a pointer into the engine's live step, so a caller could read
	// or write engine state with the lock released. That is a data race, and
	// this catches it without needing the race detector.
	h := newHarness(t, &stubInvoker{})
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	snapshot, err := h.engine.Get("ord_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	viaLookup, err := snapshot.Step("only")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	viaSlice, err := snapshot.Step("only")
	if err != nil {
		t.Fatalf("second Step() error = %v", err)
	}
	if viaLookup != viaSlice {
		t.Error("Step() returned different steps on the same snapshot")
	}

	// Mutating what the lookup handed back must not reach the engine.
	viaLookup.State = StateEscalated
	viaLookup.Attempts = 99

	fresh, err := h.engine.Get("ord_1")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	if fresh.Steps[0].State == StateEscalated {
		t.Error("mutating a snapshot step reached the engine's live state")
	}
	if fresh.Steps[0].Attempts == 99 {
		t.Error("mutating a snapshot step's attempt count reached the engine")
	}
}

func TestListAndGetReturnCopies(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindNotify, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	o, err := h.engine.Get("ord_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	o.Steps[0].State = StateSucceeded

	// Mutating a snapshot must not reach the engine's own state.
	waitFor(t, "the real state to settle", func() bool {
		got, _ := h.engine.Get("ord_1")
		s, _ := got.Step("only")
		return s.State == StateSucceeded
	})

	if _, err := h.engine.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if got := len(h.engine.List()); got != 1 {
		t.Errorf("len(List()) = %d, want 1", got)
	}
}

func TestResultURLIsWellFormed(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "svc", "act", 3)

	if _, err := h.engine.Submit(Spec{ID: "ord_1", Steps: singleStepSpec(KindCallback, "svc.act")}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, "the delivery", func() bool {
		return h.invoker.total() >= 1
	})

	h.invoker.mu.Lock()
	defer h.invoker.mu.Unlock()
	// The receiver can only report a result because it was told where to send
	// it, so this URL is part of the delivery contract.
	want := "http://rc.test/api/v1/orders/ord_1/steps/only/result"
	if got := h.invoker.calls[0].ResultURL; got != want {
		t.Errorf("ResultURL = %q, want %q", got, want)
	}
}

func TestConcurrentSubmissions(t *testing.T) {
	h := newHarness(t, &stubInvoker{})
	h.register(t, "svc", "act", 3)

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.engine.Submit(Spec{
				ID:    fmt.Sprintf("ord_%d", i),
				Steps: singleStepSpec(KindNotify, "svc.act"),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("Submit() error = %v", err)
		}
	}
	if got := len(h.engine.List()); got != n {
		t.Errorf("len(List()) = %d, want %d", got, n)
	}

	waitFor(t, "every order to finish", func() bool {
		orders := h.engine.List()
		for _, o := range orders {
			if !o.State.Terminal() {
				return false
			}
		}
		return len(orders) == n
	})
}
