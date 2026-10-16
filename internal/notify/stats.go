package notify

import (
	"sort"
	"sync"
	"time"
)

// Meta identifies the endpoint a statistic belongs to.
type Meta struct {
	Topic   string
	Service string
	Action  string
	URL     string
}

// TopicStats is the observed behaviour of one endpoint.
type TopicStats struct {
	Topic   string `json:"topic"`
	Service string `json:"service"`
	Action  string `json:"action"`
	URL     string `json:"url"`

	// Attempts counts every delivery attempt, including retries.
	Attempts int64 `json:"attempts"`
	// Delivered counts attempts acknowledged with an expected status.
	Delivered int64 `json:"delivered"`
	// Failed counts attempts that did not succeed.
	Failed int64 `json:"failed"`
	// Escalated counts tasks that exhausted their attempt budget.
	Escalated int64 `json:"escalated"`

	TotalLatencyMs int64      `json:"total_latency_ms"`
	LastError      string     `json:"last_error,omitempty"`
	LastErrorAt    *time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
}

// AverageLatencyMs is the mean latency of acknowledged attempts.
func (s TopicStats) AverageLatencyMs() float64 {
	if s.Delivered == 0 {
		return 0
	}
	return float64(s.TotalLatencyMs) / float64(s.Delivered)
}

// Stats aggregates delivery outcomes per topic. It is the monitoring surface
// for "the receiver accepted it" and, just as importantly, "it never answered
// at all" — which is what triggers a retry in the first place.
type Stats struct {
	mu      sync.Mutex
	byTopic map[string]*TopicStats
	now     func() time.Time
}

// NewStats returns empty statistics.
func NewStats() *Stats {
	return &Stats{byTopic: make(map[string]*TopicStats), now: time.Now}
}

func (s *Stats) entry(meta Meta) *TopicStats {
	ts, ok := s.byTopic[meta.Topic]
	if !ok {
		ts = &TopicStats{Topic: meta.Topic, Service: meta.Service, Action: meta.Action, URL: meta.URL}
		s.byTopic[meta.Topic] = ts
	}
	return ts
}

// RecordAttempt notes that an attempt started.
func (s *Stats) RecordAttempt(meta Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := s.entry(meta)
	ts.Attempts++
	ts.URL = meta.URL
	now := s.now().UTC()
	ts.LastAttemptAt = &now
}

// RecordDelivered notes an attempt acknowledged with an expected status.
func (s *Stats) RecordDelivered(meta Meta, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := s.entry(meta)
	ts.Delivered++
	ts.TotalLatencyMs += latency.Milliseconds()
	now := s.now().UTC()
	ts.LastSuccessAt = &now
}

// RecordFailed notes an attempt that did not succeed.
func (s *Stats) RecordFailed(meta Meta, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := s.entry(meta)
	ts.Failed++
	if err != nil {
		ts.LastError = err.Error()
		now := s.now().UTC()
		ts.LastErrorAt = &now
	}
}

// RecordEscalated notes that a task exhausted its attempts.
func (s *Stats) RecordEscalated(meta Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entry(meta).Escalated++
}

// Snapshot returns the statistics sorted by topic.
func (s *Stats) Snapshot() []TopicStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]TopicStats, 0, len(s.byTopic))
	for _, ts := range s.byTopic {
		out = append(out, *ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// Reset clears the statistics, which keeps tests independent.
func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byTopic = make(map[string]*TopicStats)
}
