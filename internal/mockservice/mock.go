// Package mockservice simulates a downstream service that registers an
// endpoint with rc_vistart and waits to be notified.
//
// It exists to exercise the delivery contract rather than to be useful. It can
// be told to fail, to stay silent, and to deduplicate, which together cover
// the retry, result-timeout and duplicate-delivery paths.
package mockservice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultBodyTemplate carries the order identity, the result URL without which a
// callback step cannot report back, and the caller's payload, which is what a
// real destination needs in order to act.
const DefaultBodyTemplate = `{"order_id":"{{ .order.id }}",` +
	`"step":"{{ .step.name }}",` +
	`"delivery_id":"{{ .delivery_id }}",` +
	`"idempotency_key":"{{ .idempotency_key }}",` +
	`"result_url":"{{ .result_url }}",` +
	`"items":{{ json .items }},` +
	`"payload":{{ json .payload }}}`

// Config describes one simulated destination.
type Config struct {
	// Target is the rc_vistart base URL this mock registers with.
	Target string
	// Service and Action form the topic, "<service>.<action>".
	Service string
	Action  string

	Method   string
	Path     string
	Secret   string
	BodyTmpl string

	// PublicURL overrides the advertised base URL. Useful when the mock must
	// be reachable under a different address than it listens on.
	PublicURL string
	// ListenAddr pins the listening address. When empty the mock takes an
	// ephemeral port, which is what the demo driver wants.
	ListenAddr string

	// FailFirst rejects the first N attempts of each task, then succeeds.
	FailFirst int
	// FailForever rejects every attempt.
	FailForever bool
	// FailStatus is the status returned when failing. Defaults to 503, which
	// is retryable; set 400 to demonstrate a permanent failure.
	FailStatus int
	// Silent acknowledges the notification but never posts a result, which is
	// what a receiver that accepts work and then goes quiet looks like.
	Silent bool

	Timeout       time.Duration
	ResultTimeout time.Duration
	MaxAttempts   int
	BackoffBase   time.Duration
	BackoffMax    time.Duration

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Method == "" {
		c.Method = http.MethodPost
	}
	if c.Path == "" {
		c.Path = "/notify"
	}
	if c.BodyTmpl == "" {
		c.BodyTmpl = DefaultBodyTemplate
	}
	if c.FailStatus == 0 {
		c.FailStatus = http.StatusServiceUnavailable
	}
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.ResultTimeout <= 0 {
		c.ResultTimeout = 10 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 300 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 3 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c.Target = strings.TrimRight(c.Target, "/")
}

// TaskStat is what one task key looks like from the receiving side.
type TaskStat struct {
	TaskKey string `json:"task_key"`
	// Attempts counts notifications received, retries included.
	Attempts int `json:"attempts"`
	// Applied is 1 once the effect ran for this task, and stays 1 no matter
	// how many times the notification was repeated.
	Applied int `json:"applied"`
}

// Stats is the receiving-side view of a whole run.
type Stats struct {
	Tasks []TaskStat `json:"tasks"`
	Count int        `json:"count"`
	// Effects is the total number of business effects applied.
	Effects int `json:"effects"`
	// Notifications is the total number of notifications received, retries
	// included. Effects must never exceed this, and per task must never
	// exceed 1.
	Notifications int `json:"notifications"`
}

// Mock is a running simulated destination.
type Mock struct {
	cfg  Config
	srv  *httptest.Server
	http *http.Server
	ln   net.Listener
	host string

	client *http.Client

	mu       sync.Mutex
	attempts map[string]int
	applied  map[string]int
	effects  int
	// lastBody records the most recent rendered body, which is what the demo
	// prints to show what the destination actually received.
	lastBody string
}

// delivery is what the mock reads out of an incoming notification.
type delivery struct {
	OrderID    string `json:"order_id"`
	Step       string `json:"step"`
	DeliveryID string `json:"delivery_id"`
	ResultURL  string `json:"result_url"`
}

// New builds a mock from a config and registers it with rc_vistart.
func New(cfg Config) (*Mock, error) {
	cfg.applyDefaults()

	m := &Mock{
		cfg:      cfg,
		attempts: make(map[string]int),
		applied:  make(map[string]int),
		client:   &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, m.handleNotify)
	mux.HandleFunc("/state", m.handleState)

	// A pinned address keeps the advertised URL predictable for manual runs;
	// an ephemeral one avoids port clashes when the demo drives everything.
	if cfg.ListenAddr != "" {
		ln, err := net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
		}
		m.ln = ln
		m.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = m.http.Serve(ln) }()
		m.host = "http://" + ln.Addr().String()
	} else {
		m.srv = httptest.NewServer(mux)
		m.host = m.srv.URL
	}

	advertised := cfg.PublicURL
	if advertised == "" {
		advertised = m.host
	}

	if err := m.register(advertised); err != nil {
		m.srv.Close()
		return nil, err
	}
	return m, nil
}

// Topic is the topic this mock registers for.
func (m *Mock) Topic() string { return m.cfg.Service + "." + m.cfg.Action }

// URL is the address the mock listens on.
func (m *Mock) URL() string { return m.host }

// Close stops the mock.
func (m *Mock) Close() {
	if m.srv != nil {
		m.srv.Close()
	}
	if m.http != nil {
		_ = m.http.Close()
	}
}

// Stats returns a snapshot of what the mock has seen.
func (m *Mock) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.attempts))
	for k := range m.attempts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := Stats{Tasks: make([]TaskStat, 0, len(keys)), Effects: m.effects}
	for _, k := range keys {
		out.Tasks = append(out.Tasks, TaskStat{TaskKey: k, Attempts: m.attempts[k], Applied: m.applied[k]})
		out.Notifications += m.attempts[k]
	}
	out.Count = len(out.Tasks)
	return out
}

// LastBody is the most recent request body the mock received.
func (m *Mock) LastBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBody
}

// Effects is the number of business effects this mock applied in total.
func (m *Mock) Effects() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effects
}

func (m *Mock) register(advertised string) error {
	payload := map[string]any{
		"service":        m.cfg.Service,
		"action":         m.cfg.Action,
		"method":         m.cfg.Method,
		"url":            advertised + m.cfg.Path,
		"body":           m.cfg.BodyTmpl,
		"headers":        []map[string]string{{"name": "Content-Type", "value": "application/json"}},
		"expect_status":  []int{200, 202, 204},
		"timeout":        m.cfg.Timeout.String(),
		"result_timeout": m.cfg.ResultTimeout.String(),
		"max_attempts":   m.cfg.MaxAttempts,
		"backoff_base":   m.cfg.BackoffBase.String(),
		"backoff_max":    m.cfg.BackoffMax.String(),
	}
	if m.cfg.Secret != "" {
		payload["secret"] = m.cfg.Secret
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := http.Post(m.cfg.Target+"/api/v1/endpoints", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("registering %s: status %s: %s", m.Topic(), resp.Status, body)
	}
	m.cfg.Logger.Info("registered topic", slog.String("topic", m.Topic()))
	return nil
}

func (m *Mock) handleNotify(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	// The task identity is the header rc_vistart always sets; the payload is
	// whatever this destination's own format happens to carry.
	key := r.Header.Get("X-Rc-Vistart-Idempotency-Key")
	if key == "" {
		http.Error(w, "missing idempotency key", http.StatusBadRequest)
		return
	}

	var d delivery
	_ = json.Unmarshal(raw, &d)
	if d.DeliveryID == "" {
		d.DeliveryID = r.Header.Get("X-Rc-Vistart-Delivery")
	}

	m.mu.Lock()
	m.attempts[key]++
	attempt := m.attempts[key]
	m.lastBody = string(raw)
	m.mu.Unlock()

	m.cfg.Logger.Info("notification received",
		slog.String("task_key", key),
		slog.String("step", r.Header.Get("X-Rc-Vistart-Step")),
		slog.Int("attempt", attempt),
		slog.String("delivery_id", d.DeliveryID),
	)

	switch {
	case m.cfg.FailForever:
		http.Error(w, "mock is configured to fail", m.cfg.FailStatus)
		return
	case attempt <= m.cfg.FailFirst:
		http.Error(w, "mock is configured to fail the first attempts", m.cfg.FailStatus)
		return
	}

	// Anti-duplication is the receiver's responsibility, because only the
	// receiver can tell a retry from a new task without trusting the caller.
	// The task key is stable across retries, so applying the effect on the
	// first arrival and ignoring every later one is exactly what stops a retry
	// from duplicating business work.
	m.mu.Lock()
	first := m.applied[key] == 0
	if first {
		m.applied[key] = 1
		m.effects++
	}
	applied := m.applied[key]
	effects := m.effects
	m.mu.Unlock()

	if !first {
		m.cfg.Logger.Info("duplicate notification ignored",
			slog.String("task_key", key),
			slog.String("reason", "the effect was already applied"),
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"received":true,"applied":%d,"effects":%d}`, applied, effects)

	if m.cfg.Silent || d.ResultURL == "" {
		return
	}

	// Report back, as a callback step requires. The delivery id is what ties
	// the answer to this attempt.
	go func() {
		if err := m.postResult(d, applied); err != nil {
			m.cfg.Logger.Warn("could not post result",
				slog.String("task_key", key),
				slog.String("error", err.Error()),
			)
		}
	}()
}

func (m *Mock) postResult(d delivery, applied int) error {
	payload, err := json.Marshal(map[string]any{
		"delivery_id": d.DeliveryID,
		"success":     true,
		"data":        map[string]any{"ok": true, "applied": applied},
	})
	if err != nil {
		return err
	}

	resp, err := m.client.Post(d.ResultURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	m.cfg.Logger.Info("result posted", slog.String("url", d.ResultURL), slog.String("status", resp.Status))
	return nil
}

// handleState exposes the receiving-side view over HTTP.
func (m *Mock) handleState(w http.ResponseWriter, r *http.Request) {
	stats := m.Stats()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(stats)
}
