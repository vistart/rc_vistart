package notify

import (
	"testing"
	"time"
)

func TestTaskKeyFormat(t *testing.T) {
	if got := TaskKey("ord_1", "verify.stock"); got != "ord_1/verify.stock" {
		t.Errorf("TaskKey() = %q, want ord_1/verify.stock", got)
	}
}

func TestQueuePopsDueInTimeOrder(t *testing.T) {
	q := NewQueue()
	now := time.Now()

	q.Push(Task{Key: "b", NotBefore: now.Add(2 * time.Second)})
	q.Push(Task{Key: "c", NotBefore: now.Add(3 * time.Second)})
	q.Push(Task{Key: "a", NotBefore: now})

	if q.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", q.Len())
	}

	for _, want := range []string{"a", "b", "c"} {
		got, ok := q.PopDue(now.Add(10 * time.Second))
		if !ok {
			t.Fatalf("PopDue() returned nothing, want %q", want)
		}
		if got.Key != want {
			t.Errorf("PopDue() = %q, want %q", got.Key, want)
		}
	}

	if _, ok := q.PopDue(now); ok {
		t.Error("PopDue() returned a task from an empty queue")
	}
}

func TestQueueHoldsTasksThatAreNotDue(t *testing.T) {
	q := NewQueue()
	now := time.Now()

	q.Push(Task{Key: "later", NotBefore: now.Add(time.Minute)})

	if _, ok := q.PopDue(now); ok {
		t.Error("PopDue() returned a task before it was due")
	}
	if q.Len() != 1 {
		t.Errorf("Len() = %d, want the task to stay queued", q.Len())
	}

	got, ok := q.PopDue(now.Add(time.Minute))
	if !ok || got.Key != "later" {
		t.Errorf("PopDue() = (%v, %v), want the task once it is due", got, ok)
	}
}

func TestQueueReplacesSameKey(t *testing.T) {
	q := NewQueue()
	now := time.Now()

	q.Push(Task{Key: "k", Attempt: 1, NotBefore: now})
	q.Push(Task{Key: "k", Attempt: 2, NotBefore: now.Add(time.Minute)})

	// A re-scheduled retry must not leave two entries for one task, or the
	// same notification would be delivered twice for one logical attempt.
	if q.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after re-scheduling the same key", q.Len())
	}
	got, _ := q.PopDue(now.Add(time.Hour))
	if got.Attempt != 2 {
		t.Errorf("Attempt = %d, want the newest scheduled attempt", got.Attempt)
	}
}

func TestQueueNextDueAtAndHas(t *testing.T) {
	q := NewQueue()
	now := time.Now()

	if _, ok := q.NextDueAt(); ok {
		t.Error("NextDueAt() reported a task in an empty queue")
	}
	if q.Has("k") {
		t.Error("Has() found a task in an empty queue")
	}

	want := now.Add(30 * time.Second)
	q.Push(Task{Key: "k", NotBefore: want})

	got, ok := q.NextDueAt()
	if !ok {
		t.Fatal("NextDueAt() reported no task")
	}
	if !got.Equal(want) {
		t.Errorf("NextDueAt() = %v, want %v", got, want)
	}
	if !q.Has("k") {
		t.Error("Has() = false for a queued task")
	}
}

func TestQueueClosedIgnoresPush(t *testing.T) {
	q := NewQueue()
	q.Close()

	if !q.Closed() {
		t.Error("Closed() = false after Close()")
	}
	q.Push(Task{Key: "k", NotBefore: time.Now().Add(-time.Second)})
	if q.Len() != 0 {
		t.Errorf("Len() = %d, want a closed queue to ignore pushes", q.Len())
	}
}

func TestQueueWakeSignal(t *testing.T) {
	q := NewQueue()

	// Nothing queued yet: the signal must still be safe to read.
	select {
	case <-q.WakeC():
		t.Fatal("WakeC() signalled before any push")
	default:
	}

	q.Push(Task{Key: "k", NotBefore: time.Now()})
	select {
	case <-q.WakeC():
	default:
		t.Error("Push did not signal WakeC()")
	}
}

func TestQueueConcurrentUse(t *testing.T) {
	q := NewQueue()
	now := time.Now()

	const workers = 16
	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			q.Push(Task{Key: "k", NotBefore: now})
			q.PopDue(now)
			q.Len()
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}
