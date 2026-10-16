package notify

import (
	"errors"
	"testing"
	"time"
)

func meta() Meta {
	return Meta{Topic: "inventory.stock.prehold", Service: "inventory", Action: "stock.prehold", URL: "https://inv.example.com/prehold"}
}

func TestStatsRecordDeliveredAndFailed(t *testing.T) {
	s := NewStats()
	m := meta()

	s.RecordAttempt(m)
	s.RecordDelivered(m, 100*time.Millisecond)
	s.RecordAttempt(m)
	s.RecordAttempt(m)
	s.RecordFailed(m, errors.New("connection refused"))

	snapshot := s.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("len(Snapshot()) = %d, want 1", len(snapshot))
	}
	ts := snapshot[0]

	if ts.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", ts.Attempts)
	}
	if ts.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", ts.Delivered)
	}
	if ts.Failed != 1 {
		t.Errorf("Failed = %d, want 1", ts.Failed)
	}
	if ts.LastError != "connection refused" {
		t.Errorf("LastError = %q, want the latest failure", ts.LastError)
	}
	if ts.LastErrorAt == nil {
		t.Error("LastErrorAt is nil")
	}
	if ts.LastSuccessAt == nil {
		t.Error("LastSuccessAt is nil")
	}
	if ts.Service != "inventory" || ts.Action != "stock.prehold" {
		t.Errorf("service/action = %q/%q, want inventory/stock.prehold", ts.Service, ts.Action)
	}
}

func TestAverageLatencyMs(t *testing.T) {
	s := NewStats()
	m := meta()

	if got := (TopicStats{}).AverageLatencyMs(); got != 0 {
		t.Errorf("AverageLatencyMs() = %v, want 0 with no deliveries", got)
	}

	s.RecordAttempt(m)
	s.RecordDelivered(m, 100*time.Millisecond)
	s.RecordAttempt(m)
	s.RecordDelivered(m, 300*time.Millisecond)

	snapshot := s.Snapshot()
	if got := snapshot[0].AverageLatencyMs(); got != 200 {
		t.Errorf("AverageLatencyMs() = %v, want 200", got)
	}
}

func TestStatsEscalated(t *testing.T) {
	s := NewStats()
	m := meta()

	s.RecordAttempt(m)
	s.RecordFailed(m, errors.New("timeout"))
	s.RecordEscalated(m)

	snapshot := s.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("len(Snapshot()) = %d, want 1", len(snapshot))
	}
	if snapshot[0].Escalated != 1 {
		t.Errorf("Escalated = %d, want 1", snapshot[0].Escalated)
	}
}

func TestStatsKeepsTopicsSeparateAndSorted(t *testing.T) {
	s := NewStats()

	s.RecordAttempt(Meta{Topic: "crm.contact.update", Service: "crm", Action: "contact.update"})
	s.RecordAttempt(Meta{Topic: "ads.registration", Service: "ads", Action: "registration"})
	s.RecordAttempt(Meta{Topic: "crm.contact.update", Service: "crm", Action: "contact.update"})

	snapshot := s.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("len(Snapshot()) = %d, want 2 topics", len(snapshot))
	}
	if snapshot[0].Topic != "ads.registration" || snapshot[1].Topic != "crm.contact.update" {
		t.Errorf("topics = %q, %q, want them sorted", snapshot[0].Topic, snapshot[1].Topic)
	}
	if snapshot[1].Attempts != 2 {
		t.Errorf("crm attempts = %d, want 2", snapshot[1].Attempts)
	}
}

func TestStatsSnapshotIsACopy(t *testing.T) {
	s := NewStats()
	s.RecordAttempt(meta())

	snapshot := s.Snapshot()
	snapshot[0].Attempts = 999

	if again := s.Snapshot(); again[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want the snapshot to be a copy", again[0].Attempts)
	}
}

func TestStatsReset(t *testing.T) {
	s := NewStats()
	s.RecordAttempt(meta())
	s.Reset()

	if len(s.Snapshot()) != 0 {
		t.Error("Snapshot() is not empty after Reset()")
	}
}
