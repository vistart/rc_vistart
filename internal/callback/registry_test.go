package callback

import (
	"errors"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *MemoryRegistry {
	t.Helper()
	r := NewMemoryRegistry()
	r.now = func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }
	return r
}

func TestRegisterAssignsIDAndTimestamp(t *testing.T) {
	r := newTestRegistry(t)

	cb, err := r.Register(Callback{Topic: "order.created", URL: "https://svc.example.com/hooks"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if cb.ID == "" {
		t.Error("Register() left the ID empty")
	}
	if !cb.CreatedAt.Equal(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want the injected clock value", cb.CreatedAt)
	}

	got, err := r.Get(cb.ID)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", cb.ID, err)
	}
	if got != cb {
		t.Errorf("Get() = %+v, want %+v", got, cb)
	}
}

func TestRegisterTrimsInput(t *testing.T) {
	r := newTestRegistry(t)

	cb, err := r.Register(Callback{Topic: "  order.created  ", URL: "  https://svc.example.com/hooks  "})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if cb.Topic != "order.created" {
		t.Errorf("Topic = %q, want %q", cb.Topic, "order.created")
	}
	if cb.URL != "https://svc.example.com/hooks" {
		t.Errorf("URL = %q, want %q", cb.URL, "https://svc.example.com/hooks")
	}
}

func TestRegisterRejectsInvalidCallbacks(t *testing.T) {
	tests := []struct {
		name string
		cb   Callback
	}{
		{"empty topic", Callback{URL: "https://svc.example.com/hooks"}},
		{"blank topic", Callback{Topic: "   ", URL: "https://svc.example.com/hooks"}},
		{"topic with slash", Callback{Topic: "order/created", URL: "https://svc.example.com/hooks"}},
		{"topic with space", Callback{Topic: "order created", URL: "https://svc.example.com/hooks"}},
		{"empty url", Callback{Topic: "order.created"}},
		{"relative url", Callback{Topic: "order.created", URL: "/hooks"}},
		{"unsupported scheme", Callback{Topic: "order.created", URL: "ftp://svc.example.com/hooks"}},
		{"missing host", Callback{Topic: "order.created", URL: "https:///hooks"}},
		{"negative timeout", Callback{Topic: "order.created", URL: "https://svc.example.com/hooks", Timeout: -time.Second}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRegistry(t)

			_, err := r.Register(tc.cb)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Register() error = %v, want ErrInvalid", err)
			}
			if cbs, _ := r.List(""); len(cbs) != 0 {
				t.Errorf("registry holds %d callbacks, want 0 after a rejected registration", len(cbs))
			}
		})
	}
}

func TestRegisterRejectsUnparseableURL(t *testing.T) {
	if err := Validate(Callback{Topic: "order.created", URL: "://nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() error = %v, want ErrInvalid", err)
	}
}

func TestListFiltersByTopicAndIsSorted(t *testing.T) {
	r := newTestRegistry(t)

	// Register in an order that differs from the ID order so the sort is
	// actually observable.
	for _, tc := range []struct{ topic, url string }{
		{"order.created", "https://a.example.com/1"},
		{"order.paid", "https://b.example.com/2"},
		{"order.created", "https://c.example.com/3"},
	} {
		if _, err := r.Register(Callback{Topic: tc.topic, URL: tc.url}); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}

	all, err := r.List("")
	if err != nil {
		t.Fatalf("List(\"\") error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") returned %d callbacks, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID > all[i].ID {
			t.Fatalf("List(\"\") is not sorted by ID: %q before %q", all[i-1].ID, all[i].ID)
		}
	}

	created, err := r.List("order.created")
	if err != nil {
		t.Fatalf("List(\"order.created\") error = %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("List(\"order.created\") returned %d callbacks, want 2", len(created))
	}
	for _, cb := range created {
		if cb.Topic != "order.created" {
			t.Errorf("List(\"order.created\") returned topic %q", cb.Topic)
		}
	}

	unknown, err := r.List("nothing.here")
	if err != nil {
		t.Fatalf("List(\"nothing.here\") error = %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("List(\"nothing.here\") returned %d callbacks, want 0", len(unknown))
	}
}

func TestGetUnknownID(t *testing.T) {
	r := newTestRegistry(t)

	if _, err := r.Get("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	r := newTestRegistry(t)

	cb, err := r.Register(Callback{Topic: "order.created", URL: "https://svc.example.com/hooks"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if err := r.Delete(cb.ID); err != nil {
		t.Fatalf("Delete(%q) error = %v", cb.ID, err)
	}
	if _, err := r.Get(cb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(%q) after delete error = %v, want ErrNotFound", cb.ID, err)
	}
	if err := r.Delete(cb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete(%q) error = %v, want ErrNotFound", cb.ID, err)
	}
}

func TestMemoryRegistryConcurrentAccess(t *testing.T) {
	r := NewMemoryRegistry()
	const workers = 16

	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			cb, err := r.Register(Callback{Topic: "order.created", URL: "https://svc.example.com/hooks"})
			if err != nil {
				t.Errorf("Register() error = %v", err)
				return
			}
			if _, err := r.List("order.created"); err != nil {
				t.Errorf("List() error = %v", err)
			}
			if _, err := r.Get(cb.ID); err != nil {
				t.Errorf("Get() error = %v", err)
			}
			if err := r.Delete(cb.ID); err != nil {
				t.Errorf("Delete() error = %v", err)
			}
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}

	if cbs, _ := r.List(""); len(cbs) != 0 {
		t.Errorf("registry holds %d callbacks, want 0", len(cbs))
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
