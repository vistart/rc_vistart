package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FailureKind classifies why an attempt failed. The distinction drives whether
// a retry can possibly help.
type FailureKind string

const (
	// FailureRender means the templates could not be expanded. Retrying the
	// same input will fail again, so this escalates immediately.
	FailureRender FailureKind = "render"
	// FailureNetwork means the request never produced a response.
	FailureNetwork FailureKind = "network"
	// FailureTimeout means the receiver did not answer in time.
	FailureTimeout FailureKind = "timeout"
	// FailureStatus means the receiver answered with an unacceptable code.
	FailureStatus FailureKind = "status"
)

// Failure is the error returned by an Invoker.
type Failure struct {
	Kind       FailureKind
	StatusCode int
	Err        error
	// Snippet is a bounded excerpt of the response body, for diagnosis.
	Snippet string
}

func (f *Failure) Error() string {
	switch {
	case f.StatusCode != 0:
		return fmt.Sprintf("%s: unexpected status %d: %s", f.Kind, f.StatusCode, f.Snippet)
	case f.Err != nil:
		return fmt.Sprintf("%s: %v", f.Kind, f.Err)
	default:
		return string(f.Kind)
	}
}

func (f *Failure) Unwrap() error { return f.Err }

// Retryable reports whether another attempt could plausibly succeed. A render
// bug and a client-side rejection are permanent; transport problems, 5xx and
// 429 are worth another try.
func (f *Failure) Retryable() bool {
	switch f.Kind {
	case FailureRender:
		return false
	case FailureStatus:
		switch {
		case f.StatusCode == http.StatusTooManyRequests:
			return true
		case f.StatusCode >= 500:
			return true
		case f.StatusCode >= 300 && f.StatusCode < 400:
			// A redirect we were not asked to follow is a configuration
			// problem, not a transient one.
			return false
		default:
			return false
		}
	default:
		return true
	}
}

// Result describes a completed attempt.
type Result struct {
	StatusCode int
	Latency    time.Duration
	Body       []byte
}

// Invoker performs one delivery attempt.
type Invoker interface {
	// Invoke renders the endpoint and performs a single attempt. It returns a
	// Result together with a *Failure when the attempt did not succeed.
	Invoke(ctx context.Context, ep Endpoint, in ContextInput) (Result, error)
}

// HTTPInvoker delivers over HTTP.
type HTTPInvoker struct {
	Client           *http.Client
	MaxResponseBytes int64
	SnippetBytes     int
	// UserAgent is sent unless a header template overrides it.
	UserAgent string
}

// NewHTTPInvoker returns an invoker with the given client. A nil client gets a
// default one; per-attempt timeouts are applied through the context, not here.
func NewHTTPInvoker(client *http.Client, maxResponseBytes int64) *HTTPInvoker {
	if client == nil {
		client = &http.Client{}
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = 64 << 10
	}
	return &HTTPInvoker{
		Client:           client,
		MaxResponseBytes: maxResponseBytes,
		SnippetBytes:     256,
		UserAgent:        "rc_vistart",
	}
}

// Invoke implements Invoker.
func (v *HTTPInvoker) Invoke(ctx context.Context, ep Endpoint, in ContextInput) (Result, error) {
	rendered, err := ep.render(in)
	if err != nil {
		return Result{}, &Failure{Kind: FailureRender, Err: err}
	}

	attemptCtx, cancel := context.WithTimeout(ctx, ep.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, rendered.Method, rendered.URL, bytes.NewReader(rendered.Body))
	if err != nil {
		return Result{}, &Failure{Kind: FailureRender, Err: fmt.Errorf("build request: %w", err)}
	}

	// Identity headers always describe the task, and are set before the
	// templates so a registration cannot accidentally lie about which order
	// it is acting on.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", v.UserAgent)
	req.Header.Set(HeaderOrder, in.OrderID)
	req.Header.Set(HeaderStep, in.StepName)
	req.Header.Set(HeaderIdempotencyKey, in.IdempotencyKey)
	req.Header.Set(HeaderDelivery, in.DeliveryID)
	req.Header.Set(HeaderAttempt, fmt.Sprintf("%d", in.Attempt))
	for _, h := range rendered.Headers {
		if h.Value == "" {
			// A template that resolves to empty means "omit this header",
			// which is how an optional credential is expressed.
			req.Header.Del(h.Name)
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}

	started := time.Now()
	resp, err := v.Client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		if isTimeout(ctx, attemptCtx, err) {
			return Result{Latency: elapsed}, &Failure{
				Kind: FailureTimeout,
				Err:  fmt.Errorf("after %s: %w", ep.Timeout, err),
			}
		}
		return Result{Latency: elapsed}, &Failure{Kind: FailureNetwork, Err: err}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, v.MaxResponseBytes))
	res := Result{StatusCode: resp.StatusCode, Latency: elapsed, Body: body}

	if accepts(resp.StatusCode, ep.ExpectStatus) {
		return res, nil
	}
	return res, &Failure{
		Kind:       FailureStatus,
		StatusCode: resp.StatusCode,
		Snippet:    snippet(body, v.SnippetBytes),
	}
}

func accepts(status int, expect []int) bool {
	for _, code := range expect {
		if code == status {
			return true
		}
	}
	return false
}

func isTimeout(parent, attempt context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// A parent cancellation is not a receiver timeout: the request was
	// abandoned by us, which the engine reports separately.
	if parent.Err() != nil {
		return false
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return attempt.Err() != nil
}

func snippet(body []byte, limit int) string {
	if limit <= 0 {
		limit = 256
	}
	s := strings.TrimSpace(string(body))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
