package callback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// HTTP header names used on an outgoing callback invocation.
const (
	HeaderEvent     = "X-Rc-Vistart-Event"
	HeaderDelivery  = "X-Rc-Vistart-Delivery"
	HeaderTimestamp = "X-Rc-Vistart-Timestamp"
	HeaderSignature = "X-Rc-Vistart-Signature"
)

// Invoker performs a single callback invocation.
type Invoker interface {
	// Invoke posts payload to cb. deliveryID identifies this attempt and is
	// echoed in a header so the receiver can deduplicate.
	Invoke(ctx context.Context, cb Callback, deliveryID string, payload []byte) error
}

// HTTPInvoker invokes callbacks over HTTP.
type HTTPInvoker struct {
	// Client is used for every invocation. Its own Timeout is only a
	// fallback: per-callback timeouts are applied via the context.
	Client *http.Client
	// MaxResponseBytes bounds how much of the response body is read.
	MaxResponseBytes int64
}

// NewHTTPInvoker returns an invoker with sensible defaults.
func NewHTTPInvoker(client *http.Client, maxResponseBytes int64) *HTTPInvoker {
	if client == nil {
		client = &http.Client{}
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = 64 << 10
	}
	return &HTTPInvoker{Client: client, MaxResponseBytes: maxResponseBytes}
}

// Invoke implements Invoker.
func (i *HTTPInvoker) Invoke(ctx context.Context, cb Callback, deliveryID string, payload []byte) error {
	if cb.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cb.Timeout)
		defer cancel()
	}

	timestamp := time.Now().UTC()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cb.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("callback %s: build request: %w", cb.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rc_vistart")
	req.Header.Set(HeaderEvent, cb.Topic)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderTimestamp, timestamp.Format(time.RFC3339))
	if cb.Secret != "" {
		req.Header.Set(HeaderSignature, "sha256="+Sign(cb.Secret, timestamp, payload))
	}

	resp, err := i.Client.Do(req)
	if err != nil {
		return fmt.Errorf("callback %s: %w", cb.ID, err)
	}
	defer resp.Body.Close()

	// Drain at most MaxResponseBytes so that the connection can be reused.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, i.MaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if snippet := snippet(body); snippet != "" {
			return fmt.Errorf("callback %s: unexpected status %s: %s", cb.ID, resp.Status, snippet)
		}
		return fmt.Errorf("callback %s: unexpected status %s", cb.ID, resp.Status)
	}
	return nil
}

// Sign returns the hex-encoded HMAC-SHA256 of "timestamp.payload", which is
// the value receivers must recompute to authenticate an invocation.
func Sign(secret string, timestamp time.Time, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature reports whether signature matches the expected HMAC of
// secret, timestamp and payload. It is exported so that both rc_vistart and
// its callback consumers can share one implementation.
func VerifySignature(secret string, timestamp time.Time, payload []byte, signature string) bool {
	const prefix = "sha256="
	if len(signature) <= len(prefix) || signature[:len(prefix)] != prefix {
		return false
	}
	got, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(Sign(secret, timestamp, payload))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func snippet(body []byte) string {
	const limit = 256
	s := string(bytes.TrimSpace(body))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
