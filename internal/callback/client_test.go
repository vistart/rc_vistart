package callback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testCallback(url string) Callback {
	return Callback{ID: "cb-1", Topic: "order.created", URL: url}
}

func TestHTTPInvokerSendsPayloadAndHeaders(t *testing.T) {
	var (
		gotBody        string
		gotEvent       string
		gotDelivery    string
		gotContentType string
		gotSignature   string
		gotTimestamp   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotEvent = r.Header.Get(HeaderEvent)
		gotDelivery = r.Header.Get(HeaderDelivery)
		gotContentType = r.Header.Get("Content-Type")
		gotSignature = r.Header.Get(HeaderSignature)
		gotTimestamp = r.Header.Get(HeaderTimestamp)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	payload := []byte(`{"id":"order-1"}`)
	cb := testCallback(srv.URL)
	cb.Secret = "s3cr3t"

	inv := NewHTTPInvoker(srv.Client(), 1024)
	if err := inv.Invoke(context.Background(), cb, "delivery-1", payload); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	if gotBody != string(payload) {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotEvent != cb.Topic {
		t.Errorf("%s = %q, want %q", HeaderEvent, gotEvent, cb.Topic)
	}
	if gotDelivery != "delivery-1" {
		t.Errorf("%s = %q, want delivery-1", HeaderDelivery, gotDelivery)
	}
	if gotSignature == "" {
		t.Fatalf("%s is empty, want a signature for a callback with a secret", HeaderSignature)
	}

	ts, err := time.Parse(time.RFC3339, gotTimestamp)
	if err != nil {
		t.Fatalf("time.Parse(%s=%q) error = %v", HeaderTimestamp, gotTimestamp, err)
	}
	if !VerifySignature(cb.Secret, ts, payload, gotSignature) {
		t.Errorf("signature %q does not verify against secret %q", gotSignature, cb.Secret)
	}
}

func TestHTTPInvokerOmitsSignatureWithoutSecret(t *testing.T) {
	var gotSignature string
	var gotSignaturePresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotSignaturePresent = r.Header[HeaderSignature]
		gotSignature = r.Header.Get(HeaderSignature)
	}))
	defer srv.Close()

	inv := NewHTTPInvoker(srv.Client(), 1024)
	if err := inv.Invoke(context.Background(), testCallback(srv.URL), "d-1", []byte(`{}`)); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if gotSignaturePresent || gotSignature != "" {
		t.Errorf("%s = %q, want it to be absent", HeaderSignature, gotSignature)
	}
}

func TestHTTPInvokerReportsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	inv := NewHTTPInvoker(srv.Client(), 1024)
	err := inv.Invoke(context.Background(), testCallback(srv.URL), "d-1", []byte(`{}`))
	if err == nil {
		t.Fatal("Invoke() error = nil, want an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("Invoke() error = %q, want it to mention the status", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Invoke() error = %q, want it to include the response snippet", err)
	}
}

func TestHTTPInvokerTruncatesLongResponseSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	inv := NewHTTPInvoker(srv.Client(), 1024)
	err := inv.Invoke(context.Background(), testCallback(srv.URL), "d-1", []byte(`{}`))
	if err == nil {
		t.Fatal("Invoke() error = nil, want an error for a 502 response")
	}
	if len(err.Error()) > 512 {
		t.Errorf("Invoke() error is %d bytes, want the snippet to be truncated", len(err.Error()))
	}
}

func TestHTTPInvokerAppliesCallbackTimeout(t *testing.T) {
	// released lets the handler finish before httptest.Server.Close waits for
	// it, so the defers must be ordered: release first, close second.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	defer srv.Close()
	defer close(released)

	cb := testCallback(srv.URL)
	cb.Timeout = 50 * time.Millisecond

	inv := NewHTTPInvoker(srv.Client(), 1024)
	start := time.Now()
	err := inv.Invoke(context.Background(), cb, "d-1", []byte(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Invoke() error = nil, want a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Invoke() took %v, want it to honour the 50ms callback timeout", elapsed)
	}
}

func TestHTTPInvokerHonoursContextCancellation(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	defer srv.Close()
	defer close(released)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	inv := NewHTTPInvoker(srv.Client(), 1024)
	if err := inv.Invoke(ctx, testCallback(srv.URL), "d-1", []byte(`{}`)); err == nil {
		t.Fatal("Invoke() error = nil, want a cancellation error")
	}
}

func TestVerifySignature(t *testing.T) {
	ts := time.Now().UTC()
	payload := []byte(`{"event":"order.created"}`)

	valid := "sha256=" + Sign("secret", ts, payload)
	if !VerifySignature("secret", ts, payload, valid) {
		t.Error("VerifySignature() = false for a valid signature")
	}

	tests := []struct {
		name      string
		secret    string
		payload   []byte
		signature string
	}{
		{"wrong secret", "other", payload, valid},
		{"tampered payload", "secret", []byte(`{"event":"order.paid"}`), valid},
		{"missing prefix", "secret", payload, Sign("secret", ts, payload)},
		{"not hex", "secret", payload, "sha256=zzz"},
		{"empty signature", "secret", payload, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if VerifySignature(tc.secret, ts, tc.payload, tc.signature) {
				t.Errorf("VerifySignature() = true, want false")
			}
		})
	}
}

func TestSignIsStableForTheSameTimestamp(t *testing.T) {
	ts := time.Now().UTC()
	payload := []byte(`{"a":1}`)

	first := Sign("s", ts, payload)
	second := Sign("s", ts, payload)
	if first != second {
		t.Errorf("Sign() = %q then %q, want a stable value", first, second)
	}
	if len(first) != 64 {
		t.Errorf("len(Sign()) = %d, want a 64-character hex digest", len(first))
	}
}

func TestNewHTTPInvokerDefaults(t *testing.T) {
	inv := NewHTTPInvoker(nil, 0)
	if inv.Client == nil {
		t.Error("Client = nil, want a default client")
	}
	if inv.MaxResponseBytes != 64<<10 {
		t.Errorf("MaxResponseBytes = %d, want %d", inv.MaxResponseBytes, 64<<10)
	}
}

func TestSnippetTruncates(t *testing.T) {
	if got := snippet([]byte("  ok  ")); got != "ok" {
		t.Errorf("snippet() = %q, want %q", got, "ok")
	}
	if got := snippet([]byte(strings.Repeat("y", 1000))); len(got) != 256+3 {
		t.Errorf("len(snippet()) = %d, want %d", len(got), 256+3)
	}
}
