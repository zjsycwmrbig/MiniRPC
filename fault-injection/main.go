package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

type scenario func(context.Context) Result

// Result is one deterministic recovery drill observation.
type Result struct {
	Name        string        `json:"name"`
	Expectation string        `json:"expectation"`
	Attempts    int           `json:"attempts"`
	Failures    int           `json:"failures"`
	Recovery    time.Duration `json:"recovery_ns"`
	Passed      bool          `json:"passed"`
	Detail      string        `json:"detail"`
}

func (result Result) errorRate() float64 {
	if result.Attempts == 0 {
		return 0
	}
	return float64(result.Failures) / float64(result.Attempts)
}

func main() {
	selected := flag.String("scenario", "all", "all or comma-separated scenario names")
	timeout := flag.Duration("timeout", 15*time.Second, "overall drill timeout")
	jsonOutput := flag.Bool("json", false, "emit machine-readable JSON")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	registry := scenarioRegistry()
	names, err := selectScenarios(*selected, registry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	results := make([]Result, 0, len(names))
	failed := false
	for _, name := range names {
		result := runScenario(ctx, name, registry[name])
		if !result.Passed {
			failed = true
		}
		results = append(results, result)
	}

	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	} else {
		if err := printTable(results); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func runScenario(ctx context.Context, name string, drill scenario) (result Result) {
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			result = Result{
				Name:        name,
				Expectation: "scenario completes without panic",
				Detail:      fmt.Sprintf("panic: %v", recovered),
			}
		}
		if result.Name == "" {
			result.Name = name
		}
		if result.Detail == "" {
			result.Detail = fmt.Sprintf("completed in %s", time.Since(started).Round(time.Millisecond))
		}
	}()
	if ctx == nil {
		return Result{Name: name, Detail: "nil context"}
	}
	return drill(ctx)
}

func scenarioRegistry() map[string]scenario {
	return map[string]scenario{
		"instance-failover":   instanceFailoverScenario,
		"all-instances-down":  allInstancesDownScenario,
		"instance-recovery":   instanceRecoveryScenario,
		"response-error":      responseErrorScenario,
		"dropped-response":    droppedResponseScenario,
		"delayed-response":    delayedResponseScenario,
		"forced-disconnect":   forcedDisconnectScenario,
		"registry-outage":     registryOutageScenario,
		"resource-exhaustion": resourceExhaustionScenario,
	}
}

func selectScenarios(value string, registry map[string]scenario) ([]string, error) {
	if strings.TrimSpace(value) == "all" {
		return []string{
			"instance-failover",
			"all-instances-down",
			"instance-recovery",
			"response-error",
			"dropped-response",
			"delayed-response",
			"forced-disconnect",
			"registry-outage",
			"resource-exhaustion",
		}, nil
	}
	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, ok := registry[name]; !ok {
			return nil, fmt.Errorf("unknown scenario %q", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return names, nil
}

func printTable(results []Result) error {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "SCENARIO\tRESULT\tATTEMPTS\tFAILURES\tERROR_RATE\tRECOVERY\tDETAIL"); err != nil {
		return err
	}
	for _, result := range results {
		status := "PASS"
		if !result.Passed {
			status = "FAIL"
		}
		recovery := "-"
		if result.Recovery > 0 {
			recovery = result.Recovery.Round(time.Millisecond).String()
		}
		if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%d\t%d\t%.2f%%\t%s\t%s\n",
			result.Name,
			status,
			result.Attempts,
			result.Failures,
			result.errorRate()*100,
			recovery,
			result.Detail,
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}
