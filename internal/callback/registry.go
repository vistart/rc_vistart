// Package callback implements the callback registry: other services register an
// HTTP endpoint for a topic, and rc_vistart invokes it whenever an event for
// that topic arrives.
package callback

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Errors returned by a Registry implementation.
var (
	// ErrNotFound is returned when no callback is registered under an id.
	ErrNotFound = errors.New("callback: not found")
	// ErrInvalid is returned when a callback fails validation.
	ErrInvalid = errors.New("callback: invalid registration")
)

// Callback is an endpoint registered by an external service.
type Callback struct {
	// ID uniquely identifies the registration. It is assigned by the registry.
	ID string `json:"id"`
	// Topic is the event name this callback subscribes to.
	Topic string `json:"topic"`
	// URL is the absolute http(s) endpoint that is invoked on an event.
	URL string `json:"url"`
	// Secret signs the request body with HMAC-SHA256. An empty secret
	// disables signing.
	Secret string `json:"secret,omitempty"`
	// Timeout bounds a single invocation. Zero means "use the service
	// default".
	Timeout time.Duration `json:"timeout,omitempty"`
	// CreatedAt is the registration time in UTC.
	CreatedAt time.Time `json:"created_at"`
}

// Registry stores callback registrations.
type Registry interface {
	// Register validates and stores a callback, returning the stored value
	// including the assigned ID.
	Register(cb Callback) (Callback, error)
	// List returns every callback registered for topic. An empty topic
	// returns all registrations.
	List(topic string) ([]Callback, error)
	// Get returns the callback registered under id.
	Get(id string) (Callback, error)
	// Delete removes the callback registered under id, or returns
	// ErrNotFound.
	Delete(id string) error
}

// MemoryRegistry is an in-memory Registry safe for concurrent use.
type MemoryRegistry struct {
	mu        sync.RWMutex
	callbacks map[string]Callback
	now       func() time.Time
}

// NewMemoryRegistry returns an empty in-memory registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{
		callbacks: make(map[string]Callback),
		now:       time.Now,
	}
}

// Register implements Registry.
func (r *MemoryRegistry) Register(cb Callback) (Callback, error) {
	cb.Topic = strings.TrimSpace(cb.Topic)
	cb.URL = strings.TrimSpace(cb.URL)
	if err := Validate(cb); err != nil {
		return Callback{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cb.ID, _ = newID() // crypto/rand never fails on supported platforms
	if cb.ID == "" {
		return Callback{}, fmt.Errorf("%w: could not allocate an id", ErrInvalid)
	}
	cb.CreatedAt = r.now().UTC()
	r.callbacks[cb.ID] = cb
	return cb, nil
}

// List implements Registry. Results are sorted by ID so that responses are
// stable between calls.
func (r *MemoryRegistry) List(topic string) ([]Callback, error) {
	topic = strings.TrimSpace(topic)

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Callback, 0, len(r.callbacks))
	for _, cb := range r.callbacks {
		if topic != "" && cb.Topic != topic {
			continue
		}
		out = append(out, cb)
	}
	sortByID(out)
	return out, nil
}

// Get implements Registry.
func (r *MemoryRegistry) Get(id string) (Callback, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cb, ok := r.callbacks[id]
	if !ok {
		return Callback{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return cb, nil
}

// Delete implements Registry.
func (r *MemoryRegistry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.callbacks[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(r.callbacks, id)
	return nil
}

// Validate reports whether cb can be registered. The ID and CreatedAt are
// assigned by the registry and therefore ignored.
func Validate(cb Callback) error {
	if strings.TrimSpace(cb.Topic) == "" {
		return fmt.Errorf("%w: topic must not be empty", ErrInvalid)
	}
	if strings.ContainsAny(cb.Topic, " \t/") {
		return fmt.Errorf("%w: topic %q must not contain whitespace or '/'", ErrInvalid, cb.Topic)
	}
	if strings.TrimSpace(cb.URL) == "" {
		return fmt.Errorf("%w: url must not be empty", ErrInvalid)
	}
	u, err := url.Parse(cb.URL)
	if err != nil {
		return fmt.Errorf("%w: url %q is not a valid URL: %v", ErrInvalid, cb.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: url %q must use the http or https scheme", ErrInvalid, cb.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: url %q must contain a host", ErrInvalid, cb.URL)
	}
	if cb.Timeout < 0 {
		return fmt.Errorf("%w: timeout must not be negative", ErrInvalid)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewID returns a random identifier usable for a callback or a delivery.
func NewID() string {
	id, err := newID()
	if err != nil {
		// Fall back to a time-derived id so that a failing entropy source
		// degrades uniqueness instead of failing the request.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return id
}

func sortByID(cbs []Callback) {
	for i := 1; i < len(cbs); i++ {
		for j := i; j > 0 && cbs[j].ID < cbs[j-1].ID; j-- {
			cbs[j], cbs[j-1] = cbs[j-1], cbs[j]
		}
	}
}
