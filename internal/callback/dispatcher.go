package callback

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Delivery is the outcome of one callback invocation.
type Delivery struct {
	CallbackID string `json:"callback_id"`
	URL        string `json:"url"`
	// Status is one of "delivered", "failed" or "skipped".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Duration is how long the invocation took.
	Duration time.Duration `json:"duration"`
}

// Delivery status values.
const (
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// Result summarises one dispatch round.
type Result struct {
	Topic      string     `json:"topic"`
	DeliveryID string     `json:"delivery_id"`
	Delivered  int        `json:"delivered"`
	Failed     int        `json:"failed"`
	Skipped    int        `json:"skipped"`
	Deliveries []Delivery `json:"deliveries"`
}

// Dispatcher fans an event out to every callback registered for a topic.
type Dispatcher struct {
	registry Registry
	invoker  Invoker
	logger   *slog.Logger
	// defaultTimeout is applied to callbacks without their own timeout.
	defaultTimeout time.Duration
	// maxParallel bounds concurrent invocations within one dispatch.
	maxParallel int
}

// NewDispatcher returns a dispatcher. maxParallel is clamped to at least one.
func NewDispatcher(registry Registry, invoker Invoker, defaultTimeout time.Duration, maxParallel int, logger *slog.Logger) *Dispatcher {
	if maxParallel < 1 {
		maxParallel = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		registry:       registry,
		invoker:        invoker,
		logger:         logger,
		defaultTimeout: defaultTimeout,
		maxParallel:    maxParallel,
	}
}

// Dispatch invokes every callback registered for topic with payload. A
// callback failure is reported in Result rather than returned as an error; an
// error is only returned when the registry itself cannot be read.
func (d *Dispatcher) Dispatch(ctx context.Context, topic string, payload []byte) (Result, error) {
	cbs, err := d.registry.List(topic)
	if err != nil {
		return Result{}, fmt.Errorf("dispatch %s: %w", topic, err)
	}

	res := Result{Topic: topic, DeliveryID: NewID(), Deliveries: make([]Delivery, 0, len(cbs))}
	if len(cbs) == 0 {
		return res, nil
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, d.maxParallel)
		out = make([]Delivery, len(cbs))
	)

	for i, cb := range cbs {
		wg.Add(1)
		go func(idx int, cb Callback) {
			defer wg.Done()

			// A context that is already done must never start a new
			// invocation, so check it before offering the semaphore.
			if err := ctx.Err(); err != nil {
				out[idx] = Delivery{
					CallbackID: cb.ID,
					URL:        cb.URL,
					Status:     StatusSkipped,
					Error:      err.Error(),
				}
				return
			}

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[idx] = Delivery{
					CallbackID: cb.ID,
					URL:        cb.URL,
					Status:     StatusSkipped,
					Error:      ctx.Err().Error(),
				}
				return
			}

			// Deliveries inherit the server deadline; a per-callback timeout
			// is layered on top by the invoker.
			if cb.Timeout <= 0 {
				cb.Timeout = d.defaultTimeout
			}

			started := time.Now()
			err := d.invoker.Invoke(ctx, cb, res.DeliveryID, payload)
			delivery := Delivery{
				CallbackID: cb.ID,
				URL:        cb.URL,
				Status:     StatusDelivered,
				Duration:   time.Since(started),
			}
			if err != nil {
				delivery.Status = StatusFailed
				delivery.Error = err.Error()
				d.logger.Warn("callback delivery failed",
					slog.String("callback_id", cb.ID),
					slog.String("url", cb.URL),
					slog.String("error", err.Error()),
				)
			}
			out[idx] = delivery
		}(i, cb)
	}
	wg.Wait()

	for _, delivery := range out {
		switch delivery.Status {
		case StatusDelivered:
			res.Delivered++
		case StatusFailed:
			res.Failed++
		default:
			res.Skipped++
		}
	}
	res.Deliveries = out

	d.logger.Info("event dispatched",
		slog.String("topic", topic),
		slog.String("delivery_id", res.DeliveryID),
		slog.Int("delivered", res.Delivered),
		slog.Int("failed", res.Failed),
		slog.Int("skipped", res.Skipped),
	)
	return res, nil
}
