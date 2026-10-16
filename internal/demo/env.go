// Package demo runs the typical delivery scenarios against a real rc_vistart
// and real simulated destinations.
//
// Each scenario asserts its expectations, so the demos double as an
// end-to-end check of the reliability contract rather than as decoration.
package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/vistart/rc_vistart/internal/httpapi"
	"github.com/vistart/rc_vistart/internal/mockservice"
	"github.com/vistart/rc_vistart/internal/notify"
	"github.com/vistart/rc_vistart/internal/order"
)

// Check is one assertion inside a scenario.
type Check struct {
	OK     bool
	Detail string
}

// Result is what a scenario reports back.
type Result struct {
	Name        string
	Title       string
	Explanation string
	Passed      bool
	Checks      []Check
	// Steps is the per-step table shown to the reader.
	Steps []StepRow
	// Notes are extra lines worth showing, such as the escalation list.
	Notes []string
	// Duration is how long the scenario took.
	Duration time.Duration
	// Log holds the service log, printed only when a scenario fails.
	Log string
}

// StepRow summarises one step of the order under test.
type StepRow struct {
	Name     string
	Phase    string
	Kind     string
	State    string
	Attempts int
	// Notified is how many notifications that step's destination received,
	// retries included.
	Notified int
	// Applied is how many times the business effect actually ran there.
	Applied int
	Topic   string
	Outcome string
}

// Scenario is one demonstrable situation.
type Scenario struct {
	// Name is the identifier used on the command line and in the Makefile.
	Name string
	// Title is the short human label.
	Title string
	// Explanation says what the scenario demonstrates and what to look for.
	Explanation string
	// Run performs the scenario against a fresh environment.
	Run func(env *Env) (Result, error)
}

// Scenarios returns the demo scenarios in the order they should be shown.
func Scenarios() []Scenario {
	return []Scenario{
		standardOrder,
		simpleNotifications,
		retryWithoutDuplicating,
		silentReceiverEscalates,
		permanentFailureEscalatesAtOnce,
		missingEndpointStopsTheOrder,
	}
}

// Lookup finds a scenario by name.
func Lookup(name string) (Scenario, error) {
	for _, s := range Scenarios() {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, 0, len(Scenarios()))
	for _, s := range Scenarios() {
		names = append(names, s.Name)
	}
	return Scenario{}, fmt.Errorf("demo: unknown scenario %q, known scenarios are %s", name, strings.Join(names, ", "))
}

// Env is a running rc_vistart plus any simulated destinations.
type Env struct {
	Registry *notify.MemoryRegistry
	Stats    *notify.Stats
	Engine   *order.Engine
	BaseURL  string

	server *httptest.Server
	client *http.Client
	mocks  []*mockservice.Mock
	logBuf *bytes.Buffer
	logger *slog.Logger
}

// defaultResultTimeout is generous on purpose. Most scenarios are not testing
// the result deadline, and a mock that answers promptly must not turn a busy
// machine — or the race detector, which is roughly an order of magnitude
// slower — into a spurious retry. A scenario that deliberately exercises the
// deadline sets its own short value.
const defaultResultTimeout = 60 * time.Second

// NewEnv starts rc_vistart on an ephemeral port with an in-memory registry.
//
// Logs are always captured at info level into a buffer that is only printed
// when something fails, so a failing scenario reports what the destinations
// actually saw rather than just the engine's warnings.
func NewEnv() *Env {
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	registry := notify.NewMemoryRegistry()
	stats := notify.NewStats()

	// The invoker is the real HTTP one, so deliveries travel over the loopback
	// interface exactly as they would in production.
	invoker := notify.NewHTTPInvoker(nil, 64<<10)
	engine := order.NewEngine(registry, invoker, stats, order.Config{
		Workers:      4,
		PollInterval: 5 * time.Millisecond,
		// Filled in below, once the listener address is known.
		PublicURL: "",
	}, logger)

	server := httptest.NewServer(httpapi.NewRouter(httpapi.Dependencies{
		Registry:     registry,
		Stats:        stats,
		Engine:       engine,
		Logger:       logger,
		MaxBodyBytes: 1 << 20,
	}))

	env := &Env{
		Registry: registry,
		Stats:    stats,
		Engine:   engine,
		BaseURL:  server.URL,
		server:   server,
		client:   server.Client(),
		logBuf:   logBuf,
		logger:   logger,
	}

	// Receivers are told where to post results through this URL, so it must be
	// the address they can actually reach.
	engine.SetPublicURL(server.URL)
	engine.Start()
	return env
}

// Close stops the service and every simulated destination.
func (e *Env) Close() {
	if e.Engine != nil {
		e.Engine.Stop()
	}
	for _, m := range e.mocks {
		m.Close()
	}
	if e.server != nil {
		e.server.Close()
	}
}

// Logs returns everything the service logged during the scenario.
func (e *Env) Logs() string { return e.logBuf.String() }

// Logger is the service logger, for the mocks to share.
func (e *Env) Logger() *slog.Logger { return e.logger }

// AddMock starts a simulated destination and registers it.
func (e *Env) AddMock(cfg mockservice.Config) *mockservice.Mock {
	cfg.Target = e.BaseURL
	if cfg.Logger == nil {
		cfg.Logger = e.logger
	}
	if cfg.ResultTimeout <= 0 {
		cfg.ResultTimeout = defaultResultTimeout
	}

	m, err := mockservice.New(cfg)
	if err != nil {
		// A registration failure is a scenario bug, and the scenario will fail
		// its checks loudly without the destination.
		e.logger.Error("mock failed to start", slog.String("error", err.Error()))
		return nil
	}
	e.mocks = append(e.mocks, m)
	return m
}

// Submit posts an order and returns the id.
func (e *Env) Submit(spec order.Spec) (string, error) {
	body := map[string]any{
		"id":           spec.ID,
		"external_ref": spec.ExternalRef,
		"subject":      spec.Subject,
		"preset":       spec.Preset,
		"items":        spec.Items,
		"payload":      spec.Payload,
	}
	if len(spec.Steps) > 0 {
		steps := make([]map[string]any, 0, len(spec.Steps))
		for _, s := range spec.Steps {
			steps = append(steps, map[string]any{
				"name":       s.Name,
				"phase":      string(s.Phase),
				"kind":       string(s.Kind),
				"topic":      s.Topic,
				"depends_on": s.DependsOn,
				"optional":   s.Optional,
			})
		}
		body["steps"] = steps
	}

	resp, err := e.Post("/api/v1/orders", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("submitting order: status %s: %s", resp.Status, raw)
	}

	var created order.Order
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Post sends a JSON request to the service.
func (e *Env) Post(path string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return e.client.Do(req)
}

// Get sends a GET request to the service.
func (e *Env) Get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, e.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return e.client.Do(req)
}

// Order fetches an order, or returns the zero value when it is not there yet.
func (e *Env) Order(id string) order.Order {
	o, err := e.Engine.Get(id)
	if err != nil {
		return order.Order{}
	}
	return o
}

// WaitFor polls until cond holds or the budget runs out.
func (e *Env) WaitFor(what string, budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = what
	return false
}

// WaitForOrderTerminal waits until an order reaches a terminal state.
func (e *Env) WaitForOrderTerminal(id string, budget time.Duration) bool {
	return e.WaitFor("order "+id+" to settle", budget, func() bool {
		return e.Order(id).State.Terminal()
	})
}

// Escalations returns the service's escalation list.
func (e *Env) Escalations() []order.Escalation { return e.Engine.Escalations() }

// rows builds the step table, annotating each step with how many times its
// destination was notified and how many times the effect actually ran.
func (e *Env) rows(o order.Order) []StepRow {
	byTopic := make(map[string]*mockservice.Mock, len(e.mocks))
	for _, m := range e.mocks {
		if m == nil {
			continue
		}
		byTopic[m.Topic()] = m
	}

	out := make([]StepRow, 0, len(o.Steps))
	for _, s := range o.Steps {
		row := StepRow{
			Name:     s.Spec.Name,
			Phase:    string(s.Spec.Phase),
			Kind:     string(s.Spec.Kind),
			State:    string(s.State),
			Attempts: s.Attempts,
			Topic:    s.Spec.Topic,
		}
		if len(s.Deliveries) > 0 {
			last := s.Deliveries[len(s.Deliveries)-1]
			row.Outcome = last.Error
			if row.Outcome == "" {
				row.Outcome = fmt.Sprintf("http %d", last.StatusCode)
			}
		}

		if m, ok := byTopic[s.Spec.Topic]; ok {
			row.Notified = m.Stats().Notifications
			row.Applied = m.Effects()
		}
		out = append(out, row)
	}
	return out
}

// collector gathers a scenario's checks.
type collector struct {
	checks []Check
}

func (c *collector) add(ok bool, format string, args ...any) {
	c.checks = append(c.checks, Check{OK: ok, Detail: fmt.Sprintf(format, args...)})
}

func (c *collector) equal(got, want any, what string) {
	c.checks = append(c.checks, Check{
		OK:     fmt.Sprint(got) == fmt.Sprint(want),
		Detail: fmt.Sprintf("%s = %v, want %v", what, got, want),
	})
}

func (c *collector) ok(cond bool, format string, args ...any) {
	c.checks = append(c.checks, Check{OK: cond, Detail: fmt.Sprintf(format, args...)})
}

func (c *collector) passed() bool {
	for _, c := range c.checks {
		if !c.OK {
			return false
		}
	}
	return len(c.checks) > 0
}
