package notify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func registerForTest(t *testing.T, ep Endpoint) Endpoint {
	t.Helper()
	r := NewMemoryRegistry()
	stored, err := r.Register(ep)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return stored
}

func sampleInput() ContextInput {
	return ContextInput{
		OrderID:        "ord_123",
		ExternalRef:    "ref 42",
		Subject:        "user-9",
		Preset:         "order.standard",
		Phase:          "prepare",
		CreatedAt:      time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		StepName:       "verify.stock",
		StepKind:       string(KindNotifyKind()),
		Topic:          "inventory.stock.prehold",
		Service:        "inventory",
		Action:         "stock.prehold",
		Attempt:        2,
		DeliveryID:     "dlv-1",
		IdempotencyKey: "ord_123/verify.stock",
		ResultURL:      "https://rc.invalid/api/v1/orders/ord_123/steps/verify.stock/result",
		Secret:         "s3cr3t",
		Timestamp:      time.Date(2026, 10, 16, 12, 30, 0, 0, time.UTC),
		Items:          []Item{{SKU: "SKU-1", Name: "Widget", Quantity: 2, Price: 9.5}},
		Payload:        map[string]any{"channel": "app"},
	}
}

// KindNotifyKind exists only to keep the sample input readable; the value is
// what the engine passes for a notify step.
func KindNotifyKind() string { return "notify" }

func TestRenderExpandsURLHeadersAndBody(t *testing.T) {
	ep := registerForTest(t, Endpoint{
		Service: "inventory",
		Action:  "stock.prehold",
		Method:  "POST",
		URL:     "https://inv.example.com/prehold?order={{ .order.id }}&ref={{ urlquery .order.external_ref }}",
		Headers: []Header{
			{Name: "X-Order", Value: "{{ .order.id }}"},
			{Name: "X-Attempt", Value: "{{ .step.attempt }}"},
		},
		Body: `{"order":"{{ .order.id }}","sku":"{{ (index .items 0).sku }}","quantity":{{ (index .items 0).quantity }}}`,
	})

	rendered, err := ep.render(sampleInput())
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}

	wantURL := "https://inv.example.com/prehold?order=ord_123&ref=ref+42"
	if rendered.URL != wantURL {
		t.Errorf("URL = %q, want %q", rendered.URL, wantURL)
	}
	if rendered.Method != "POST" {
		t.Errorf("Method = %q, want POST", rendered.Method)
	}

	headers := make(map[string]string, len(rendered.Headers))
	for _, h := range rendered.Headers {
		headers[h.Name] = h.Value
	}
	if headers["X-Order"] != "ord_123" {
		t.Errorf("X-Order = %q, want ord_123", headers["X-Order"])
	}
	if headers["X-Attempt"] != "2" {
		t.Errorf("X-Attempt = %q, want 2", headers["X-Attempt"])
	}

	var body map[string]any
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("rendered body is not valid JSON: %v (%s)", err, rendered.Body)
	}
	if body["order"] != "ord_123" {
		t.Errorf("body order = %v, want ord_123", body["order"])
	}
	if body["sku"] != "SKU-1" {
		t.Errorf("body sku = %v, want SKU-1", body["sku"])
	}
	if body["quantity"] != float64(2) {
		t.Errorf("body quantity = %v, want 2", body["quantity"])
	}
}

func TestRenderExposesIdempotencyKeyAndResultURL(t *testing.T) {
	ep := registerForTest(t, Endpoint{
		Service: "inventory",
		Action:  "stock.prehold",
		URL:     "https://inv.example.com/prehold",
		Body:    `{"key":"{{ .idempotency_key }}","reply":"{{ .result_url }}"}`,
	})

	rendered, err := ep.render(sampleInput())
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}

	body := string(rendered.Body)
	if !strings.Contains(body, "ord_123/verify.stock") {
		t.Errorf("body = %s, want it to carry the stable task key", body)
	}
	if !strings.Contains(body, "/steps/verify.stock/result") {
		t.Errorf("body = %s, want it to carry the result URL", body)
	}
}

func TestRenderSignatureIsUsableFromHeaderTemplates(t *testing.T) {
	ep := registerForTest(t, Endpoint{
		Service: "inventory",
		Action:  "stock.prehold",
		URL:     "https://inv.example.com/prehold",
		Headers: []Header{{Name: "X-Signature", Value: "sha256={{ .signature }}"}},
		Body:    `{"order":"{{ .order.id }}"}`,
	})

	in := sampleInput()
	rendered, err := ep.render(in)
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}

	// The header must sign the body that was actually sent, which is only
	// possible because headers render after the body.
	if rendered.Headers[0].Value != "sha256="+Sign(in.Secret, in.Timestamp, rendered.Body) {
		t.Errorf("X-Signature = %q, want it to sign the rendered body", rendered.Headers[0].Value)
	}
}

func TestRenderExposesTemplateHelpers(t *testing.T) {
	ep := registerForTest(t, Endpoint{
		Service: "svc",
		Action:  "act",
		URL:     "https://x.example.com/h",
		Body: `{"json":{{ json .items }},` +
			`"upper":"{{ upper .step.name }}",` +
			`"lower":"{{ lower .order.subject }}",` +
			`"trim":"{{ trim .order.external_ref }}",` +
			`"default":"{{ default "none" (index .payload "missing") }}",` +
			`"mac":"{{ hmacSHA256 .secret .order.id }}"}`,
	})

	rendered, err := ep.render(sampleInput())
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("rendered body is not valid JSON: %v (%s)", err, rendered.Body)
	}

	if body["upper"] != "VERIFY.STOCK" {
		t.Errorf("upper = %v, want VERIFY.STOCK", body["upper"])
	}
	if body["lower"] != "user-9" {
		t.Errorf("lower = %v, want user-9", body["lower"])
	}
	if body["trim"] != "ref 42" {
		t.Errorf("trim = %v, want 'ref 42'", body["trim"])
	}
	if body["default"] != "none" {
		t.Errorf("default = %v, want none for a missing key", body["default"])
	}
	if body["mac"] == "" || body["mac"] == nil {
		t.Errorf("hmacSHA256 = %v, want a digest", body["mac"])
	}
}

func TestRenderFailsOnMissingPayloadKey(t *testing.T) {
	ep := registerForTest(t, Endpoint{
		Service: "svc",
		Action:  "act",
		URL:     "https://x.example.com/h",
		Body:    `{"channel":"{{ .payload.channel_that_does_not_exist }}"}`,
	})

	// Registration succeeds: the dry run cannot know which payload keys real
	// orders carry. The failure appears on delivery, where a typo must not
	// silently send a blank field to a vendor.
	if _, err := ep.render(sampleInput()); err == nil {
		t.Fatal("render() error = nil, want a missingkey error")
	}
}

func TestRegisterRejectsTemplateThatCannotWork(t *testing.T) {
	r := NewMemoryRegistry()

	// items is a slice, so treating it as a map is unsatisfiable for any
	// payload and must be caught at registration.
	_, err := r.Register(Endpoint{
		Service: "svc",
		Action:  "act",
		URL:     "https://x.example.com/h",
		Body:    `{{ .items.sku }}`,
	})
	if err == nil {
		t.Fatal("Register() error = nil, want a dry-run failure")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("Register() error = %q, want it to name the offending field", err)
	}
}

func TestRenderUncompiledEndpointFails(t *testing.T) {
	ep := Endpoint{Service: "svc", Action: "act", URL: "https://x.example.com/h"}

	if _, err := ep.render(sampleInput()); err == nil {
		t.Fatal("render() error = nil, want an error for an endpoint that was never compiled")
	}
}

func TestRegisterRejectsReservedKeysOutsideHeaders(t *testing.T) {
	// .signature covers the body, so it cannot appear inside the body, and
	// .body only exists once the body is rendered. Catching this at
	// registration turns a guaranteed delivery-time failure into an immediate,
	// actionable error.
	tests := []struct {
		name string
		ep   Endpoint
		want string
	}{
		{
			name: "signature in body",
			ep: Endpoint{
				Service: "s", Action: "a",
				URL: "https://x.example.com/h", Body: `{"sig":"{{ .signature }}"}`,
			},
			want: "body",
		},
		{
			name: "body in body",
			ep: Endpoint{
				Service: "s", Action: "a",
				URL: "https://x.example.com/h", Body: `{{ .body }}`,
			},
			want: "body",
		},
		{
			name: "signature in url",
			ep: Endpoint{
				Service: "s", Action: "a",
				URL: "https://x.example.com/h?sig={{ .signature }}",
			},
			want: "url",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMemoryRegistry()
			_, err := r.Register(tc.ep)
			if err == nil {
				t.Fatal("Register() error = nil, want a reserved-key error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Register() error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Register() error = %q, want it to name the %q template", err, tc.want)
			}
		})
	}
}

func TestHeadersMayReferenceBodyAndSignature(t *testing.T) {
	r := NewMemoryRegistry()
	if _, err := r.Register(Endpoint{
		Service: "s", Action: "a",
		URL: "https://x.example.com/h",
		Headers: []Header{
			{Name: "X-Sig", Value: "{{ .signature }}"},
			{Name: "X-Len", Value: "{{ len .body }}"},
		},
		Body: `{"a":1}`,
	}); err != nil {
		t.Fatalf("Register() error = %v, want header references to be allowed", err)
	}
}

func TestReservedKeyDetectionInsideControlFlow(t *testing.T) {
	// The check must also catch a reference hidden inside an if or range, not
	// only a bare one on the first line.
	r := NewMemoryRegistry()
	_, err := r.Register(Endpoint{
		Service: "s", Action: "a",
		URL:  "https://x.example.com/h",
		Body: `{{ if .order.id }}{{ .signature }}{{ end }}`,
	})
	if err == nil {
		t.Fatal("Register() error = nil, want the nested .signature reference to be caught")
	}
}

func TestSignAndVerify(t *testing.T) {
	ts := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"order":"ord_1"}`)

	sig := Sign("secret", ts, body)
	if len(sig) != 64 {
		t.Errorf("len(Sign()) = %d, want a 64-character hex digest", len(sig))
	}
	if !VerifySignature("secret", ts, body, "sha256="+sig) {
		t.Error("VerifySignature() = false for a valid signature")
	}

	tests := []struct {
		name      string
		secret    string
		ts        time.Time
		body      []byte
		signature string
	}{
		{"wrong secret", "other", ts, body, "sha256=" + sig},
		{"tampered body", "secret", ts, []byte(`{"order":"ord_2"}`), "sha256=" + sig},
		{"different second", "secret", ts.Add(time.Second), body, "sha256=" + sig},
		{"missing prefix", "secret", ts, body, sig},
		{"not hex", "secret", ts, body, "sha256=zzz"},
		{"empty", "secret", ts, body, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if VerifySignature(tc.secret, tc.ts, tc.body, tc.signature) {
				t.Error("VerifySignature() = true, want false")
			}
		})
	}
}

func TestSignIsStableForTheSameInput(t *testing.T) {
	ts := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"a":1}`)

	if Sign("s", ts, body) != Sign("s", ts, body) {
		t.Error("Sign() is not stable for the same timestamp and body")
	}
}
