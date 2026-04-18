package simulation

import (
	"fmt"
	"time"
)

// Scenario pairs a named traffic pattern with the time-budget the runner
// enforces when driving load. The Runner propagates Duration onto the
// generator's Config.Duration so the two agree on when to stop.
type Scenario struct {
	Name      string
	Generator Generator
	Duration  time.Duration
}

const scenarioDuration = 60 * time.Second

// SteadyState — constant 100 RPS for 60s, default mix.
func SteadyState() Scenario {
	cfg := GeneratorConfig{Duration: scenarioDuration}
	return Scenario{
		Name:      "steady_state",
		Generator: NewConstantRateGenerator(100, cfg),
		Duration:  scenarioDuration,
	}
}

// RampUp — linear ramp 10→500 RPS over 60s.
func RampUp() Scenario {
	cfg := GeneratorConfig{Duration: scenarioDuration}
	return Scenario{
		Name:      "ramp_up",
		Generator: NewRampGenerator(10, 500, cfg),
		Duration:  scenarioDuration,
	}
}

// SpikeTest — 100 RPS base with a single 10× burst at t=30s for 10s.
// The bursty generator repeats (BurstInterval + BurstDuration), so
// picking BurstInterval=30s and BurstDuration=10s means exactly one
// burst fires within the 60s scenario duration (at t=30s).
func SpikeTest() Scenario {
	cfg := GeneratorConfig{Duration: scenarioDuration}
	return Scenario{
		Name:      "spike_test",
		Generator: NewBurstyGenerator(100, 10, 30*time.Second, 10*time.Second, cfg),
		Duration:  scenarioDuration,
	}
}

// MixedPriority — constant 100 RPS with the default 80/15/5 split.
// Explicitly sets the fractions so the scenario's intent is recorded
// at the call site.
func MixedPriority() Scenario {
	cfg := GeneratorConfig{
		Duration:         scenarioDuration,
		NormalFraction:   0.80,
		HighFraction:     0.15,
		CriticalFraction: 0.05,
	}
	return Scenario{
		Name:      "mixed_priority",
		Generator: NewConstantRateGenerator(100, cfg),
		Duration:  scenarioDuration,
	}
}

// LongTail — constant 100 RPS with a bimodal token distribution
// (70% ~100 tokens, 30% ~3000 tokens).
func LongTail() Scenario {
	cfg := GeneratorConfig{
		Duration:        scenarioDuration,
		BimodalLongTail: true,
	}
	return Scenario{
		Name:      "long_tail",
		Generator: NewConstantRateGenerator(100, cfg),
		Duration:  scenarioDuration,
	}
}

// AllScenarios returns every predefined scenario in its documentation
// order.
func AllScenarios() []Scenario {
	return []Scenario{
		SteadyState(),
		RampUp(),
		SpikeTest(),
		MixedPriority(),
		LongTail(),
	}
}

// ScenarioByName constructs a fresh Scenario for the given name so that
// the runner gets unshared generator state per invocation.
func ScenarioByName(name string) (Scenario, error) {
	switch name {
	case "steady_state":
		return SteadyState(), nil
	case "ramp_up":
		return RampUp(), nil
	case "spike_test":
		return SpikeTest(), nil
	case "mixed_priority":
		return MixedPriority(), nil
	case "long_tail":
		return LongTail(), nil
	default:
		return Scenario{}, fmt.Errorf("unknown scenario: %q", name)
	}
}
