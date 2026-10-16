package callback

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCallbackMarshalJSON(t *testing.T) {
	cb := Callback{
		ID:        "cb-1",
		Topic:     "order.created",
		URL:       "https://billing.example.com/hooks",
		Secret:    "s3cr3t",
		Timeout:   3 * time.Second,
		CreatedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	got := string(data)

	want := `{"id":"cb-1","topic":"order.created","url":"https://billing.example.com/hooks","timeout":"3s","created_at":"2026-10-16T12:00:00Z"}`
	if got != want {
		t.Errorf("json.Marshal() =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("json.Marshal() leaked the secret: %s", got)
	}
}

func TestCallbackMarshalJSONOmitsZeroTimeout(t *testing.T) {
	data, err := json.Marshal(Callback{ID: "cb-1", Topic: "t", URL: "https://a.example.com/h"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "timeout") {
		t.Errorf("json.Marshal() = %s, want no timeout field when it is zero", data)
	}
}

func TestCallbackUnmarshalJSON(t *testing.T) {
	var cb Callback
	err := json.Unmarshal([]byte(`{"id":"cb-1","topic":"order.created","url":"https://a.example.com/h","timeout":"1m30s","created_at":"2026-10-16T12:00:00Z"}`), &cb)
	if err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if cb.ID != "cb-1" || cb.Topic != "order.created" || cb.URL != "https://a.example.com/h" {
		t.Errorf("decoded = %+v, want the registered fields", cb)
	}
	if cb.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v, want 1m30s", cb.Timeout)
	}
	if !cb.CreatedAt.Equal(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want the encoded timestamp", cb.CreatedAt)
	}
}

func TestCallbackUnmarshalJSONEmptyTimeout(t *testing.T) {
	var cb Callback
	if err := json.Unmarshal([]byte(`{"topic":"t","url":"https://a.example.com/h"}`), &cb); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if cb.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 for an absent timeout", cb.Timeout)
	}
}

func TestCallbackUnmarshalJSONRejectsBadTimeout(t *testing.T) {
	tests := []struct{ name, payload string }{
		{"not a duration", `{"timeout":"soon"}`},
		{"nanoseconds", `{"timeout":3000000000}`},
		{"zero", `{"timeout":"0s"}`},
		{"negative", `{"timeout":"-5s"}`},
		{"malformed json", `{"timeout":`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cb Callback
			if err := json.Unmarshal([]byte(tc.payload), &cb); err == nil {
				t.Fatalf("json.Unmarshal(%s) error = nil, want an error", tc.payload)
			}
		})
	}
}

func TestCallbackJSONRoundTrip(t *testing.T) {
	want := Callback{
		ID:        "cb-1",
		Topic:     "order.created",
		URL:       "https://billing.example.com/hooks",
		Secret:    "s3cr3t",
		Timeout:   2500 * time.Millisecond,
		CreatedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var got Callback
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	// Secret is write-only, so it is the one field a round trip must not keep.
	want.Secret = ""
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
