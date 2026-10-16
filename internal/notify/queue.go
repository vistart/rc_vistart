package notify

import (
	"sync"
	"time"
)

// Headers describing the task and the attempt. The task identity
// (HeaderIdempotencyKey, HeaderOrder, HeaderStep) is stable across retries so
// a receiver can deduplicate; HeaderDelivery and HeaderAttempt change on every
// try and exist only for tracing.
const (
	HeaderOrder          = "X-Rc-Vistart-Order"
	HeaderStep           = "X-Rc-Vistart-Step"
	HeaderIdempotencyKey = "X-Rc-Vistart-Idempotency-Key"
	HeaderDelivery       = "X-Rc-Vistart-Delivery"
	HeaderAttempt        = "X-Rc-Vistart-Attempt"
)

// Task is one scheduled delivery of a step.
//
// TaskKey is the stable identity of the work: it stays the same across every
// retry, which is what lets both sides prevent duplicates. DeliveryID is
// unique per attempt so a receiver can tell a retry from a new task, and so we
// can tell which attempt produced a late result.
type Task struct {
	// Key is "<orderID>/<stepName>".
	Key        string
	OrderID    string
	StepName   string
	Topic      string
	Attempt    int
	DeliveryID string
	NotBefore  time.Time

	// AwaitResult marks a scheduled check for a late result rather than a
	// delivery attempt. Popping such a task does not contact the receiver; it
	// only asks whether the awaited result arrived in time.
	AwaitResult bool
}

// Queue is a small time-ordered scheduler for pending deliveries.
//
// The slice stays sorted by NotBefore. That is fine at prototype scale; a
// heap would only matter with thousands of in-flight tasks.
type Queue struct {
	mu     sync.Mutex
	tasks  []*Task
	wake   chan struct{}
	closed bool
}

// NewQueue returns an empty queue.
func NewQueue() *Queue {
	return &Queue{wake: make(chan struct{}, 1)}
}

// Push schedules a task and wakes a waiting worker. A task already queued for
// the same key is replaced, so a re-scheduled retry never doubles up.
func (q *Queue) Push(t Task) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}

	stored := &t
	at := len(q.tasks)
	for i, existing := range q.tasks {
		if existing.Key == t.Key {
			// Re-scheduling one task replaces it rather than adding a second
			// copy, which would deliver one logical attempt twice.
			q.tasks[i] = stored
			at = -1
			break
		}
		if existing.NotBefore.After(t.NotBefore) {
			at = i
			break
		}
	}
	if at >= 0 {
		q.tasks = append(q.tasks, nil)
		copy(q.tasks[at+1:], q.tasks[at:])
		q.tasks[at] = stored
	}
	q.mu.Unlock()

	q.signal()
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Wake asks an idle worker to re-check the queue.
func (q *Queue) Wake() { q.signal() }

// PopDue removes and returns the earliest task that is due at or before now.
// It returns false when nothing is due yet.
func (q *Queue) PopDue(now time.Time) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) == 0 || q.tasks[0].NotBefore.After(now) {
		return Task{}, false
	}
	t := *q.tasks[0]
	q.tasks = q.tasks[1:]
	return t, true
}

// WakeC is signalled when work is pushed or the queue closes, so an idle
// worker can block instead of polling.
func (q *Queue) WakeC() <-chan struct{} { return q.wake }

// NextDueAt returns when the earliest queued task becomes runnable, and false
// when the queue is empty.
func (q *Queue) NextDueAt() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) == 0 {
		return time.Time{}, false
	}
	return q.tasks[0].NotBefore, true
}

// Len returns the number of queued tasks.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

// Has reports whether a task for the key is queued.
func (q *Queue) Has(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.tasks {
		if t.Key == key {
			return true
		}
	}
	return false
}

// Close stops accepting work and wakes every worker.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()

	q.signal()
}

// Closed reports whether Close was called.
func (q *Queue) Closed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// TaskKey is the stable identity of a task.
func TaskKey(orderID, stepName string) string {
	return orderID + "/" + stepName
}
