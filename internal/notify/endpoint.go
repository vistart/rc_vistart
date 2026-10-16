package notify

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

// Errors returned by the endpoint registry.
var (
	// ErrNotFound is returned for an unknown topic or id.
	ErrNotFound = errors.New("notify: endpoint not found")
	// ErrInvalid is returned when a registration fails validation.
	ErrInvalid = errors.New("notify: invalid endpoint")
)

// DeliverableMethods are the HTTP methods an endpoint may declare. rc_vistart
// pushes state changes, so verbs that merely fetch are rejected.
var DeliverableMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch}

// DefaultExpectedStatus is used when a registration does not state which
// response codes count as an acknowledgement.
var DefaultExpectedStatus = []int{200, 201, 202, 204}

// reservedHeaders may not be set from a registration template: the transport
// owns them and a template can only produce an invalid request.
var reservedHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
}

// Header is a templated request header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Endpoint is one destination registered by a downstream service.
type Endpoint struct {
	// ID is assigned at registration.
	ID string `json:"id"`
	// Service is the registering service, e.g. "inventory".
	Service string `json:"service"`
	// Action is what the service does, e.g. "stock.prehold".
	Action string `json:"action"`
	// Method is the HTTP method to use.
	Method string `json:"method"`
	// URL is a template expanded per attempt.
	URL string `json:"url"`
	// Headers are templated request headers.
	Headers []Header `json:"headers,omitempty"`
	// Body is a templated request body.
	Body string `json:"body,omitempty"`
	// ExpectStatus lists the response codes that count as an acknowledgement.
	ExpectStatus []int `json:"expect_status,omitempty"`
	// Timeout bounds a single attempt, including the response wait.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Secret is the shared key exposed to templates as .secret and used for
	// the default .signature.
	Secret string `json:"secret,omitempty"`
	// MaxAttempts is the total number of attempts before escalation.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// BackoffBase and BackoffMax bound the exponential retry delay.
	BackoffBase time.Duration `json:"backoff_base,omitempty"`
	BackoffMax  time.Duration `json:"backoff_max,omitempty"`
	// ResultURL is where the receiver posts the step result, when the step
	// needs an acknowledgement beyond the HTTP response.
	ResultURL string `json:"result_url,omitempty"`
	// ResultTimeout bounds how long the receiver has to post that result
	// before the step is retried.
	ResultTimeout time.Duration `json:"result_timeout,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// templates is the compiled form, populated by the registry. It is not
	// part of the wire representation.
	templates []*template.Template
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
var actionRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62}[a-z0-9])?$`)

// Defaults applied when a registration leaves a field empty.
const (
	DefaultTimeout       = 5 * time.Second
	DefaultResultTimeout = 30 * time.Second
	DefaultMaxAttempts   = 3
	DefaultBackoffBase   = 500 * time.Millisecond
	DefaultBackoffMax    = 30 * time.Second
)

// Topic is the routing key: "<service>.<action>".
func (e *Endpoint) Topic() string {
	if e.Service == "" || e.Action == "" {
		return ""
	}
	return e.Service + "." + e.Action
}

// Backoff returns the delay before the given attempt number is retried.
// Attempt 1 is the first try, so attempt 2 waits BackoffBase.
func (e *Endpoint) Backoff(attempt int) time.Duration {
	base, max := e.BackoffBase, e.BackoffMax
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if max <= 0 {
		max = DefaultBackoffMax
	}
	if attempt < 1 {
		attempt = 1
	}

	// Cap the shift before multiplying so a large attempt count cannot
	// overflow the duration.
	shift := attempt - 1
	if shift > 16 {
		shift = 16
	}
	delay := base << uint(shift)
	if delay <= 0 || delay > max {
		delay = max
	}
	return delay
}

func (e *Endpoint) applyDefaults() {
	e.Service = strings.TrimSpace(strings.ToLower(e.Service))
	e.Action = strings.TrimSpace(strings.ToLower(e.Action))
	e.Method = strings.ToUpper(strings.TrimSpace(e.Method))
	e.URL = strings.TrimSpace(e.URL)

	if e.Method == "" {
		e.Method = http.MethodPost
	}
	if len(e.ExpectStatus) == 0 {
		e.ExpectStatus = append([]int(nil), DefaultExpectedStatus...)
	}
	if e.Timeout <= 0 {
		e.Timeout = DefaultTimeout
	}
	if e.MaxAttempts <= 0 {
		e.MaxAttempts = DefaultMaxAttempts
	}
	if e.BackoffBase <= 0 {
		e.BackoffBase = DefaultBackoffBase
	}
	if e.BackoffMax <= 0 {
		e.BackoffMax = DefaultBackoffMax
	}
	if e.BackoffMax < e.BackoffBase {
		e.BackoffMax = e.BackoffBase
	}
	if e.ResultTimeout <= 0 {
		e.ResultTimeout = DefaultResultTimeout
	}
}

func (e *Endpoint) validateFields() error {
	if !nameRE.MatchString(e.Service) {
		return fmt.Errorf("%w: service %q must be 1-64 lower-case letters, digits or '-'", ErrInvalid, e.Service)
	}
	if !actionRE.MatchString(e.Action) {
		return fmt.Errorf("%w: action %q must be 1-64 lower-case letters, digits, '-' or '.'", ErrInvalid, e.Action)
	}
	if e.Method == "" {
		return fmt.Errorf("%w: method must not be empty", ErrInvalid)
	}
	allowed := false
	for _, m := range DeliverableMethods {
		if e.Method == m {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: method %q is not one of %v", ErrInvalid, e.Method, DeliverableMethods)
	}

	// The URL is a template, so only the literal prefix can be parsed. It is
	// enough to prove a scheme and a host are present.
	prefix := e.URL
	if i := strings.IndexAny(prefix, "{"); i >= 0 {
		prefix = prefix[:i]
	}
	u, err := url.Parse(prefix)
	if err != nil {
		return fmt.Errorf("%w: url %q is not parseable: %v", ErrInvalid, e.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: url %q must use the http or https scheme", ErrInvalid, e.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: url %q must contain a host", ErrInvalid, e.URL)
	}

	seen := make(map[string]bool, len(e.Headers))
	for i, h := range e.Headers {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return fmt.Errorf("%w: headers[%d] has an empty name", ErrInvalid, i)
		}
		if reservedHeaders[strings.ToLower(name)] {
			return fmt.Errorf("%w: header %q is reserved", ErrInvalid, name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if seen[canonical] {
			return fmt.Errorf("%w: header %q is declared twice", ErrInvalid, name)
		}
		seen[canonical] = true
	}

	if len(e.ExpectStatus) == 0 {
		return fmt.Errorf("%w: expect_status must list at least one status code", ErrInvalid)
	}
	for _, code := range e.ExpectStatus {
		if code < 100 || code > 599 {
			return fmt.Errorf("%w: expect_status contains the out-of-range code %d", ErrInvalid, code)
		}
	}
	if e.Timeout <= 0 {
		return fmt.Errorf("%w: timeout must be positive", ErrInvalid)
	}
	if e.MaxAttempts < 1 {
		return fmt.Errorf("%w: max_attempts must be at least 1", ErrInvalid)
	}
	if e.ResultTimeout <= 0 {
		return fmt.Errorf("%w: result_timeout must be positive", ErrInvalid)
	}
	return nil
}

// Registry stores the endpoints downstream services registered.
type Registry interface {
	Register(ep Endpoint) (Endpoint, error)
	// ByTopic returns the endpoint registered for a topic.
	ByTopic(topic string) (Endpoint, error)
	// List returns every registration.
	List() []Endpoint
	// Unregister removes the registration for a topic and returns ErrNotFound
	// if there was none.
	Unregister(topic string) error
}

// MemoryRegistry is an in-memory Registry safe for concurrent use.
type MemoryRegistry struct {
	mu    sync.RWMutex
	byID  map[string]Endpoint
	now   func() time.Time
	newID func() string
}

// NewMemoryRegistry returns an empty registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{
		byID:  make(map[string]Endpoint),
		now:   time.Now,
		newID: NewID,
	}
}

// Register implements Registry.
func (r *MemoryRegistry) Register(ep Endpoint) (Endpoint, error) {
	ep.applyDefaults()

	// Normalise into a fresh slice. Assigning over ep.Headers first would read
	// from the zero value just written and drop every header.
	headers := make([]Header, len(ep.Headers))
	for i, h := range ep.Headers {
		headers[i] = Header{
			Name:  strings.TrimSpace(h.Name),
			Value: strings.TrimSpace(h.Value),
		}
	}
	ep.Headers = headers
	ep.URL = strings.TrimSpace(ep.URL)
	ep.Body = strings.TrimSpace(ep.Body)

	if err := compileEndpoint(&ep); err != nil {
		return Endpoint{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-registering the same topic replaces the previous registration, so a
	// service can rotate its URL or templates without creating a second
	// destination for the same task. The id and creation time are preserved.
	for _, existing := range r.byID {
		if existing.Topic() == ep.Topic() {
			ep.ID = existing.ID
			ep.CreatedAt = existing.CreatedAt
			break
		}
	}
	if ep.ID == "" {
		ep.ID = r.newID()
		ep.CreatedAt = r.now().UTC()
	}
	r.byID[ep.ID] = ep
	return ep, nil
}

// ByTopic implements Registry.
func (r *MemoryRegistry) ByTopic(topic string) (Endpoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ep := range r.byID {
		if ep.Topic() == topic {
			return ep, nil
		}
	}
	return Endpoint{}, fmt.Errorf("%w: topic %q", ErrNotFound, topic)
}

// Unregister implements Registry.
func (r *MemoryRegistry) Unregister(topic string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, ep := range r.byID {
		if ep.Topic() == topic {
			delete(r.byID, id)
			return nil
		}
	}
	return fmt.Errorf("%w: topic %q", ErrNotFound, topic)
}

// List implements Registry.
func (r *MemoryRegistry) List() []Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Endpoint, 0, len(r.byID))
	for _, ep := range r.byID {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic() < out[j].Topic() })
	return out
}

// NewID returns a random identifier used for endpoint ids, order ids and
// delivery ids.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
