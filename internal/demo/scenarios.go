package demo

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vistart/rc_vistart/internal/mockservice"
	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
)

// doc holds a scenario's descriptive text. Keeping it separate from the
// Scenario value lets a run function refer to its own text without creating an
// initialisation cycle.
type doc struct {
	name        string
	title       string
	explanation string
}

func newScenario(d doc, run func(*Env) (Result, error)) Scenario {
	return Scenario{Name: d.name, Title: d.title, Explanation: d.explanation, Run: run}
}

// result assembles a scenario's report from its own doc.
func (d doc) result(c *collector, steps []StepRow, notes []string, started time.Time) Result {
	return Result{
		Name:        d.name,
		Title:       d.title,
		Explanation: d.explanation,
		Checks:      c.checks,
		Passed:      c.passed(),
		Steps:       steps,
		Notes:       notes,
		Duration:    time.Since(started),
	}
}

var standardOrderDoc = doc{
	name:  "order",
	title: "Standard order: check, then fulfil",
	explanation: "A user buys something. Identity, stock reservation and payment are " +
		"verified in prepare; only then does production start. Stock is touched twice: " +
		"reserved during prepare, handed over during produce. Each check is a callback " +
		"step, so the order waits for the destination's answer rather than a bare 200.",
}

// standardOrder is the canonical buy-goods flow.
var standardOrder = newScenario(standardOrderDoc, func(env *Env) (Result, error) {
	started := time.Now()

	identity := env.AddMock(mockservice.Config{Service: "identity", Action: "verify"})
	prehold := env.AddMock(mockservice.Config{Service: "inventory", Action: "stock.prehold"})
	payment := env.AddMock(mockservice.Config{Service: "payment", Action: "verify"})
	delivery := env.AddMock(mockservice.Config{Service: "fulfilment", Action: "delivery"})
	outbound := env.AddMock(mockservice.Config{Service: "inventory", Action: "stock.outbound"})
	for _, m := range []*mockservice.Mock{identity, prehold, payment, delivery, outbound} {
		if m == nil {
			return Result{}, fmt.Errorf("a destination failed to register")
		}
	}

	id, err := env.Submit(order.Spec{
		ID:          "ord-demo-order",
		Preset:      order.PresetOrder,
		ExternalRef: "BIZ-2026-0042",
		Subject:     "user-42",
		Items:       []notify.Item{{SKU: "SKU-1", Name: "Widget", Quantity: 2, Price: 19.9}},
		Payload:     map[string]any{"channel": "app"},
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(id, 30*time.Second)
	o := env.Order(id)

	c := &collector{}
	c.equal(o.State, order.OrderSucceeded, "order state")
	c.equal(o.Phase, order.PhaseProduce, "final phase")
	c.equal(len(o.Steps), 5, "step count")

	allSucceeded := true
	for _, s := range o.Steps {
		if s.State != order.StateSucceeded {
			allSucceeded = false
		}
	}
	c.ok(allSucceeded, "every step succeeded")

	// The phase gate: a produce step cannot start before every prepare check
	// has passed, so the chain of attempts must be one attempt each.
	for _, s := range o.Steps {
		if s.Attempts != 1 {
			c.add(false, "%s took %d attempts, want 1", s.Spec.Name, s.Attempts)
		}
	}
	c.ok(true, "each destination needed exactly one attempt")

	for _, m := range []*mockservice.Mock{identity, prehold, payment, delivery, outbound} {
		c.equal(m.Effects(), 1, "effects applied at "+m.Topic())
	}

	// Stock was reserved during prepare and handed over during produce: the
	// reservation topic was notified exactly once.
	c.equal(prehold.Stats().Notifications, 1, "notifications to inventory.stock.prehold during prepare")
	c.equal(outbound.Stats().Notifications, 1, "notifications to inventory.stock.outbound during produce")

	return standardOrderDoc.result(c, env.rows(o), []string{
		"prepare: verify identity -> reserve stock -> verify payment, then produce: deliver -> ship",
	}, started), nil
})

var simpleNotificationsDoc = doc{
	name:  "simple",
	title: "Single notifications: ad registration and CRM contact",
	explanation: "The other two business cases need no order graph at all. An ad " +
		"registration and a paid subscription are each one notify step, which is done as " +
		"soon as the destination acknowledges the request.",
}

// simpleNotifications covers the two single-notification business cases.
var simpleNotifications = newScenario(simpleNotificationsDoc, func(env *Env) (Result, error) {
	started := time.Now()

	ads := env.AddMock(mockservice.Config{Service: "ads", Action: "registration"})
	crm := env.AddMock(mockservice.Config{Service: "crm", Action: "contact.update"})
	if ads == nil || crm == nil {
		return Result{}, fmt.Errorf("a destination failed to register")
	}

	adOrder, err := env.Submit(order.Spec{
		ID:          "ord-demo-ad",
		Preset:      order.PresetAdRegistration,
		ExternalRef: "BIZ-2026-0043",
		Subject:     "visitor-7",
		Payload:     map[string]any{"campaign_id": "C-9"},
	})
	if err != nil {
		return Result{}, err
	}
	crmOrder, err := env.Submit(order.Spec{
		ID:          "ord-demo-crm",
		Preset:      order.PresetContactUpdate,
		ExternalRef: "BIZ-2026-0044",
		Subject:     "user-42",
		Payload:     map[string]any{"contact_id": "CT-3", "status": "paid"},
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(adOrder, 20*time.Second)
	env.WaitForOrderTerminal(crmOrder, 20*time.Second)

	ad := env.Order(adOrder)
	crmState := env.Order(crmOrder)

	c := &collector{}
	c.equal(ad.State, order.OrderSucceeded, "ad registration order")
	c.equal(crmState.State, order.OrderSucceeded, "crm contact order")
	c.equal(ads.Effects(), 1, "effects applied at ads.registration")
	c.equal(crm.Effects(), 1, "effects applied at crm.contact.update")

	// The destination's payload format is entirely its own business: the CRM
	// mock expects "status", and gets it from the shared order payload.
	c.ok(strings.Contains(crm.LastBody(), `"status":"paid"`),
		"the CRM destination received its own payload shape: %s", compact(crm.LastBody()))

	steps := append(env.rows(ad), env.rows(crmState)...)
	return simpleNotificationsDoc.result(c, steps, nil, started), nil
})

var retryWithoutDuplicatingDoc = doc{
	name:  "retry",
	title: "Retry without duplicating the effect",
	explanation: "The destination rejects the first two attempts and accepts the third. " +
		"rc_vistart backs off and tries again, so one task produces three notifications. " +
		"Because the task key is stable across retries, the destination applies the " +
		"business effect exactly once.",
}

// retryWithoutDuplicating is the central reliability claim.
var retryWithoutDuplicating = newScenario(retryWithoutDuplicatingDoc, func(env *Env) (Result, error) {
	started := time.Now()

	outbound := env.AddMock(mockservice.Config{
		Service:     "inventory",
		Action:      "stock.outbound",
		FailFirst:   2,
		FailStatus:  http.StatusServiceUnavailable,
		MaxAttempts: 5,
		BackoffBase: 100 * time.Millisecond,
		BackoffMax:  400 * time.Millisecond,
	})
	if outbound == nil {
		return Result{}, fmt.Errorf("the destination failed to register")
	}

	id, err := env.Submit(order.Spec{
		ID: "ord-demo-retry",
		Steps: []order.StepSpec{{
			Name:  "ship",
			Phase: order.PhasePrepare,
			Kind:  order.KindNotify,
			Topic: "inventory.stock.outbound",
		}},
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(id, 30*time.Second)
	o := env.Order(id)
	s, err := o.Step("ship")
	if err != nil {
		return Result{}, err
	}

	c := &collector{}
	c.equal(o.State, order.OrderSucceeded, "order state")
	c.equal(s.State, order.StateSucceeded, "step state")
	c.equal(s.Attempts, 3, "delivery attempts")

	stats := outbound.Stats()
	c.equal(stats.Notifications, 3, "notifications received")
	// The whole point of the task key: three notifications, one effect.
	c.equal(stats.Effects, 1, "business effects applied")

	// Every attempt is on the record, which is the other half of the contract:
	// nothing is delivered invisibly.
	c.equal(len(s.Deliveries), 3, "ledger entries")

	return retryWithoutDuplicatingDoc.result(c, env.rows(o), []string{
		"one task key, three notifications, one applied effect",
		"ledger: attempt 1 -> 503, attempt 2 -> 503, attempt 3 -> 200",
	}, started), nil
})

var silentReceiverEscalatesDoc = doc{
	name:  "silent",
	title: "A receiver that never answers",
	explanation: "The destination acknowledges with 200 but never posts a result. For a " +
		"callback step that is not success: the step waits, the deadline passes, and the " +
		"notification is retried. After the budget is spent the task is escalated for a " +
		"human.",
}

// silentReceiverEscalates covers "we never heard back".
var silentReceiverEscalates = newScenario(silentReceiverEscalatesDoc, func(env *Env) (Result, error) {
	started := time.Now()

	inventory := env.AddMock(mockservice.Config{
		Service:       "inventory",
		Action:        "stock.prehold",
		Silent:        true,
		ResultTimeout: 700 * time.Millisecond,
		MaxAttempts:   2,
		BackoffBase:   50 * time.Millisecond,
		BackoffMax:    100 * time.Millisecond,
	})
	if inventory == nil {
		return Result{}, fmt.Errorf("the destination failed to register")
	}

	id, err := env.Submit(order.Spec{
		ID: "ord-demo-silent",
		Steps: []order.StepSpec{{
			Name:  "reserve",
			Phase: order.PhasePrepare,
			Kind:  order.KindCallback,
			Topic: "inventory.stock.prehold",
		}},
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(id, 30*time.Second)
	o := env.Order(id)
	s, err := o.Step("reserve")
	if err != nil {
		return Result{}, err
	}

	c := &collector{}
	c.equal(o.State, order.OrderFailed, "order state")
	c.equal(s.State, order.StateEscalated, "step state")
	c.ok(strings.Contains(s.LastError, "result timeout"),
		"the reason names the missed deadline: %q", s.LastError)

	stats := inventory.Stats()
	c.equal(stats.Notifications, 2, "notifications received before escalation")
	// Even here the effect ran once: a repeated notification is not a new
	// request to do the work again.
	c.equal(stats.Effects, 1, "business effects applied")
	c.equal(len(env.Escalations()), 1, "escalations recorded")

	return silentReceiverEscalatesDoc.result(c, env.rows(o), []string{
		"acknowledged twice, answered never: the step is not done until the result arrives",
	}, started), nil
})

var permanentFailureDoc = doc{
	name:  "permanent",
	title: "A permanent failure is not retried",
	explanation: "The destination rejects the request with 400. The request itself is " +
		"wrong, so repeating it cannot help, and the task escalates after a single " +
		"attempt even though the budget would allow five.",
}

// permanentFailureEscalatesAtOnce covers the "do not retry what cannot work" rule.
var permanentFailureEscalatesAtOnce = newScenario(permanentFailureDoc, func(env *Env) (Result, error) {
	started := time.Now()

	crm := env.AddMock(mockservice.Config{
		Service:     "crm",
		Action:      "contact.update",
		FailForever: true,
		FailStatus:  http.StatusBadRequest,
		MaxAttempts: 5,
		BackoffBase: 50 * time.Millisecond,
	})
	if crm == nil {
		return Result{}, fmt.Errorf("the destination failed to register")
	}

	id, err := env.Submit(order.Spec{
		ID:    "ord-demo-permanent",
		Steps: []order.StepSpec{{Name: "update", Phase: order.PhasePrepare, Kind: order.KindNotify, Topic: "crm.contact.update"}},
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(id, 30*time.Second)
	o := env.Order(id)
	s, err := o.Step("update")
	if err != nil {
		return Result{}, err
	}

	c := &collector{}
	c.equal(o.State, order.OrderFailed, "order state")
	c.equal(s.State, order.StateEscalated, "step state")
	c.equal(s.Attempts, 1, "delivery attempts")
	c.equal(crm.Stats().Notifications, 1, "notifications received")

	return permanentFailureDoc.result(c, env.rows(o),
		[]string{"4xx is the caller's mistake; 5xx and 429 are retried, 4xx is not"}, started), nil
})

var missingEndpointDoc = doc{
	name:  "missing",
	title: "An unregistered destination stops the order",
	explanation: "No service has registered for the topic, so the notification cannot be " +
		"delivered. It is retried in case the destination registers late, then escalated. " +
		"The dependent step is skipped rather than left waiting, so the order fails " +
		"cleanly instead of hanging.",
}

// missingEndpointStopsTheOrder covers an unregistered destination.
var missingEndpointStopsTheOrder = newScenario(missingEndpointDoc, func(env *Env) (Result, error) {
	started := time.Now()

	// Only the identity destination exists; stock, payment, fulfilment and
	// outbound are all unregistered.
	env.AddMock(mockservice.Config{
		Service:     "identity",
		Action:      "verify",
		MaxAttempts: 2,
		BackoffBase: 50 * time.Millisecond,
	})

	id, err := env.Submit(order.Spec{
		ID:     "ord-demo-missing",
		Preset: order.PresetOrder,
	})
	if err != nil {
		return Result{}, err
	}

	env.WaitForOrderTerminal(id, 30*time.Second)
	o := env.Order(id)

	identity, err1 := o.Step(order.StepVerifyIdentity)
	stock, err2 := o.Step(order.StepVerifyStock)
	payment, err3 := o.Step(order.StepVerifyPayment)
	deliver, err4 := o.Step(order.StepProduceDelivery)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return Result{}, fmt.Errorf("order %s is missing its steps", id)
	}

	c := &collector{}
	c.equal(identity.State, order.StateSucceeded, "identity step, which had a destination")
	c.equal(stock.State, order.StateEscalated, "stock step, which had none")
	c.ok(strings.Contains(stock.LastError, "no endpoint registered"),
		"the reason is explicit: %q", stock.LastError)
	c.equal(payment.State, order.StateSkipped, "payment step, blocked by the failure")
	c.equal(deliver.State, order.StatePending, "produce step, never reached")
	c.equal(o.State, order.OrderFailed, "order state")
	c.equal(len(env.Escalations()), 1, "escalations recorded")

	return missingEndpointDoc.result(c, env.rows(o),
		[]string{"the order never entered produce, so fulfilment was never contacted"}, started), nil
})

// compact trims a body so it fits on one line.
func compact(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}
