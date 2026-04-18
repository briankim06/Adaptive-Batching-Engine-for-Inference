// Command simulator drives load through the real gateway (batcher,
// worker pool, collector) against an in-process mock upstream and
// prints a strategy × scenario comparison matrix. See
// docs/spec/07-simulation.md §7.4 for the CLI contract.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/simulation"
)

const (
	latencyAwareKP   = 0.05
	defaultStrategies = "all"
	defaultScenarios  = "all"
)

var allStrategyNames = []string{"fixed", "queue_depth", "latency_aware"}
var allScenarioNames = []string{"steady_state", "ramp_up", "spike_test", "mixed_priority", "long_tail"}

func main() {
	scenariosFlag := flag.String("scenarios", defaultScenarios, "comma-separated scenario names, or 'all'")
	strategiesFlag := flag.String("strategies", defaultStrategies, "comma-separated strategy names, or 'all'")
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	output := flag.String("output", "table", "output format: table | csv | json")
	flag.Parse()

	if err := runSimulator(*scenariosFlag, *strategiesFlag, *configPath, *output); err != nil {
		fmt.Fprintln(os.Stderr, "simulator:", err)
		os.Exit(1)
	}
}

func runSimulator(scenariosArg, strategiesArg, configPath, output string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	scenarioNames, err := resolveScenarios(scenariosArg)
	if err != nil {
		return err
	}
	strategyNames, err := resolveStrategies(strategiesArg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := simulation.NewRunner(cfg)
	mockCfg := simulation.DefaultMockUpstreamConfig()

	results := make([]simulation.SimulationResult, 0, len(scenarioNames)*len(strategyNames))
	for _, sname := range scenarioNames {
		for _, stratName := range strategyNames {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			scenario, err := simulation.ScenarioByName(sname)
			if err != nil {
				return err
			}
			strategy, err := buildStrategy(stratName, cfg.Batching)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "running scenario=%s strategy=%s duration=%s\n",
				sname, stratName, scenario.Duration)
			res := runner.Run(ctx, scenario, strategy, mockCfg)
			results = append(results, res)
		}
	}

	return writeOutput(os.Stdout, output, results)
}

// loadConfig resolves --config by ensuring config.Load()'s viper search
// paths ("." and "./config") can find the requested file. The file must
// be named config.yaml — that constraint is a consequence of the shared
// config.Load() implementation, not a design choice of this CLI.
func loadConfig(path string) (*config.Config, error) {
	if path != "" && path != "config.yaml" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve --config path: %w", err)
		}
		dir, base := filepath.Split(abs)
		if base != "config.yaml" {
			return nil, fmt.Errorf("--config must point to a file named config.yaml (got %q)", base)
		}
		if err := os.Chdir(dir); err != nil {
			return nil, fmt.Errorf("chdir to %q: %w", dir, err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func resolveScenarios(arg string) ([]string, error) {
	return resolveList(arg, allScenarioNames, "scenarios")
}

func resolveStrategies(arg string) ([]string, error) {
	return resolveList(arg, allStrategyNames, "strategies")
}

func resolveList(arg string, all []string, label string) ([]string, error) {
	if arg == "" || arg == "all" {
		out := make([]string, len(all))
		copy(out, all)
		return out, nil
	}
	parts := strings.Split(arg, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if !contains(all, name) {
			return nil, fmt.Errorf("unknown %s %q (known: %s)", label, name, strings.Join(all, ","))
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s selected", label)
	}
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// buildStrategy constructs a fresh strategy per run so nothing leaks
// between runs (notably LatencyAwareStrategy's internal multiplier).
func buildStrategy(name string, cfg config.BatchingConfig) (strategies.Strategy, error) {
	switch name {
	case "fixed":
		return strategies.NewFixedStrategy(cfg.MaxWaitMs, cfg.MaxBatchSize), nil
	case "queue_depth":
		return strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold,
			cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs,
			cfg.MaxWaitMs,
			cfg.MaxBatchSize,
		), nil
	case "latency_aware":
		base := strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold,
			cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs,
			cfg.MaxWaitMs,
			cfg.MaxBatchSize,
		)
		return strategies.NewLatencyAwareStrategy(base, latencyAwareKP, cfg.TargetP99Ms), nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}

func writeOutput(w io.Writer, format string, results []simulation.SimulationResult) error {
	switch format {
	case "", "table":
		return writeTable(w, results)
	case "csv":
		return writeCSV(w, results)
	case "json":
		return writeJSON(w, results)
	default:
		return fmt.Errorf("unknown output format %q (want table|csv|json)", format)
	}
}

func writeTable(w io.Writer, results []simulation.SimulationResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Scenario\tStrategy\tRPS\tP50\tP90\tP99\tErrorRate\tAvgBatch")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ScenarioName,
			r.StrategyName,
			formatFloat(r.ThroughputRPS, 1),
			formatFloat(r.LatencyP50Ms, 2),
			formatFloat(r.LatencyP90Ms, 2),
			formatFloat(r.LatencyP99Ms, 2),
			formatFloat(r.ErrorRate*100, 2)+"%",
			formatFloat(r.AvgBatchSize, 2),
		)
	}
	return tw.Flush()
}

func writeCSV(w io.Writer, results []simulation.SimulationResult) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{
		"scenario", "strategy", "duration_ms", "total_requests", "total_errors",
		"error_rate", "throughput_rps", "latency_p50_ms", "latency_p90_ms",
		"latency_p99_ms", "avg_batch_size",
	}); err != nil {
		return err
	}
	for _, r := range results {
		row := []string{
			r.ScenarioName,
			r.StrategyName,
			strconv.FormatInt(r.Duration.Milliseconds(), 10),
			strconv.FormatInt(r.TotalRequests, 10),
			strconv.FormatInt(r.TotalErrors, 10),
			formatFloat(r.ErrorRate, 4),
			formatFloat(r.ThroughputRPS, 2),
			formatFloat(r.LatencyP50Ms, 2),
			formatFloat(r.LatencyP90Ms, 2),
			formatFloat(r.LatencyP99Ms, 2),
			formatFloat(r.AvgBatchSize, 2),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(w io.Writer, results []simulation.SimulationResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func formatFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}
