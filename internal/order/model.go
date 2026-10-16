// Package order implements the order model: an order is a fixed graph of
// steps, and every step is one task identified by "<order id>/<step name>".
// That identity is stable across retries, so both rc_vistart and the receiver
// can tell a retry from a new task and neither can process the same work twice.
package order

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vistart/rc_vistart/internal/notify"
)

// Errors returned when an order or step graph is invalid.
var (
	// ErrInvalid marks a malformed order or step graph.
	ErrInvalid = errors.New("order: invalid")
	// ErrNotFound marks an unknown order, step or delivery.
	ErrNotFound = errors.New("order: not found")
	// ErrConflict marks an operation that conflicts with the current state.
	ErrConflict = errors.New("order: conflict")
)

// Phase groups steps into the stages of the business flow.
type Phase string

// The two phases of a standard order: verification before any commitment, then
// fulfilment.
const (
	PhasePrepare Phase = "prepare"
	PhaseProduce Phase = "produce"
)

// StepKind decides what counts as "done" for a step.
type StepKind string

const (
	// KindNotify is done when the receiver acknowledges the HTTP request with
	// an expected status.
	KindNotify StepKind = "notify"
	// KindCallback is done only when the receiver additionally posts a result,
	// which is what a reservation or a verification needs: the HTTP 200 only
	// means the request was understood, not that the stock exists.
	KindCallback StepKind = "callback"
)

// StepState is the lifecycle position of a step.
type StepState string

const (
	// StatePending means the step is eligible but not queued yet.
	StatePending StepState = "pending"
	// StateQueued means a delivery is scheduled.
	StateQueued StepState = "queued"
	// StateRunning means a delivery attempt is in flight.
	StateRunning StepState = "running"
	// StateWaiting means the request was acknowledged and a result is awaited.
	StateWaiting StepState = "waiting"
	// StateSucceeded is a terminal success.
	StateSucceeded StepState = "succeeded"
	// StateEscalated is a terminal failure after exhausting the attempts.
	StateEscalated StepState = "escalated"
	// StateSkipped is terminal and means a dependency did not succeed.
	StateSkipped StepState = "skipped"
)

// Terminal reports whether no further transition is possible.
func (s StepState) Terminal() bool {
	return s == StateSucceeded || s == StateEscalated || s == StateSkipped
}

// Satisfied reports whether the step counts towards its phase being complete.
func (s StepState) Satisfied() bool {
	return s == StateSucceeded
}

// OrderState is the lifecycle position of an order.
type OrderState string

const (
	OrderPending   OrderState = "pending"
	OrderRunning   OrderState = "running"
	OrderSucceeded OrderState = "succeeded"
	OrderFailed    OrderState = "failed"
)

// Terminal reports whether the order will not change state again.
func (s OrderState) Terminal() bool {
	return s == OrderSucceeded || s == OrderFailed
}

// StepSpec is the declaration of a step, fixed when the order is created.
type StepSpec struct {
	Name  string   `json:"name"`
	Phase Phase    `json:"phase"`
	Kind  StepKind `json:"kind"`
	// Topic is the endpoint topic to deliver to, in "<service>.<action>" form.
	Topic string `json:"topic"`
	// DependsOn lists step names that must succeed first.
	DependsOn []string `json:"depends_on,omitempty"`
	// Optional steps that fail do not fail the order. A step is required by
	// default, which is what keeps an order from completing while a step was
	// silently dropped.
	Optional bool `json:"optional,omitempty"`
}

// Required reports whether the step must succeed for the order to complete.
func (s StepSpec) Required() bool { return !s.Optional }

// Delivery is one attempt, recorded so that both "did we send it?" and "did we
// send it twice?" can be answered after the fact.
type Delivery struct {
	Attempt    int       `json:"attempt"`
	DeliveryID string    `json:"delivery_id"`
	At         time.Time `json:"at"`
	// Outcome is "delivered", "failed" or "awaited".
	Outcome string `json:"outcome"`
	// StatusCode is the response code, when one arrived.
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMs  int64  `json:"latency_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	// Retryable records whether another attempt could plausibly succeed.
	Retryable bool `json:"retryable,omitempty"`
	// NextAttemptAt is when the retry became due, if it was scheduled.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

// maxLedger bounds the per-step delivery history.
const maxLedger = 16

// Step is a step's declaration plus its runtime state.
type Step struct {
	Spec StepSpec `json:"spec"`
	// TaskKey is the stable identity of this step's work.
	TaskKey string    `json:"task_key"`
	State   StepState `json:"state"`

	Attempts   int        `json:"attempts"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	LastError  string     `json:"last_error,omitempty"`

	// PendingDeliveryID is the delivery whose result is currently awaited.
	PendingDeliveryID string `json:"pending_delivery_id,omitempty"`
	// ResultDeadline is when an awaited result stops counting as on time.
	ResultDeadline *time.Time `json:"result_deadline,omitempty"`

	// Result is the receiver's report for a callback step.
	Result json.RawMessage `json:"result,omitempty"`

	// Deliveries is the attempt ledger, newest last.
	Deliveries []Delivery `json:"deliveries,omitempty"`

	byName map[string]struct{}
}

// Topic is the endpoint topic this step delivers to.
func (s *Step) Topic() string { return s.Spec.Topic }

// Order is one business transaction.
type Order struct {
	ID string `json:"id"`
	// Preset records which scenario built the step graph, if any.
	Preset string `json:"preset,omitempty"`
	// ExternalRef is the caller's own reference, kept for reconciliation.
	ExternalRef string         `json:"external_ref,omitempty"`
	Subject     string         `json:"subject,omitempty"`
	Items       []notify.Item  `json:"items,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`

	State OrderState `json:"state"`
	Phase Phase      `json:"phase"`

	Steps     []*Step   `json:"steps"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// CompletedAt is set once the order reaches a terminal state.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	byName map[string]*Step
}

// New builds an order from a step graph and validates it. It is the pure,
// engine-free constructor used by tests; the engine goes through buildOrder so
// both share one validation path.
func New(id, preset string, steps []StepSpec, now time.Time) (*Order, error) {
	return buildOrder(Spec{ID: id, Preset: preset, Steps: steps}, now, "")
}

// buildOrder assembles and validates an order, defaulting the id to defaultID
// when the spec carries none.
func buildOrder(spec Spec, now time.Time, defaultID string) (*Order, error) {
	if len(spec.Steps) == 0 {
		return nil, fmt.Errorf("%w: an order needs at least one step", ErrInvalid)
	}

	id := spec.ID
	if id == "" {
		id = defaultID
	}
	if id == "" {
		return nil, fmt.Errorf("%w: id must not be empty", ErrInvalid)
	}

	o := &Order{
		ID:          id,
		Preset:      spec.Preset,
		ExternalRef: spec.ExternalRef,
		Subject:     spec.Subject,
		Items:       spec.Items,
		Payload:     spec.Payload,
		State:       OrderPending,
		Phase:       PhasePrepare,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
		byName:      make(map[string]*Step, len(spec.Steps)),
		Steps:       make([]*Step, 0, len(spec.Steps)),
	}

	for i := range spec.Steps {
		s := spec.Steps[i]

		if s.Kind == "" {
			s.Kind = KindNotify
		}
		if s.Phase == "" {
			s.Phase = PhasePrepare
		}

		if s.Name == "" {
			return nil, fmt.Errorf("%w: steps[%d] has no name", ErrInvalid, i)
		}
		if _, dup := o.byName[s.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate step name %q", ErrInvalid, s.Name)
		}
		if s.Topic == "" {
			return nil, fmt.Errorf("%w: step %q has no topic", ErrInvalid, s.Name)
		}
		if s.Kind != KindNotify && s.Kind != KindCallback {
			return nil, fmt.Errorf("%w: step %q has unknown kind %q", ErrInvalid, s.Name, s.Kind)
		}
		if s.Phase != PhasePrepare && s.Phase != PhaseProduce {
			return nil, fmt.Errorf("%w: step %q has unknown phase %q", ErrInvalid, s.Name, s.Phase)
		}

		step := &Step{
			Spec:      s,
			TaskKey:   notify.TaskKey(id, s.Name),
			State:     StatePending,
			UpdatedAt: now.UTC(),
		}
		o.Steps = append(o.Steps, step)
		o.byName[s.Name] = step
	}

	if err := o.validateGraph(); err != nil {
		return nil, err
	}
	return o, nil
}

// Step looks a step up by name.
func (o *Order) Step(name string) (*Step, error) {
	s, ok := o.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: step %q of order %q", ErrNotFound, name, o.ID)
	}
	return s, nil
}

// validateGraph checks that dependencies exist, stay inside a phase and
// contain no cycle.
func (o *Order) validateGraph() error {
	// Three-colour depth-first search: grey means "on the current path", which
	// is what distinguishes a real cycle from a node visited more than once
	// through different branches.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[string]int, len(o.Steps))

	var visit func(name string) error
	visit = func(name string) error {
		switch color[name] {
		case grey:
			return fmt.Errorf("%w: dependency cycle through step %q", ErrInvalid, name)
		case black:
			return nil
		}

		s, ok := o.byName[name]
		if !ok {
			return fmt.Errorf("%w: unknown step %q", ErrInvalid, name)
		}
		color[name] = grey

		for _, dep := range s.Spec.DependsOn {
			d, ok := o.byName[dep]
			if !ok {
				return fmt.Errorf("%w: step %q depends on unknown step %q", ErrInvalid, name, dep)
			}
			if d.Spec.Phase != s.Spec.Phase {
				return fmt.Errorf("%w: step %q depends on %q, which is in phase %q", ErrInvalid, name, dep, d.Spec.Phase)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}

		color[name] = black
		return nil
	}

	for _, s := range o.Steps {
		if err := visit(s.Spec.Name); err != nil {
			return err
		}
	}
	return nil
}

// Ready returns the steps of the current phase that are pending with every
// dependency succeeded, in declaration order.
//
// Steps of a later phase are never ready: reaching that phase is what gates
// them, so consulting this before the phase advanced would let fulfilment run
// before the checks passed.
func (o *Order) Ready() []*Step {
	var out []*Step
	for _, s := range o.Steps {
		if s.State != StatePending || s.Spec.Phase != o.Phase {
			continue
		}
		if o.dependenciesMet(s) {
			out = append(out, s)
		}
	}
	return out
}

func (o *Order) dependenciesMet(s *Step) bool {
	for _, dep := range s.Spec.DependsOn {
		d, ok := o.byName[dep]
		if !ok || d.State != StateSucceeded {
			return false
		}
	}
	return true
}

// Blocked returns the steps of the current phase that can never run because a
// dependency failed.
func (o *Order) Blocked() []*Step {
	var out []*Step
	for _, s := range o.Steps {
		if s.State != StatePending || s.Spec.Phase != o.Phase {
			continue
		}
		if o.anyDependencyFailed(s) {
			out = append(out, s)
		}
	}
	return out
}

func (o *Order) anyDependencyFailed(s *Step) bool {
	for _, dep := range s.Spec.DependsOn {
		d, ok := o.byName[dep]
		if !ok {
			continue
		}
		if d.State == StateEscalated || d.State == StateSkipped {
			return true
		}
	}
	return false
}

// PhaseComplete reports whether every step of the phase reached a terminal
// state, and whether they all succeeded.
func (o *Order) PhaseComplete(phase Phase) (complete, ok bool) {
	seen := false
	for _, s := range o.Steps {
		if s.Spec.Phase != phase {
			continue
		}
		seen = true
		if !s.State.Terminal() {
			return false, false
		}
		// A non-required step is allowed to fail without failing the order.
		if !s.State.Satisfied() && s.Spec.Required() {
			return true, false
		}
	}
	return seen, true
}

// Phases returns the phases the order actually has steps in, in canonical
// order. A single-phase order has no produce phase, and must not wait for one.
func (o *Order) Phases() []Phase {
	hasPrepare, hasProduce := false, false
	for _, s := range o.Steps {
		switch s.Spec.Phase {
		case PhasePrepare:
			hasPrepare = true
		case PhaseProduce:
			hasProduce = true
		}
	}

	var out []Phase
	if hasPrepare {
		out = append(out, PhasePrepare)
	}
	if hasProduce {
		out = append(out, PhaseProduce)
	}
	return out
}

// Counts summarises the step states.
func (o *Order) Counts() map[string]int {
	counts := make(map[string]int, 7)
	for _, s := range o.Steps {
		counts[string(s.State)]++
	}
	counts["total"] = len(o.Steps)
	return counts
}

// Succeeded reports whether every required step succeeded, which is the
// invariant that stops an order from being declared done while a step was
// silently dropped.
func (o *Order) Succeeded() bool {
	for _, s := range o.Steps {
		if s.Spec.Required() && s.State != StateSucceeded {
			return false
		}
	}
	return len(o.Steps) > 0
}
