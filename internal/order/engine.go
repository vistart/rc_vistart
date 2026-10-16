package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vistart/rc_vistart/internal/notify"
)

// Config tunes the engine.
type Config struct {
	// Workers is the number of concurrent delivery attempts.
	Workers int
	// PollInterval is how often an idle worker re-checks the queue.
	PollInterval time.Duration
	// PublicURL is this service's externally reachable base URL. It is handed
	// to receivers as .result_url so they know where to post a result.
	PublicURL string
}

// Escalation describes a task that exhausted its attempts.
type Escalation struct {
	OrderID  string
	TaskKey  string
	Step     string
	Topic    string
	Attempts int
	Err      error
	At       time.Time
}

// Engine drives orders: it schedules deliveries, applies outcomes, retries with
// backoff and escalates what will not succeed.
//
// The guarantee it implements is at-least-once delivery per task, never
// at-most-once: a task key is retried until it is acknowledged, and because a
// retry is indistinguishable from the first attempt to the receiver unless the
// receiver deduplicates, every attempt carries the same task key. Losing a
// notification is worse than repeating one.
type Engine struct {
	registry notify.Registry
	stats    *notify.Stats
	queue    *notify.Queue
	invoker  notify.Invoker
	logger   *slog.Logger
	cfg      Config

	mu     sync.Mutex
	orders map[string]*Order

	escalations []Escalation
	onEscalate  func(Escalation)

	now   func() time.Time
	newID func() string

	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// NewEngine wires an engine. It does not start any goroutine; call Start.
func NewEngine(registry notify.Registry, invoker notify.Invoker, stats *notify.Stats, cfg Config, logger *slog.Logger) *Engine {
	if cfg.Workers < 1 {
		cfg.Workers = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	cfg.PublicURL = trimSlash(cfg.PublicURL)
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		registry: registry,
		invoker:  invoker,
		stats:    stats,
		queue:    notify.NewQueue(),
		logger:   logger,
		cfg:      cfg,
		orders:   make(map[string]*Order),
		now:      time.Now,
		newID:    notify.NewID,
		stopped:  make(chan struct{}),
	}
}

// OnEscalate registers a hook invoked for every escalation, which is where a
// real deployment would page an operator.
func (e *Engine) OnEscalate(fn func(Escalation)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onEscalate = fn
}

// Start launches the workers.
func (e *Engine) Start() {
	for i := 0; i < e.cfg.Workers; i++ {
		e.wg.Add(1)
		go e.worker()
	}
}

// Stop shuts the workers down and waits for in-flight attempts to finish.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		close(e.stopped)
		e.queue.Close()
	})
	e.wg.Wait()
}

// SetPublicURL sets the externally reachable base URL handed to receivers as
// .result_url. A service that learns its address only after binding must set
// this before it starts accepting deliveries.
func (e *Engine) SetPublicURL(url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.PublicURL = trimSlash(url)
}

// ResultURL is where a receiver posts the result of a callback step.
func (e *Engine) ResultURL(orderID, stepName string) string {
	return fmt.Sprintf("%s/api/v1/orders/%s/steps/%s/result", e.cfg.PublicURL, orderID, stepName)
}

// Submit creates an order and schedules whatever is immediately runnable.
// It returns the stored order.
func (e *Engine) Submit(spec Spec) (*Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now().UTC()
	o, err := buildOrder(spec, now, "ord_"+e.newID())
	if err != nil {
		return nil, err
	}
	if _, exists := e.orders[o.ID]; exists {
		return nil, fmt.Errorf("%w: order %q already exists", ErrConflict, o.ID)
	}

	e.orders[o.ID] = o
	e.logger.Info("order accepted",
		slog.String("order_id", o.ID),
		slog.String("preset", o.Preset),
		slog.Int("steps", len(o.Steps)),
	)

	e.advanceLocked(o)
	return e.snapshotLocked(o), nil
}

// Spec describes an order to submit.
type Spec struct {
	ID          string
	Preset      string
	ExternalRef string
	Subject     string
	Items       []notify.Item
	Payload     map[string]any
	Steps       []StepSpec
}

// Get returns a copy of an order.
func (e *Engine) Get(id string) (Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	o, ok := e.orders[id]
	if !ok {
		return Order{}, fmt.Errorf("%w: order %q", ErrNotFound, id)
	}
	return *e.snapshotLocked(o), nil
}

// List returns copies of the orders, newest first.
func (e *Engine) List() []Order {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Order, 0, len(e.orders))
	for _, o := range e.orders {
		out = append(out, *e.snapshotLocked(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Escalations returns the recorded escalations, newest first.
func (e *Engine) Escalations() []Escalation {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Escalation, len(e.escalations))
	copy(out, e.escalations)
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// SubmitResult applies a receiver's report for a callback step.
//
// It is idempotent by delivery id: a receiver that posts twice, or retries its
// own result call, gets the same answer instead of a second transition.
func (e *Engine) SubmitResult(orderID, stepName, deliveryID string, success bool, data json.RawMessage) (Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	o, ok := e.orders[orderID]
	if !ok {
		return Order{}, fmt.Errorf("%w: order %q", ErrNotFound, orderID)
	}
	s, ok := o.byName[stepName]
	if !ok {
		return Order{}, fmt.Errorf("%w: step %q of order %q", ErrNotFound, stepName, orderID)
	}

	// A result for a step that already finished is a duplicate, not a conflict:
	// the receiver is entitled to retry, and re-applying must not corrupt the
	// ledger or re-trigger anything downstream.
	if s.State.Terminal() {
		e.logger.Info("duplicate step result ignored",
			slog.String("order_id", orderID),
			slog.String("step", stepName),
			slog.String("state", string(s.State)),
		)
		return *e.snapshotLocked(o), nil
	}
	if deliveryID == "" || deliveryID != s.PendingDeliveryID {
		return Order{}, fmt.Errorf("%w: delivery %q does not match the awaited delivery for %s/%s", ErrConflict, deliveryID, orderID, stepName)
	}

	now := e.now().UTC()
	s.ResultDeadline = nil
	s.Result = data
	s.UpdatedAt = now

	if success {
		s.State = StateSucceeded
		s.FinishedAt = &now
		s.LastError = ""
		s.PendingDeliveryID = ""

		// A receiver may answer before its HTTP response is applied, in which
		// case this result is what completed the step. Record the attempt here
		// so the ledger still accounts for it; execute then discards the
		// redundant response. Without this, an attempt could succeed and leave
		// no trace in the audit trail.
		if !s.hasDelivery(deliveryID) {
			s.Deliveries = append(s.Deliveries, Delivery{
				Attempt:    s.Attempts,
				DeliveryID: deliveryID,
				At:         now,
				Outcome:    "delivered",
				FromResult: true,
			})
			s.trimLedger()
			e.stats.RecordDelivered(e.metaFor(s.Spec.Topic), 0)
		}

		e.logger.Info("step succeeded",
			slog.String("order_id", orderID),
			slog.String("step", stepName),
			slog.String("task_key", s.TaskKey),
		)
	} else {
		// The receiver answered, but with a negative result. Retrying a
		// business rejection would only repeat a decision, so this escalates
		// immediately rather than burning the attempt budget.
		err := errors.New("receiver reported failure")
		if len(data) > 0 {
			err = fmt.Errorf("receiver reported failure: %s", truncate(data, 200))
		}
		e.recordFailureLocked(o, s, Delivery{DeliveryID: deliveryID}, err, false)
		s.PendingDeliveryID = ""
	}

	e.advanceLocked(o)
	return *e.snapshotLocked(o), nil
}

// maxAdvanceRounds bounds the phase-propagation loop. There are two phases, so
// this only guards against a future phase looping back.
const maxAdvanceRounds = 8

// advanceLocked schedules everything that became runnable and settles the
// order's state. The caller must hold e.mu.
func (e *Engine) advanceLocked(o *Order) {
	// Advancing from prepare to produce makes a new set of steps runnable, so
	// the pass repeats until nothing more changes.
	for round := 0; round < maxAdvanceRounds; round++ {
		if !e.advanceOnceLocked(o) {
			return
		}
	}
	e.logger.Error("order did not settle, giving up on advancing",
		slog.String("order_id", o.ID),
		slog.String("phase", string(o.Phase)),
	)
}

// advanceOnceLocked runs a single scheduling pass and reports whether the
// order moved to a new phase, which is the only reason to run it again.
func (e *Engine) advanceOnceLocked(o *Order) bool {
	now := e.now().UTC()

	// A step whose dependency failed can never run; skipping it is what stops
	// a half-broken graph from waiting forever.
	for _, s := range o.Blocked() {
		s.State = StateSkipped
		s.LastError = "skipped: a dependency did not succeed"
		s.FinishedAt = &now
		s.UpdatedAt = now
		e.logger.Warn("step skipped",
			slog.String("order_id", o.ID),
			slog.String("step", s.Spec.Name),
		)
	}

	for _, s := range o.Ready() {
		if _, err := e.registry.ByTopic(s.Spec.Topic); err != nil {
			// Nobody has registered for this topic. That is exactly the case
			// a prototype must not spin on forever: burn one attempt and let
			// the normal budget escalate it.
			s.Attempts++
			e.recordFailureLocked(o, s, Delivery{}, fmt.Errorf("no endpoint registered for topic %q", s.Spec.Topic), true)
			continue
		}

		// Attempts comes from the task, not from an increment here, so the
		// count stays correct for retries queued by recordFailureLocked.
		s.Attempts = 1
		s.State = StateQueued
		s.UpdatedAt = now
		if s.StartedAt == nil {
			s.StartedAt = &now
		}

		e.queue.Push(notify.Task{
			Key:        s.TaskKey,
			OrderID:    o.ID,
			StepName:   s.Spec.Name,
			Topic:      s.Spec.Topic,
			Attempt:    s.Attempts,
			DeliveryID: e.newID(),
			NotBefore:  now,
		})
		e.logger.Info("delivery scheduled",
			slog.String("task_key", s.TaskKey),
			slog.String("topic", s.Spec.Topic),
			slog.Int("attempt", s.Attempts),
		)
	}

	return e.settleLocked(o, now)
}

// settleLocked computes the order and phase state once nothing more can be
// scheduled right now, reporting whether the order moved to a new phase.
func (e *Engine) settleLocked(o *Order, now time.Time) bool {
	o.UpdatedAt = now
	if o.State.Terminal() {
		return false
	}

	// Walk only the phases this order actually has, starting from the current
	// one: the order may already be past prepare. A single-phase order is
	// finished once that phase is done and must not wait for a produce phase
	// it does not have.
	phases := o.Phases()
	start := 0
	for start < len(phases) && phases[start] != o.Phase {
		start++
	}

	for i := start; i < len(phases); i++ {
		phase := phases[i]

		complete, ok := o.PhaseComplete(phase)
		if !complete {
			o.State = OrderRunning
			return false
		}
		if !ok {
			o.State = OrderFailed
			o.CompletedAt = &now
			e.logger.Error("order failed",
				slog.String("order_id", o.ID),
				slog.String("phase", string(phase)),
				slog.Any("counts", o.Counts()),
			)
			return false
		}

		if i+1 < len(phases) {
			o.Phase = phases[i+1]
			e.logger.Info("order advanced to the next phase",
				slog.String("order_id", o.ID),
				slog.String("phase", string(o.Phase)),
			)
			// Another pass will schedule this phase's steps.
			return true
		}
	}

	o.State = OrderSucceeded
	o.CompletedAt = &now
	e.logger.Info("order completed",
		slog.String("order_id", o.ID),
		slog.Any("counts", o.Counts()),
	)
	return false
}

// recordFailureLocked applies a failed attempt: it records the attempt in the
// step ledger, then either retries with backoff or escalates. The ledger is
// written here and only here, so one attempt can never be counted twice. The
// caller must hold e.mu.
func (e *Engine) recordFailureLocked(o *Order, s *Step, detail Delivery, err error, retryable bool) {
	now := e.now().UTC()
	s.UpdatedAt = now
	s.LastError = err.Error()

	detail.At = now
	detail.Outcome = "failed"
	detail.Error = err.Error()
	detail.Retryable = retryable
	if detail.Attempt == 0 {
		detail.Attempt = s.Attempts
	}
	if detail.DeliveryID == "" {
		detail.DeliveryID = s.PendingDeliveryID
	}
	s.Deliveries = append(s.Deliveries, detail)
	s.trimLedger()

	meta := e.metaFor(s.Spec.Topic)
	e.stats.RecordAttempt(meta)
	e.stats.RecordFailed(meta, err)

	if retryable && s.Attempts < e.maxAttempts(s.Spec.Topic) {
		delay := e.backoffFor(s.Spec.Topic, s.Attempts)
		next := now.Add(delay)
		s.State = StateQueued
		e.queue.Push(notify.Task{
			Key:        s.TaskKey,
			OrderID:    o.ID,
			StepName:   s.Spec.Name,
			Topic:      s.Spec.Topic,
			Attempt:    s.Attempts + 1,
			DeliveryID: e.newID(),
			NotBefore:  next,
		})
		e.logger.Warn("delivery failed, retry scheduled",
			slog.String("task_key", s.TaskKey),
			slog.Int("attempt", s.Attempts),
			slog.Duration("retry_in", delay),
			slog.String("error", err.Error()),
		)
		return
	}

	e.escalateLocked(o, s, err, now)
}

// escalateLocked retires a task as escalated and notifies the hook.
func (e *Engine) escalateLocked(o *Order, s *Step, err error, now time.Time) {
	s.State = StateEscalated
	s.FinishedAt = &now
	s.UpdatedAt = now
	s.PendingDeliveryID = ""
	s.ResultDeadline = nil

	meta := e.metaFor(s.Spec.Topic)
	e.stats.RecordEscalated(meta)

	esc := Escalation{
		OrderID:  o.ID,
		TaskKey:  s.TaskKey,
		Step:     s.Spec.Name,
		Topic:    s.Spec.Topic,
		Attempts: s.Attempts,
		Err:      err,
		At:       now,
	}
	e.escalations = append(e.escalations, esc)

	e.logger.Error("task escalated",
		slog.String("task_key", s.TaskKey),
		slog.String("order_id", o.ID),
		slog.String("step", s.Spec.Name),
		slog.Int("attempts", s.Attempts),
		slog.String("error", err.Error()),
	)
	if hook := e.onEscalate; hook != nil {
		hook(esc)
	}
}

func (s *Step) trimLedger() {
	if len(s.Deliveries) > maxLedger {
		s.Deliveries = s.Deliveries[len(s.Deliveries)-maxLedger:]
	}
}

// hasDelivery reports whether this attempt is already in the ledger, so a
// result that arrives before its own response cannot record it twice.
func (s *Step) hasDelivery(deliveryID string) bool {
	if deliveryID == "" {
		return false
	}
	for _, d := range s.Deliveries {
		if d.DeliveryID == deliveryID {
			return true
		}
	}
	return false
}

func (e *Engine) metaFor(topic string) notify.Meta {
	ep, err := e.registry.ByTopic(topic)
	if err != nil {
		return notify.Meta{Topic: topic}
	}
	return notify.Meta{Topic: topic, Service: ep.Service, Action: ep.Action, URL: ep.URL}
}

func (e *Engine) maxAttempts(topic string) int {
	ep, err := e.registry.ByTopic(topic)
	if err != nil {
		return notify.DefaultMaxAttempts
	}
	return ep.MaxAttempts
}

func (e *Engine) backoffFor(topic string, attempt int) time.Duration {
	ep, err := e.registry.ByTopic(topic)
	if err != nil {
		ep = notify.Endpoint{
			BackoffBase: notify.DefaultBackoffBase,
			BackoffMax:  notify.DefaultBackoffMax,
		}
	}
	return ep.Backoff(attempt)
}

func (e *Engine) snapshotLocked(o *Order) *Order {
	out := *o
	out.Steps = make([]*Step, len(o.Steps))
	// The name index must point at the clones. Copying the map by reference
	// would leave Step() handing out pointers to the live steps, which a
	// caller could then read with the lock released.
	out.byName = make(map[string]*Step, len(o.Steps))

	for i, s := range o.Steps {
		clone := *s
		clone.Deliveries = append([]Delivery(nil), s.Deliveries...)
		clone.Result = append(json.RawMessage(nil), s.Result...)
		out.Steps[i] = &clone
		out.byName[s.Spec.Name] = &clone
	}
	return &out
}

func truncate(b []byte, limit int) string {
	s := string(b)
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// worker pulls due tasks and executes them.
func (e *Engine) worker() {
	defer e.wg.Done()

	for {
		select {
		case <-e.stopped:
			return
		default:
		}

		task, ok := e.queue.PopDue(e.now().UTC())
		if !ok {
			select {
			case <-e.stopped:
				return
			case <-e.queue.WakeC():
			case <-time.After(e.cfg.PollInterval):
			}
			continue
		}

		if err := e.execute(task); err != nil && !errors.Is(err, context.Canceled) {
			e.logger.Error("delivery execution failed", slog.String("error", err.Error()))
		}
	}
}

// execute performs one attempt, or checks for an awaited result.
func (e *Engine) execute(task notify.Task) error {
	e.mu.Lock()

	o, ok := e.orders[task.OrderID]
	if !ok {
		e.mu.Unlock()
		return nil
	}
	s, ok := o.byName[task.StepName]
	if !ok || s.TaskKey != task.Key {
		// The step was renumbered or the order is gone; drop the task.
		e.mu.Unlock()
		return nil
	}

	if task.AwaitResult {
		return e.checkResultLocked(o, s, task)
	}

	if s.State != StateQueued {
		e.mu.Unlock()
		return nil
	}
	ep, err := e.registry.ByTopic(task.Topic)
	if err != nil {
		s.Attempts++
		e.recordFailureLocked(o, s, Delivery{Attempt: task.Attempt}, fmt.Errorf("no endpoint registered for topic %q", task.Topic), true)
		e.advanceLocked(o)
		e.mu.Unlock()
		return nil
	}

	s.State = StateRunning
	// The task owns the attempt number, so a retry queued elsewhere is still
	// counted correctly.
	s.Attempts = task.Attempt
	s.PendingDeliveryID = task.DeliveryID
	input := e.contextForLocked(o, s, ep, task)
	e.stats.RecordAttempt(notify.Meta{Topic: task.Topic, Service: ep.Service, Action: ep.Action, URL: ep.URL})
	e.mu.Unlock()

	res, invokeErr := e.invoker.Invoke(context.Background(), ep, input)

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-resolve: the order may have been settled while the request was in
	// flight.
	o, ok = e.orders[task.OrderID]
	if !ok {
		return nil
	}
	s, ok = o.byName[task.StepName]
	if !ok || s.TaskKey != task.Key {
		return nil
	}

	// A receiver is allowed to post its result before it returns the HTTP
	// response, and a fast one will. If that already resolved the step, this
	// attempt's outcome is stale: applying it would drag a finished step back
	// into waiting, where it would sit until its deadline and then be
	// retried. Only the delivery this attempt is still waiting on may change
	// the step.
	if s.PendingDeliveryID != task.DeliveryID {
		e.logger.Debug("discarding a stale delivery outcome",
			slog.String("task_key", task.Key),
			slog.String("step_state", string(s.State)),
		)
		return nil
	}

	if invokeErr != nil {
		return e.applyFailureLocked(o, s, ep, task, res, invokeErr)
	}
	return e.applySuccessLocked(o, s, ep, task, res)
}

// checkResultLocked handles the deadline for an awaited result. Reaching it
// without a result counts as a failed attempt, which is what turns "we never
// heard back" into a retry and, eventually, an escalation.
func (e *Engine) checkResultLocked(o *Order, s *Step, task notify.Task) error {
	defer e.mu.Unlock()

	if s.State != StateWaiting {
		// The result arrived first and this task is now obsolete.
		return nil
	}

	s.PendingDeliveryID = ""
	s.ResultDeadline = nil
	err := fmt.Errorf("no result for delivery %s within the result timeout", task.DeliveryID)

	e.logger.Warn("result deadline missed",
		slog.String("task_key", s.TaskKey),
		slog.Int("attempt", s.Attempts),
	)

	e.recordFailureLocked(o, s, Delivery{Attempt: task.Attempt, DeliveryID: task.DeliveryID}, err, true)
	e.advanceLocked(o)
	return nil
}

func (e *Engine) applySuccessLocked(o *Order, s *Step, ep notify.Endpoint, task notify.Task, res notify.Result) error {
	now := e.now().UTC()
	e.stats.RecordDelivered(notify.Meta{Topic: task.Topic, Service: ep.Service, Action: ep.Action, URL: ep.URL}, res.Latency)

	if s.Spec.Kind == KindCallback {
		// Acknowledged but not finished: the receiver owes us a result. If it
		// never arrives, the deadline task below retries the notification.
		deadline := now.Add(ep.ResultTimeout)
		s.State = StateWaiting
		s.ResultDeadline = &deadline
		s.UpdatedAt = now
		s.Deliveries = append(s.Deliveries, Delivery{
			Attempt:    task.Attempt,
			DeliveryID: task.DeliveryID,
			At:         now,
			Outcome:    "delivered",
			StatusCode: res.StatusCode,
			LatencyMs:  res.Latency.Milliseconds(),
		})
		s.trimLedger()
		e.logger.Info("delivery acknowledged, awaiting result",
			slog.String("task_key", s.TaskKey),
			slog.Int("status", res.StatusCode),
			slog.Time("deadline", deadline),
		)
		e.queue.Push(notify.Task{
			Key:         task.Key,
			OrderID:     task.OrderID,
			StepName:    task.StepName,
			Topic:       task.Topic,
			Attempt:     task.Attempt,
			DeliveryID:  task.DeliveryID,
			NotBefore:   deadline,
			AwaitResult: true,
		})
		e.settleLocked(o, now)
		return nil
	}

	s.State = StateSucceeded
	s.FinishedAt = &now
	s.UpdatedAt = now
	s.PendingDeliveryID = ""
	s.Deliveries = append(s.Deliveries, Delivery{
		Attempt:    task.Attempt,
		DeliveryID: task.DeliveryID,
		At:         now,
		Outcome:    "delivered",
		StatusCode: res.StatusCode,
		LatencyMs:  res.Latency.Milliseconds(),
	})
	s.trimLedger()

	e.logger.Info("task delivered",
		slog.String("task_key", s.TaskKey),
		slog.Int("attempt", task.Attempt),
		slog.Int("status", res.StatusCode),
	)
	e.advanceLocked(o)
	return nil
}

func (e *Engine) applyFailureLocked(o *Order, s *Step, ep notify.Endpoint, task notify.Task, res notify.Result, invokeErr error) error {
	retryable := true
	var failure *notify.Failure
	if errors.As(invokeErr, &failure) {
		retryable = failure.Retryable()
	}
	if s.Attempts >= ep.MaxAttempts {
		retryable = false
	}

	e.logger.Warn("delivery attempt failed",
		slog.String("task_key", s.TaskKey),
		slog.Int("attempt", task.Attempt),
		slog.Int("max_attempts", ep.MaxAttempts),
		slog.Bool("retryable", retryable),
		slog.String("error", invokeErr.Error()),
	)

	e.recordFailureLocked(o, s, Delivery{
		Attempt:    task.Attempt,
		DeliveryID: task.DeliveryID,
		StatusCode: res.StatusCode,
		LatencyMs:  res.Latency.Milliseconds(),
	}, invokeErr, retryable)
	e.advanceLocked(o)
	return nil
}

// contextForLocked assembles the template context. The caller must hold e.mu.
func (e *Engine) contextForLocked(o *Order, s *Step, ep notify.Endpoint, task notify.Task) notify.ContextInput {
	return notify.ContextInput{
		OrderID:        o.ID,
		ExternalRef:    o.ExternalRef,
		Subject:        o.Subject,
		Preset:         o.Preset,
		Phase:          string(o.Phase),
		CreatedAt:      o.CreatedAt,
		StepName:       s.Spec.Name,
		StepKind:       string(s.Spec.Kind),
		Topic:          s.Spec.Topic,
		Service:        ep.Service,
		Action:         ep.Action,
		Attempt:        task.Attempt,
		DeliveryID:     task.DeliveryID,
		IdempotencyKey: task.Key,
		ResultURL:      e.ResultURL(o.ID, s.Spec.Name),
		Secret:         ep.Secret,
		Timestamp:      e.now().UTC(),
		Items:          o.Items,
		Payload:        o.Payload,
	}
}
