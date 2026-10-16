package notify

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRegisterAppliesDefaults(t *testing.T) {
	r := NewMemoryRegistry()

	ep, err := r.Register(Endpoint{
		Service: "inventory",
		Action:  "stock.prehold",
		URL:     "https://inv.example.com/prehold",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if ep.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", ep.Method)
	}
	if ep.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", ep.Timeout, DefaultTimeout)
	}
	if ep.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", ep.MaxAttempts, DefaultMaxAttempts)
	}
	if ep.ResultTimeout != DefaultResultTimeout {
		t.Errorf("ResultTimeout = %v, want %v", ep.ResultTimeout, DefaultResultTimeout)
	}
	if len(ep.ExpectStatus) != len(DefaultExpectedStatus) {
		t.Errorf("ExpectStatus = %v, want %v", ep.ExpectStatus, DefaultExpectedStatus)
	}
	if ep.ID == "" {
		t.Error("ID is empty")
	}
	if ep.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if ep.Topic() != "inventory.stock.prehold" {
		t.Errorf("Topic() = %q, want inventory.stock.prehold", ep.Topic())
	}
}

func TestRegisterNormalisesCase(t *testing.T) {
	r := NewMemoryRegistry()

	ep, err := r.Register(Endpoint{
		Service: "Inventory",
		Action:  "Stock.Prehold",
		Method:  "post",
		URL:     "  https://inv.example.com/prehold  ",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if ep.Service != "inventory" || ep.Action != "stock.prehold" {
		t.Errorf("service/action = %q/%q, want inventory/stock.prehold", ep.Service, ep.Action)
	}
	if ep.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", ep.Method)
	}
	if ep.URL != "https://inv.example.com/prehold" {
		t.Errorf("URL = %q, want it trimmed", ep.URL)
	}
}

func TestRegisterReplacesSameTopic(t *testing.T) {
	r := NewMemoryRegistry()

	first, err := r.Register(Endpoint{Service: "crm", Action: "contact.update", URL: "https://crm/v1"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	second, err := r.Register(Endpoint{Service: "crm", Action: "contact.update", URL: "https://crm/v2"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("ID = %q, want it preserved across re-registration (%q)", second.ID, first.ID)
	}
	if len(r.List()) != 1 {
		t.Errorf("registry holds %d endpoints, want 1 after re-registering a topic", len(r.List()))
	}

	got, err := r.ByTopic("crm.contact.update")
	if err != nil {
		t.Fatalf("ByTopic() error = %v", err)
	}
	if got.URL != "https://crm/v2" {
		t.Errorf("URL = %q, want the newest registration", got.URL)
	}
}

func TestByTopicAndUnregister(t *testing.T) {
	r := NewMemoryRegistry()

	if _, err := r.Register(Endpoint{Service: "ads", Action: "registration", URL: "https://ads/hook"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := r.ByTopic("ads.registration"); err != nil {
		t.Fatalf("ByTopic() error = %v", err)
	}
	if _, err := r.ByTopic("ads.nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByTopic() error = %v, want ErrNotFound", err)
	}

	if err := r.Unregister("ads.registration"); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if _, err := r.ByTopic("ads.registration"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByTopic() after removal error = %v, want ErrNotFound", err)
	}
	if err := r.Unregister("ads.registration"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Unregister() error = %v, want ErrNotFound", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name string
		ep   Endpoint
	}{
		{"empty service", Endpoint{Action: "a", URL: "https://x.example.com/h"}},
		{"service with dot", Endpoint{Service: "inv.en", Action: "a", URL: "https://x.example.com/h"}},
		{"empty action", Endpoint{Service: "inv", URL: "https://x.example.com/h"}},
		{"missing url", Endpoint{Service: "inv", Action: "a"}},
		{"bad scheme", Endpoint{Service: "inv", Action: "a", URL: "ftp://x.example.com/h"}},
		{"no host", Endpoint{Service: "inv", Action: "a", URL: "https:///h"}},
		{"relative url", Endpoint{Service: "inv", Action: "a", URL: "/h"}},
		{"get method", Endpoint{Service: "inv", Action: "a", Method: "GET", URL: "https://x.example.com/h"}},
		{"delete method", Endpoint{Service: "inv", Action: "a", Method: "DELETE", URL: "https://x.example.com/h"}},
		{"reserved header", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Headers: []Header{{Name: "Host", Value: "evil"}}}},
		{"length header", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Headers: []Header{{Name: "Content-Length", Value: "10"}}}},
		{"duplicate header", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Headers: []Header{{Name: "X-A", Value: "1"}, {Name: "x-a", Value: "2"}}}},
		{"empty header name", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Headers: []Header{{Value: "1"}}}},
		{"out of range status", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			ExpectStatus: []int{99}}},
		{"bad template syntax", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/{{ .order.id "}},
		{"bad template in header", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Headers: []Header{{Name: "X-A", Value: "{{ nope }}"}}}},
		{"bad template in body", Endpoint{Service: "inv", Action: "a", URL: "https://x.example.com/h",
			Body: "{{ if }}"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMemoryRegistry()

			_, err := r.Register(tc.ep)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Register() error = %v, want ErrInvalid", err)
			}
			if len(r.List()) != 0 {
				t.Error("a rejected registration was stored")
			}
		})
	}
}

func TestRegisterAcceptsTemplatedURL(t *testing.T) {
	r := NewMemoryRegistry()

	ep, err := r.Register(Endpoint{
		Service: "inventory",
		Action:  "stock.prehold",
		URL:     "https://inv.example.com/prehold?order={{ .order.id }}&ref={{ urlquery .order.external_ref }}",
		Body:    `{"order":"{{ .order.id }}"}`,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if ep.URL == "" {
		t.Error("URL is empty")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	ep := Endpoint{BackoffBase: 100 * time.Millisecond, BackoffMax: time.Second}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, time.Second},
		{50, time.Second},
	}
	for _, tc := range tests {
		if got := ep.Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffUsesDefaults(t *testing.T) {
	ep := Endpoint{}

	if got := ep.Backoff(1); got != DefaultBackoffBase {
		t.Errorf("Backoff(1) = %v, want %v", got, DefaultBackoffBase)
	}
	if got := ep.Backoff(99); got != DefaultBackoffMax {
		t.Errorf("Backoff(99) = %v, want the cap %v", got, DefaultBackoffMax)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 128; i++ {
		id := NewID()
		if id == "" {
			t.Fatal("NewID() returned an empty string")
		}
		if seen[id] {
			t.Fatalf("NewID() returned the duplicate %q", id)
		}
		seen[id] = true
	}
}

func TestMemoryRegistryConcurrentAccess(t *testing.T) {
	r := NewMemoryRegistry()
	const workers = 16

	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			ep := Endpoint{
				Service: "svc",
				Action:  "act",
				URL:     "https://x.example.com/h",
			}
			if _, err := r.Register(ep); err != nil {
				t.Errorf("Register() error = %v", err)
				return
			}
			if _, err := r.ByTopic("svc.act"); err != nil {
				t.Errorf("ByTopic() error = %v", err)
			}
			r.List()
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}
