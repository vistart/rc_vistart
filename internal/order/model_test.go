package order

import (
	"errors"
	"testing"
	"time"

	"github.com/vistart/rc_vistart/internal/notify"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func TestNewBuildsStepsAndTaskKeys(t *testing.T) {
	o, err := New("ord_1", "order.standard", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if o.State != OrderPending {
		t.Errorf("State = %q, want pending", o.State)
	}
	if o.Phase != PhasePrepare {
		t.Errorf("Phase = %q, want prepare", o.Phase)
	}
	if len(o.Steps) != 5 {
		t.Fatalf("len(Steps) = %d, want 5", len(o.Steps))
	}

	first := o.Steps[0]
	if first.TaskKey != "ord_1/verify.identity" {
		t.Errorf("TaskKey = %q, want ord_1/verify.identity", first.TaskKey)
	}
	if first.State != StatePending {
		t.Errorf("State = %q, want pending", first.State)
	}
	if !first.Spec.Required() {
		t.Error("steps are optional, want required by default")
	}
	if !o.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt = %v, want %v", o.CreatedAt, testNow)
	}
}

func TestStepLookup(t *testing.T) {
	o, err := New("ord_1", "", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	s, err := o.Step(StepVerifyStock)
	if err != nil {
		t.Fatalf("Step(%q) error = %v", StepVerifyStock, err)
	}
	if s.Spec.Topic != TopicStockPrehold {
		t.Errorf("Topic = %q, want %q", s.Spec.Topic, TopicStockPrehold)
	}
	if _, err := o.Step("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Step(nope) error = %v, want ErrNotFound", err)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name  string
		steps []StepSpec
	}{
		{"no steps", nil},
		{"step without a name", []StepSpec{{Topic: "a.b"}}},
		{"step without a topic", []StepSpec{{Name: "s"}}},
		{"duplicate names", []StepSpec{{Name: "s", Topic: "a.b"}, {Name: "s", Topic: "c.d"}}},
		{"unknown kind", []StepSpec{{Name: "s", Topic: "a.b", Kind: "magic"}}},
		{"unknown phase", []StepSpec{{Name: "s", Topic: "a.b", Phase: "ship"}}},
		{"unknown dependency", []StepSpec{{Name: "s", Topic: "a.b", DependsOn: []string{"ghost"}}}},
		{"self dependency", []StepSpec{{Name: "s", Topic: "a.b", DependsOn: []string{"s"}}}},
		{"dependency cycle", []StepSpec{
			{Name: "a", Topic: "x.y", DependsOn: []string{"b"}},
			{Name: "b", Topic: "x.y", DependsOn: []string{"a"}},
		}},
		{"cross-phase dependency", []StepSpec{
			{Name: "a", Topic: "x.y", Phase: PhasePrepare},
			{Name: "b", Topic: "x.y", Phase: PhaseProduce, DependsOn: []string{"a"}},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New("ord_1", "", tc.steps, testNow); !errors.Is(err, ErrInvalid) {
				t.Fatalf("New() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewRejectsEmptyID(t *testing.T) {
	if _, err := New("", "", StandardSteps(), testNow); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New() error = %v, want ErrInvalid", err)
	}
}

func TestReadyRespectsDependencies(t *testing.T) {
	o, err := New("ord_1", "", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Only the first prepare step is unblocked; stock waits on identity.
	ready := o.Ready()
	if len(ready) != 1 || ready[0].Spec.Name != StepVerifyIdentity {
		t.Fatalf("Ready() = %v, want only %s", names(ready), StepVerifyIdentity)
	}

	o.byName[StepVerifyIdentity].State = StateSucceeded
	ready = o.Ready()
	if len(ready) != 1 || ready[0].Spec.Name != StepVerifyStock {
		t.Fatalf("Ready() = %v, want only %s", names(ready), StepVerifyStock)
	}
}

func TestBlockedDetectsFailedDependencies(t *testing.T) {
	o, err := New("ord_1", "", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	o.byName[StepVerifyIdentity].State = StateEscalated

	blocked := o.Blocked()
	if len(blocked) != 1 || blocked[0].Spec.Name != StepVerifyStock {
		t.Fatalf("Blocked() = %v, want only %s", names(blocked), StepVerifyStock)
	}
	if len(o.Ready()) != 0 {
		t.Errorf("Ready() = %v, want nothing runnable after a failure", names(o.Ready()))
	}
}

func TestPhaseComplete(t *testing.T) {
	o, err := New("ord_1", "", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, s := range o.Steps {
		if s.Spec.Phase == PhasePrepare {
			s.State = StateSucceeded
		}
	}
	complete, ok := o.PhaseComplete(PhasePrepare)
	if !complete || !ok {
		t.Errorf("PhaseComplete(prepare) = (%v, %v), want (true, true)", complete, ok)
	}
	complete, ok = o.PhaseComplete(PhaseProduce)
	if complete {
		t.Errorf("PhaseComplete(produce) complete = %v, want false while steps are pending", complete)
	}

	// A required failure fails the phase.
	o.byName[StepVerifyStock].State = StateEscalated
	complete, ok = o.PhaseComplete(PhasePrepare)
	if !complete || ok {
		t.Errorf("PhaseComplete(prepare) = (%v, %v), want (true, false)", complete, ok)
	}
}

func TestPhaseCompleteIgnoresOptionalFailure(t *testing.T) {
	o, err := New("ord_1", "", []StepSpec{
		{Name: "must", Topic: "a.b"},
		{Name: "nice", Topic: "a.c", Optional: true},
	}, testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	o.byName["must"].State = StateSucceeded
	o.byName["nice"].State = StateEscalated

	complete, ok := o.PhaseComplete(PhasePrepare)
	if !complete || !ok {
		t.Errorf("PhaseComplete() = (%v, %v), want (true, true): an optional failure must not fail the phase", complete, ok)
	}
}

func TestSucceededRequiresEveryRequiredStep(t *testing.T) {
	o, err := New("ord_1", "", []StepSpec{
		{Name: "must", Topic: "a.b"},
		{Name: "also-must", Topic: "a.c"},
	}, testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// This is the invariant that stops an order being declared complete while
	// a step was silently dropped.
	o.byName["must"].State = StateSucceeded
	if o.Succeeded() {
		t.Error("Succeeded() = true while a required step is pending")
	}

	o.byName["also-must"].State = StateSkipped
	if o.Succeeded() {
		t.Error("Succeeded() = true while a required step was skipped")
	}

	o.byName["also-must"].State = StateSucceeded
	if !o.Succeeded() {
		t.Error("Succeeded() = false, want true when every required step succeeded")
	}
}

func TestOptionalStepFailureDoesNotBlockSuccess(t *testing.T) {
	o, err := New("ord_1", "", []StepSpec{
		{Name: "must", Topic: "a.b"},
		{Name: "nice", Topic: "a.c", Optional: true},
	}, testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	o.byName["must"].State = StateSucceeded
	o.byName["nice"].State = StateEscalated

	if !o.Succeeded() {
		t.Error("Succeeded() = false, want an optional failure to be tolerated")
	}
}

func TestCounts(t *testing.T) {
	o, err := New("ord_1", "", StandardSteps(), testNow)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	o.byName[StepVerifyIdentity].State = StateSucceeded
	o.byName[StepVerifyStock].State = StateEscalated

	counts := o.Counts()
	if counts["succeeded"] != 1 || counts["escalated"] != 1 || counts["pending"] != 3 {
		t.Errorf("Counts() = %v, want 1 succeeded, 1 escalated, 3 pending", counts)
	}
	if counts["total"] != 5 {
		t.Errorf("total = %d, want 5", counts["total"])
	}
}

func TestStateAndStepHelpers(t *testing.T) {
	if !StateSucceeded.Terminal() || !StateEscalated.Terminal() || !StateSkipped.Terminal() {
		t.Error("a terminal state does not report Terminal()")
	}
	if StateWaiting.Terminal() || StateQueued.Terminal() || StateRunning.Terminal() || StatePending.Terminal() {
		t.Error("a non-terminal state reports Terminal()")
	}
	if !OrderSucceeded.Terminal() || !OrderFailed.Terminal() {
		t.Error("a finished order does not report Terminal()")
	}
	if OrderRunning.Terminal() || OrderPending.Terminal() {
		t.Error("an unfinished order reports Terminal()")
	}
	if !StateSucceeded.Satisfied() {
		t.Error("StateSucceeded.Satisfied() = false")
	}
	if StateSkipped.Satisfied() {
		t.Error("StateSkipped.Satisfied() = true")
	}
}

func TestPresetsAreWellFormed(t *testing.T) {
	for _, p := range ListPresets() {
		t.Run(p.Name, func(t *testing.T) {
			if p.Description == "" {
				t.Error("Description is empty")
			}
			if _, err := New("ord_1", p.Name, p.StepsFor(), testNow); err != nil {
				t.Errorf("preset does not build a valid order: %v", err)
			}
		})
	}
}

func TestLookupPreset(t *testing.T) {
	if _, err := LookupPreset(PresetOrder); err != nil {
		t.Errorf("LookupPreset(%q) error = %v", PresetOrder, err)
	}
	if _, err := LookupPreset("nope"); !errors.Is(err, ErrInvalid) {
		t.Errorf("LookupPreset(nope) error = %v, want ErrInvalid", err)
	}
}

func TestPresetStepsAreCopies(t *testing.T) {
	p, err := LookupPreset(PresetOrder)
	if err != nil {
		t.Fatalf("LookupPreset() error = %v", err)
	}

	steps := p.StepsFor()
	steps[0].DependsOn = append(steps[0].DependsOn, "injected")

	again := p.StepsFor()
	for _, dep := range again[0].DependsOn {
		if dep == "injected" {
			t.Error("StepsFor() shares its DependsOn slice with the preset")
		}
	}
}

func TestStandardStepsCoverTheTwoStockTouches(t *testing.T) {
	// Stock is reserved during prepare and handed over during produce, which
	// is the split the business described.
	var prehold, outbound bool
	for _, s := range StandardSteps() {
		if s.Name == StepVerifyStock && s.Phase == PhasePrepare && s.Kind == KindCallback {
			prehold = true
		}
		if s.Name == StepStockOutbound && s.Phase == PhaseProduce {
			outbound = true
		}
	}
	if !prehold {
		t.Error("no prepare-phase stock reservation step")
	}
	if !outbound {
		t.Error("no produce-phase outbound step")
	}
}

func names(steps []*Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Spec.Name)
	}
	return out
}

var _ = notify.TaskKey
