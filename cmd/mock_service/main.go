// Command mock_service simulates a downstream service (inventory, CRM, an ad
// network, ...) that registers its endpoint with rc_vistart and waits to be
// notified.
//
// It exists to exercise the contract rather than to be useful: it can be told
// to fail, to stay silent, and to deduplicate, which together cover the retry,
// result-timeout and duplicate-delivery paths.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// delivery is what the mock reads out of an incoming notification.
type delivery struct {
	OrderID     string `json:"order_id"`
	Step        string `json:"step"`
	DeliveryID  string `json:"delivery_id"`
	ResultURL   string `json:"result_url"`
	Idempotency string `json:"-"`
}

type server struct {
	target      string
	service     string
	action      string
	method      string
	publicURL   string
	path        string
	bodyTmpl    string
	secret      string
	resultValue string

	failFirst   int
	failForever bool
	silent      bool

	mu sync.Mutex
	// attempts counts notifications received, retries included.
	attempts map[string]int
	// applied counts how many times the business effect actually ran, which
	// must never exceed one per task key.
	applied map[string]int
	// effects is the total number of effects applied, across all tasks.
	effects int
}

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:9001", "listen address")
		target      = flag.String("target", "http://127.0.0.1:8080", "rc_vistart base URL")
		service     = flag.String("service", "inventory", "service name to register")
		action      = flag.String("action", "stock.prehold", "action name to register")
		method      = flag.String("method", "POST", "HTTP method to advertise")
		path        = flag.String("path", "/notify", "path this service serves")
		publicURL   = flag.String("public-url", "", "URL to advertise (defaults to http://<addr>)")
		secret      = flag.String("secret", "", "shared secret, exposed to templates as .secret")
		bodyTmpl    = flag.String("body", "", "body template to advertise (defaults to a JSON one carrying the result URL)")
		failFirst   = flag.Int("fail-first", 0, "fail the first N attempts of each task, then succeed")
		failForever = flag.Bool("fail-forever", false, "always fail with 500, to demonstrate escalation")
		silent      = flag.Bool("silent", false, "acknowledge but never post a result, to demonstrate the result timeout")
		timeout     = flag.Duration("result-timeout", 10*time.Second, "advertised result timeout")
		maxAttempts = flag.Int("max-attempts", 3, "advertised attempt budget")
		backoffBase = flag.Duration("backoff-base", 300*time.Millisecond, "advertised first retry delay")
		backoffMax  = flag.Duration("backoff-max", 3*time.Second, "advertised maximum retry delay")
	)
	flag.Parse()

	advertised := *publicURL
	if advertised == "" {
		advertised = "http://" + *addr
	}

	body := *bodyTmpl
	if body == "" {
		// The receiver needs the result URL to be able to answer, so the
		// default payload carries it.
		body = `{"order_id":"{{ .order.id }}",` +
			`"step":"{{ .step.name }}",` +
			`"delivery_id":"{{ .delivery_id }}",` +
			`"idempotency_key":"{{ .idempotency_key }}",` +
			`"result_url":"{{ .result_url }}",` +
			`"items":{{ json .items }}}`
	}

	s := &server{
		target:      strings.TrimRight(*target, "/"),
		service:     *service,
		action:      *action,
		method:      *method,
		publicURL:   advertised,
		path:        *path,
		bodyTmpl:    body,
		secret:      *secret,
		failFirst:   *failFirst,
		failForever: *failForever,
		silent:      *silent,
		attempts:    make(map[string]int),
		applied:     make(map[string]int),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(*path, s.handleNotify)
	mux.HandleFunc("/state", s.handleState)

	registration := map[string]any{
		"service":        s.service,
		"action":         s.action,
		"method":         s.method,
		"url":            s.publicURL + s.path,
		"body":           s.bodyTmpl,
		"headers":        []map[string]string{{"name": "Content-Type", "value": "application/json"}},
		"expect_status":  []int{200, 202, 204},
		"timeout":        "3s",
		"result_timeout": timeout.String(),
		"max_attempts":   *maxAttempts,
		"backoff_base":   backoffBase.String(),
		"backoff_max":    backoffMax.String(),
	}
	if s.secret != "" {
		registration["secret"] = s.secret
	}
	if err := s.register(registration); err != nil {
		log.Fatalf("could not register with %s: %v", s.target, err)
	}

	log.Printf("mock %s.%s listening on %s%s (fail-first=%d fail-forever=%v silent=%v)",
		s.service, s.action, *addr, s.path, s.failFirst, s.failForever, s.silent)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) register(payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := http.Post(s.target+"/api/v1/endpoints", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("status %s: %s", resp.Status, body)
	}
	log.Printf("registered topic %s.%s: %s", s.service, s.action, bytes.TrimSpace(body))
	return nil
}

func (s *server) handleNotify(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	// The task identity is the header rc_vistart always sets; the payload is
	// whatever this service's own format happens to carry.
	key := r.Header.Get("X-Rc-Vistart-Idempotency-Key")
	if key == "" {
		http.Error(w, "missing idempotency key", http.StatusBadRequest)
		return
	}

	var d delivery
	_ = json.Unmarshal(raw, &d)
	if d.ResultURL == "" {
		d.ResultURL = r.Header.Get("X-Rc-Vistart-Result-Url")
	}
	if d.DeliveryID == "" {
		d.DeliveryID = r.Header.Get("X-Rc-Vistart-Delivery")
	}

	s.mu.Lock()
	s.attempts[key]++
	attempt := s.attempts[key]
	s.mu.Unlock()

	log.Printf("received attempt=%d task=%s order=%s step=%s delivery=%s",
		attempt, key, r.Header.Get("X-Rc-Vistart-Order"), r.Header.Get("X-Rc-Vistart-Step"), d.DeliveryID)

	switch {
	case s.failForever:
		log.Printf("failing on purpose (fail-forever) task=%s", key)
		http.Error(w, "mock is configured to fail", http.StatusInternalServerError)
		return
	case attempt <= s.failFirst:
		log.Printf("failing on purpose attempt=%d/%d task=%s", attempt, s.failFirst, key)
		http.Error(w, "mock is configured to fail the first attempts", http.StatusServiceUnavailable)
		return
	}

	// Anti-duplication is the receiver's responsibility, because only the
	// receiver can tell a retry from a new task without trusting the caller.
	// The task key is stable across retries, so applying the effect on the
	// first arrival and ignoring every later one is exactly the behaviour
	// that prevents a retry from duplicating business work.
	s.mu.Lock()
	first := s.applied[key] == 0
	if first {
		s.applied[key] = 1
		s.effects++
	}
	applied := s.applied[key]
	s.mu.Unlock()

	if !first {
		log.Printf("duplicate task=%s ignored, the effect was already applied", key)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"received":true,"applied":%d,"effects":%d}`, applied, s.totalEffects())

	if s.silent || d.ResultURL == "" {
		return
	}

	// Report back, as a callback step requires. The delivery id is what ties
	// the answer to this attempt.
	go func() {
		if err := s.postResult(d, applied); err != nil {
			log.Printf("could not post result for task=%s: %v", key, err)
		}
	}()
}

func (s *server) totalEffects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effects
}

func (s *server) postResult(d delivery, applied int) error {
	payload, err := json.Marshal(map[string]any{
		"delivery_id": d.DeliveryID,
		"success":     true,
		"data": map[string]any{
			"ok":      true,
			"applied": applied,
		},
	})
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(d.ResultURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	log.Printf("posted result to %s -> %s %s", d.ResultURL, resp.Status, bytes.TrimSpace(body))
	return nil
}

// handleState reports what the mock has seen, which is how a demo verifies
// that a retried task was applied exactly once.
func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	type taskView struct {
		TaskKey string `json:"task_key"`
		// Attempts counts notifications received, retries included.
		Attempts int `json:"attempts"`
		// Applied is 1 when the effect ran for this task and 0 when it has not.
		Applied int `json:"applied"`
	}
	keys := make([]string, 0, len(s.attempts))
	for k := range s.attempts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	views := make([]taskView, 0, len(keys))
	for _, k := range keys {
		views = append(views, taskView{TaskKey: k, Attempts: s.attempts[k], Applied: s.applied[k]})
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"tasks":   views,
		"count":   len(views),
		"effects": s.effects,
	})
}
