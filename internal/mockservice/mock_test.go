package mockservice

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubTarget is a minimal stand-in for rc_vistart's registration endpoint.
func stubTarget(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()

	var (
		mu       sync.Mutex
		payloads []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ep-1"}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &payloads
}

func TestRegisterSendsTheAdvertisedShape(t *testing.T) {
	target, payloads := stubTarget(t)

	m, err := New(Config{Target: target.URL, Service: "inventory", Action: "stock.prehold", ResultTimeout: time.Minute})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	if len(*payloads) != 1 {
		t.Fatalf("registered %d times, want 1", len(*payloads))
	}
	body := (*payloads)[0]

	if body["service"] != "inventory" || body["action"] != "stock.prehold" {
		t.Errorf("topic = %v.%v, want inventory.stock.prehold", body["service"], body["action"])
	}
	if body["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", body["method"])
	}
	if url, _ := body["url"].(string); !strings.HasPrefix(url, m.URL()) {
		t.Errorf("url = %v, want it to point at %s", body["url"], m.URL())
	}
	// The result URL is what makes a callback step answerable, so it must be in
	// the default payload.
	if tmpl, _ := body["body"].(string); !strings.Contains(tmpl, ".result_url") {
		t.Errorf("default body = %v, want it to carry the result URL", tmpl)
	}
}

func TestRegisterReportsFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer bad.Close()

	if _, err := New(Config{Target: bad.URL, Service: "s", Action: "a"}); err == nil {
		t.Fatal("New() error = nil, want the registration failure")
	}
}

func TestDeduplicatesOnTheTaskKey(t *testing.T) {
	target, _ := stubTarget(t)
	m, err := New(Config{Target: target.URL, Service: "s", Action: "a"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	// Three notifications for one task key, which is what a retry looks like.
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, m.URL()+"/notify", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("X-Rc-Vistart-Idempotency-Key", "ord_1/step")
		req.Header.Set("X-Rc-Vistart-Delivery", "delivery")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", attempt, resp.StatusCode)
		}
		if attempt == 1 && !strings.Contains(string(body), `"applied":1`) {
			t.Errorf("first response = %s, want applied 1", body)
		}
		if attempt > 1 && !strings.Contains(string(body), `"applied":1`) {
			t.Errorf("attempt %d response = %s, want applied to stay 1", attempt, body)
		}
	}

	stats := m.Stats()
	if stats.Notifications != 3 {
		t.Errorf("Notifications = %d, want 3", stats.Notifications)
	}
	if stats.Effects != 1 {
		t.Errorf("Effects = %d, want 1: a retry must not repeat the effect", stats.Effects)
	}
	if len(stats.Tasks) != 1 || stats.Tasks[0].Attempts != 3 {
		t.Errorf("tasks = %+v, want one task with three attempts", stats.Tasks)
	}
}

func TestFailFirstThenSucceed(t *testing.T) {
	target, _ := stubTarget(t)
	m, err := New(Config{Target: target.URL, Service: "s", Action: "a", FailFirst: 2})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	for attempt, want := range []int{503, 503, 200} {
		resp := notifyOnce(t, m, "ord_1/step")
		if resp != want {
			t.Errorf("attempt %d status = %d, want %d", attempt+1, resp, want)
		}
	}

	if got := m.Effects(); got != 1 {
		t.Errorf("Effects = %d, want 1", got)
	}
}

func TestFailForeverNeverApplies(t *testing.T) {
	target, _ := stubTarget(t)
	m, err := New(Config{Target: target.URL, Service: "s", Action: "a", FailForever: true, FailStatus: http.StatusBadRequest})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	if got := notifyOnce(t, m, "ord_1/step"); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
	if got := m.Effects(); got != 0 {
		t.Errorf("Effects = %d, want 0: a rejected request must not take effect", got)
	}
}

func TestMissingTaskKeyIsRejected(t *testing.T) {
	target, _ := stubTarget(t)
	m, err := New(Config{Target: target.URL, Service: "s", Action: "a"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	resp, err := http.Post(m.URL()+"/notify", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without a task key", resp.StatusCode)
	}
}

func TestLastBodyIsRecorded(t *testing.T) {
	target, _ := stubTarget(t)
	m, err := New(Config{Target: target.URL, Service: "s", Action: "a"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	req, err := http.NewRequest(http.MethodPost, m.URL()+"/notify", strings.NewReader(`{"sku":"SKU-1"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-Rc-Vistart-Idempotency-Key", "ord_1/step")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	resp.Body.Close()

	if got := m.LastBody(); got != `{"sku":"SKU-1"}` {
		t.Errorf("LastBody() = %q, want the body that was sent", got)
	}
}

func notifyOnce(t *testing.T, m *Mock, key string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, m.URL()+"/notify", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-Rc-Vistart-Idempotency-Key", key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}
