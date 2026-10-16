package demo

import (
	"strings"
	"testing"
	"time"
)

// TestScenarios runs every demo scenario as a test, so `go test ./...` covers
// the reliability claims the demos make. `make demo` runs the same code and
// prints the report.
//
// Scenarios run one at a time. Each starts a service, several destinations and
// a pool of delivery workers, so running six of them at once on a two-core
// runner adds contention that has nothing to do with what is being tested.
func TestScenarios(t *testing.T) {
	for _, scenario := range Scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			env := NewEnv()
			defer env.Close()

			result, err := scenario.Run(env)
			if err != nil {
				t.Fatalf("scenario could not run: %v\nservice log:\n%s", err, env.Logs())
			}

			for _, check := range result.Checks {
				if !check.OK {
					t.Errorf("%s\nservice log:\n%s", check.Detail, env.Logs())
				}
			}
			if len(result.Checks) == 0 {
				t.Error("scenario asserted nothing, so it cannot fail")
			}
		})
	}
}

func TestScenariosHaveDistinctNames(t *testing.T) {
	seen := make(map[string]bool)
	for _, s := range Scenarios() {
		if s.Name == "" {
			t.Error("a scenario has no name")
		}
		if seen[s.Name] {
			t.Errorf("duplicate scenario name %q", s.Name)
		}
		seen[s.Name] = true

		if s.Title == "" {
			t.Errorf("scenario %q has no title", s.Name)
		}
		if s.Explanation == "" {
			t.Errorf("scenario %q has no explanation, so a reader learns nothing from it", s.Name)
		}
		if s.Run == nil {
			t.Errorf("scenario %q has no run function", s.Name)
		}
	}
}

func TestLookup(t *testing.T) {
	for _, s := range Scenarios() {
		got, err := Lookup(s.Name)
		if err != nil {
			t.Errorf("Lookup(%q) error = %v", s.Name, err)
			continue
		}
		if got.Title != s.Title {
			t.Errorf("Lookup(%q) returned %q, want %q", s.Name, got.Title, s.Title)
		}
	}

	_, err := Lookup("nope")
	if err == nil {
		t.Fatal("Lookup(nope) error = nil, want an error")
	}
	// The message must name the alternatives, or it is not actionable.
	for _, s := range Scenarios() {
		if !strings.Contains(err.Error(), s.Name) {
			t.Errorf("Lookup error does not mention %q: %v", s.Name, err)
		}
	}
}

func TestEnvSetPublicURLBuildsResultURLs(t *testing.T) {
	env := NewEnv()
	defer env.Close()

	if got, want := env.BaseURL, env.Engine.ResultURL("ord_1", "step_a"); !strings.HasPrefix(want, got) {
		t.Errorf("ResultURL() = %q, want it to start with the public URL %q", want, got)
	}
	if got := env.Engine.ResultURL("ord_1", "step_a"); got != env.BaseURL+"/api/v1/orders/ord_1/steps/step_a/result" {
		t.Errorf("ResultURL() = %q", got)
	}
}

func TestWaitForReportsFailure(t *testing.T) {
	env := NewEnv()
	defer env.Close()

	// A condition that never holds must time out rather than hang forever.
	start := time.Now()
	if env.WaitFor("never", 50*time.Millisecond, func() bool { return false }) {
		t.Error("WaitFor() = true for a condition that never holds")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WaitFor() took %v, want it bounded by its budget", elapsed)
	}
}

func TestSubmitRejectsDuplicateOrder(t *testing.T) {
	env := NewEnv()
	defer env.Close()

	spec := map[string]any{
		"id":    "ord_duplicate",
		"steps": []map[string]any{{"name": "s", "topic": "a.b"}},
	}

	resp, err := env.Post("/api/v1/orders", spec)
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	resp.Body.Close()

	resp2, err := env.Post("/api/v1/orders", spec)
	if err != nil {
		t.Fatalf("second Post() error = %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != 409 {
		t.Errorf("status = %d, want 409 for a duplicate order id", resp2.StatusCode)
	}
}
