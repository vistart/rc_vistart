// Command demo runs the typical delivery scenarios against a real rc_vistart
// and real simulated destinations.
//
// It doubles as an end-to-end check: every scenario asserts what it expects, and
// the process exits non-zero if any of them fail, so `make demo` is both a
// demonstration and a test.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vistart/rc_vistart/internal/demo"
)

func main() {
	var (
		list    = flag.Bool("list", false, "list the scenarios and exit")
		verbose = flag.Bool("v", false, "include the service log for every scenario")
		timeout = flag.Duration("timeout", 5*time.Minute, "overall budget")
		only    = flag.String("scenario", "", "run only this scenario (repeatable, comma separated)")
	)
	flag.Parse()

	if *list {
		printList()
		return
	}

	selected, err := selectScenarios(*only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	fmt.Println("rc_vistart demo scenarios")
	fmt.Println(strings.Repeat("=", 72))

	deadline := time.Now().Add(*timeout)
	var (
		passed int
		failed int
	)

	for i, scenario := range selected {
		if time.Now().After(deadline) {
			fmt.Printf("\noverall timeout of %s reached, stopping\n", *timeout)
			break
		}

		result := run(scenario, *verbose)
		header := fmt.Sprintf("%d/%d  %-9s %s", i+1, len(selected), scenario.Name, result.Title)
		fmt.Printf("\n%s\n", header)
		fmt.Println(strings.Repeat("-", 96))

		if result.Explanation != "" {
			for _, line := range wrap(result.Explanation, 92) {
				fmt.Printf("  %s\n", line)
			}
			fmt.Println()
		}

		for _, check := range result.Checks {
			mark := "ok  "
			if !check.OK {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %s\n", mark, check.Detail)
		}

		if len(result.Steps) > 0 {
			fmt.Println()
			printSteps(result.Steps)
		}
		for _, note := range result.Notes {
			fmt.Printf("\n  note: %s\n", note)
		}

		verdict := "PASS"
		if !result.Passed {
			verdict = "FAIL"
			failed++
			if result.Log != "" {
				fmt.Printf("\n  service log:\n%s\n", indent(result.Log, "    "))
			}
		} else {
			passed++
		}
		fmt.Printf("\n  %s  %s (%s)\n", verdict, result.Name, result.Duration.Round(time.Millisecond))
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("%d passed, %d failed, %d total\n", passed, failed, len(selected))
	if failed > 0 {
		os.Exit(1)
	}
}

func run(scenario demo.Scenario, verbose bool) demo.Result {
	env := demo.NewEnv(verbose)
	defer env.Close()

	result, err := scenario.Run(env)
	if err != nil {
		return demo.Result{
			Name:   scenario.Name,
			Title:  scenario.Title,
			Passed: false,
			Checks: []demo.Check{{OK: false, Detail: "scenario could not run: " + err.Error()}},
			Log:    env.Logs(),
		}
	}
	result.Log = env.Logs()
	if verbose {
		result.Log = env.Logs()
	}
	return result
}

func printSteps(steps []demo.StepRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  step\tphase\tkind\tstate\tattempts\tnotified\tapplied\tlast outcome")
	for _, s := range steps {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			s.Name, s.Phase, s.Kind, s.State, s.Attempts, s.Notified, s.Applied, s.Outcome)
	}
	w.Flush()
}

func printList() {
	fmt.Println("scenarios:")
	for _, s := range demo.Scenarios() {
		fmt.Printf("  %-10s %s\n", s.Name, s.Title)
	}
	fmt.Println("\nrun one with: demo -scenario <name>")
}

// selectScenarios resolves the requested names, defaulting to all of them.
func selectScenarios(request string) ([]demo.Scenario, error) {
	if strings.TrimSpace(request) == "" {
		return demo.Scenarios(), nil
	}

	var out []demo.Scenario
	for _, name := range strings.Split(request, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s, err := demo.Lookup(name)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return out, nil
}

// wrap breaks text into lines of at most width runes.
func wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var (
		lines   []string
		current string
	)
	for _, w := range words {
		switch {
		case current == "":
			current = w
		case len(current)+1+len(w) <= width:
			current += " " + w
		default:
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func indent(text, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
