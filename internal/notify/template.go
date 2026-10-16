// Package notify delivers HTTP notifications to the endpoints other services
// registered, retrying until the endpoint acknowledges or the attempt budget is
// exhausted.
package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
)

// Item is one line of an order.
type Item struct {
	SKU      string  `json:"sku"`
	Name     string  `json:"name"`
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
}

// ContextInput is everything a template may reference when rendering a request.
// Templates see it as nested maps with lower-case keys, so the names documented
// in the README are exactly the names a template must use.
type ContextInput struct {
	OrderID     string
	ExternalRef string
	Subject     string
	Preset      string
	Phase       string
	CreatedAt   time.Time

	StepName   string
	StepKind   string
	Topic      string
	Service    string
	Action     string
	Attempt    int
	DeliveryID string
	// IdempotencyKey identifies the task, not the attempt: it stays the same
	// across retries so the receiver can deduplicate.
	IdempotencyKey string

	ResultURL string
	Secret    string
	Timestamp time.Time

	Items   []Item
	Payload map[string]any
}

// Rendered is a fully expanded request.
type Rendered struct {
	Method  string
	URL     string
	Headers []Header
	Body    []byte
	// Signature is the HMAC-SHA256 hex digest of "<unix>.<body>", also exposed
	// to templates as .signature.
	Signature string
}

func (in ContextInput) data() map[string]any {
	items := make([]map[string]any, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, map[string]any{
			"sku":      it.SKU,
			"name":     it.Name,
			"quantity": it.Quantity,
			"price":    it.Price,
		})
	}

	return map[string]any{
		"order": map[string]any{
			"id":           in.OrderID,
			"external_ref": in.ExternalRef,
			"subject":      in.Subject,
			"preset":       in.Preset,
			"phase":        in.Phase,
			"created_at":   in.CreatedAt.UTC().Format(time.RFC3339),
		},
		"step": map[string]any{
			"name":    in.StepName,
			"kind":    in.StepKind,
			"topic":   in.Topic,
			"service": in.Service,
			"action":  in.Action,
			"attempt": in.Attempt,
		},
		"delivery_id":     in.DeliveryID,
		"idempotency_key": in.IdempotencyKey,
		"result_url":      in.ResultURL,
		"secret":          in.Secret,
		"timestamp":       in.Timestamp.UTC().Format(time.RFC3339),
		"unix":            in.Timestamp.Unix(),
		"items":           items,
		"payload":         in.Payload,
	}
}

// funcs are available to every template.
var funcs = template.FuncMap{
	// json encodes a value as compact JSON, for building payloads by hand.
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	},
	// urlquery escapes a value so it is safe inside a query string.
	"urlquery": url.QueryEscape,
	// hmacSHA256 returns the hex HMAC-SHA256 of msg keyed by secret.
	"hmacSHA256": func(secret, msg string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(msg))
		return hex.EncodeToString(mac.Sum(nil))
	},
	// default returns fallback when value is empty.
	"default": func(fallback, value any) any {
		if isEmpty(value) {
			return fallback
		}
		return value
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"trim":  strings.TrimSpace,
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	}
	return false
}

func compile(name, text string, missingKey string) (*template.Template, error) {
	return template.New(name).Funcs(funcs).Option("missingkey=" + missingKey).Parse(text)
}

func execute(t *template.Template, data map[string]any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// reservedInFirstPass are only defined once the body exists, so they cannot
// appear in the URL or the body: .signature covers the body, which makes
// referencing it from the body a guaranteed runtime failure. Only header
// templates may use them.
var reservedInFirstPass = []string{"body", "signature"}

// referencesReserved reports whether a template reads any of the given
// top-level keys, by walking its parse tree.
func referencesReserved(t *template.Template, names ...string) string {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}

	var found string
	seen := make(map[parse.Node]bool)

	var walk func(parse.Node)
	walk = func(n parse.Node) {
		if n == nil || found != "" || seen[n] {
			return
		}
		seen[n] = true

		switch node := n.(type) {
		case *parse.ListNode:
			for _, c := range node.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(node.Pipe)
		case *parse.PipeNode:
			for _, c := range node.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range node.Args {
				walk(a)
			}
		case *parse.FieldNode:
			if len(node.Ident) > 0 && wanted[node.Ident[0]] {
				found = node.Ident[0]
			}
		case *parse.IfNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		case *parse.RangeNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		case *parse.WithNode:
			walk(node.Pipe)
			walk(node.List)
			walk(node.ElseList)
		case *parse.TemplateNode:
			walk(node.Pipe)
		}
	}

	walk(t.Tree.Root)
	return found
}

// compileEndpoint parses every template of an endpoint and renders them once
// against sample data. Parsing alone would miss runtime errors such as a
// pipeline with a wrong type, so the dry run is what makes registration a
// meaningful validation gate.
func compileEndpoint(e *Endpoint) error {
	if err := e.validateFields(); err != nil {
		return err
	}

	fields := make([]struct {
		name string
		text string
	}, 0, len(e.Headers)+2)

	fields = append(fields, struct{ name, text string }{"url", e.URL})
	for i, h := range e.Headers {
		fields = append(fields, struct{ name, text string }{
			name: fmt.Sprintf("header[%d] %s", i, h.Name),
			text: h.Value,
		})
	}
	if e.Body != "" {
		fields = append(fields, struct{ name, text string }{"body", e.Body})
	}

	compiled := make([]*template.Template, 0, len(fields))
	// The strict set backs real deliveries, where a typo in a payload key must
	// fail loudly rather than send a blank field to a vendor.
	for _, f := range fields {
		t, err := compile(f.name, f.text, "error")
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalid, f.name, err)
		}
		compiled = append(compiled, t)
	}

	// The dry run renders against a synthetic payload, which by definition
	// lacks the keys a real order would carry. It therefore uses a lenient set
	// so that referencing .payload.something is not treated as a defect, while
	// still catching type errors that no payload could ever satisfy.
	dry := make([]*template.Template, 0, len(fields))
	for _, f := range fields {
		t, err := compile(f.name, f.text, "zero")
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalid, f.name, err)
		}
		dry = append(dry, t)
	}

	// Catch references that can never resolve, now rather than on the third
	// retry of a real order. Header templates are the one place where these
	// keys do resolve, because they render after the body.
	if key := referencesReserved(dry[0], reservedInFirstPass...); key != "" {
		return fmt.Errorf("%w: url: .%s is only available to header templates, not to the url", ErrInvalid, key)
	}
	if e.Body != "" {
		if key := referencesReserved(dry[len(dry)-1], reservedInFirstPass...); key != "" {
			return fmt.Errorf("%w: body: .%s covers the body itself, so it is only available to header templates", ErrInvalid, key)
		}
	}

	if err := e.dryRun(dry); err != nil {
		return err
	}

	e.templates = compiled
	return nil
}

// dryRun renders the endpoint against a fully populated sample context to catch
// errors that parsing alone would miss, such as indexing a slice as a map.
func (e *Endpoint) dryRun(templates []*template.Template) error {
	sample := ContextInput{
		OrderID:     "dry-run-order",
		ExternalRef: "dry-run-ref",
		Subject:     "dry-run-subject",
		Preset:      "dry-run-preset",
		Phase:       "prepare",
		CreatedAt:   time.Now(),
		StepName:    "dry-run-step",
		StepKind:    "notify",
		Topic:       e.Topic(),
		Service:     e.Service,
		Action:      e.Action,
		Attempt:     1,
		DeliveryID:  "dry-run-delivery",
		ResultURL:   "https://rc-vistart.invalid/api/v1/orders/dry-run-order/steps/dry-run-step/result",
		Secret:      "dry-run-secret",
		Timestamp:   time.Now(),
		Items:       []Item{{SKU: "SKU-1", Name: "Item", Quantity: 1, Price: 1}},
		Payload: map[string]any{
			"sample": "value",
			"nested": map[string]any{"key": "value"},
		},
	}

	data := sample.data()
	if _, err := execute(templates[0], data); err != nil { // url
		return fmt.Errorf("%w: url: %v", ErrInvalid, err)
	}

	// Headers render after the body, so they may read it and its signature.
	// The dry run must offer those keys too or it would reject a header
	// template that works perfectly well in production.
	headerData := sample.data()
	headerData["body"] = `{"sample":"value"}`
	headerData["signature"] = strings.Repeat("a", 64)
	for i, h := range e.Headers {
		if _, err := execute(templates[1+i], headerData); err != nil {
			return fmt.Errorf("%w: header[%d] %s: %v", ErrInvalid, i, h.Name, err)
		}
	}

	if e.Body != "" {
		if _, err := execute(templates[len(templates)-1], data); err != nil {
			return fmt.Errorf("%w: body: %v", ErrInvalid, err)
		}
	}
	return nil
}

// render expands the endpoint for one attempt. The URL and body are rendered
// first, then the headers, so a header template can reference .body and
// .signature — that ordering is what lets an endpoint reproduce a vendor's own
// signing scheme.
func (e *Endpoint) render(in ContextInput) (*Rendered, error) {
	if e.templates == nil {
		return nil, fmt.Errorf("%w: endpoint %s was not compiled", ErrInvalid, e.Topic())
	}

	rawURL, err := execute(e.templates[0], in.data())
	if err != nil {
		return nil, fmt.Errorf("render url: %w", err)
	}
	rawURL = strings.TrimSpace(rawURL)

	var body []byte
	if e.Body != "" {
		out, err := execute(e.templates[len(e.templates)-1], in.data())
		if err != nil {
			return nil, fmt.Errorf("render body: %w", err)
		}
		body = []byte(out)
	}

	signature := Sign(in.Secret, in.Timestamp, body)

	data := in.data()
	data["body"] = string(body)
	data["signature"] = signature

	headers := make([]Header, 0, len(e.Headers))
	for i, h := range e.Headers {
		value, err := execute(e.templates[1+i], data)
		if err != nil {
			return nil, fmt.Errorf("render header %s: %w", h.Name, err)
		}
		headers = append(headers, Header{Name: h.Name, Value: strings.TrimSpace(value)})
	}

	return &Rendered{
		Method:    e.Method,
		URL:       rawURL,
		Headers:   headers,
		Body:      body,
		Signature: signature,
	}, nil
}

// Sign returns the hex HMAC-SHA256 of "<unix timestamp>.<body>".
func Sign(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature reports whether signature matches Sign(secret, ts, body).
// Receivers that already depend on this module can reuse it.
func VerifySignature(secret string, ts time.Time, body []byte, signature string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	want := Sign(secret, ts, body)
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(signature, prefix)))
}
