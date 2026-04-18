// Package simulation provides a load-testing harness that drives real
// gateway components (batcher, worker pool, collector) against an
// in-process mock upstream. The production code path is unchanged —
// only the target upstream.url is swapped for a httptest.Server URL.
//
// See docs/spec/07-simulation.md for the full spec.
package simulation

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// Generator produces a stream of InferenceRequests over time. Generate
// returns a channel that emits requests according to the generator's
// configured rate; it closes the channel when ctx is cancelled or the
// generator's duration has expired.
type Generator interface {
	Generate(ctx context.Context) <-chan *models.InferenceRequest
}

// GeneratorConfig parameterises the request mix: arrival rate, request
// type split, priority split, and token-length distribution. The
// per-generator rate fields (RPS, StartRPS/EndRPS, etc.) live on the
// individual generator structs.
type GeneratorConfig struct {
	// Duration is the total wall-clock time the generator emits for.
	Duration time.Duration

	// CompletionFraction is the probability a request is a completion.
	// Defaults to 0.9 via defaults() when set to 0.
	CompletionFraction float64

	// Priority distribution — fractions must sum to <= 1.0. Any residual
	// mass falls to PriorityLow. Defaults: 0.80 normal, 0.15 high,
	// 0.05 critical, 0.0 low.
	NormalFraction   float64
	HighFraction     float64
	CriticalFraction float64

	// Token-length distribution: normal(mean, stddev) clamped to [1, 8192].
	MeanTokens   float64
	StddevTokens float64

	// BimodalLongTail, when true, overrides the token distribution with a
	// bimodal mix used by the long_tail scenario (70% short, 30% long).
	BimodalLongTail bool

	// Rand is the random source. Tests may set this; production leaves it
	// nil and the generators use rand.New(rand.NewSource(time.Now())) on
	// first use.
	Rand *rand.Rand
}

// defaults fills in zero-valued fields with the documented defaults. It
// returns a copy so callers that passed a partially-populated config get
// a ready-to-use value without mutating their original.
func (c GeneratorConfig) defaults() GeneratorConfig {
	out := c
	if out.CompletionFraction == 0 {
		out.CompletionFraction = 0.9
	}
	if out.NormalFraction == 0 && out.HighFraction == 0 && out.CriticalFraction == 0 {
		out.NormalFraction = 0.80
		out.HighFraction = 0.15
		out.CriticalFraction = 0.05
	}
	if out.MeanTokens == 0 {
		out.MeanTokens = 256
	}
	if out.StddevTokens == 0 {
		out.StddevTokens = 64
	}
	if out.Rand == nil {
		out.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return out
}

// ConstantRateGenerator emits at a fixed RPS using a time.Ticker with
// inter-arrival interval 1s/rps.
type ConstantRateGenerator struct {
	RPS    int
	Config GeneratorConfig
}

func NewConstantRateGenerator(rps int, cfg GeneratorConfig) *ConstantRateGenerator {
	return &ConstantRateGenerator{RPS: rps, Config: cfg}
}

func (g *ConstantRateGenerator) Generate(ctx context.Context) <-chan *models.InferenceRequest {
	out := make(chan *models.InferenceRequest)
	cfg := g.Config.defaults()
	rps := g.RPS
	if rps < 1 {
		rps = 1
	}
	interval := time.Second / time.Duration(rps)
	go func() {
		defer close(out)
		genCtx, cancel := withDuration(ctx, cfg.Duration)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-genCtx.Done():
				return
			case <-ticker.C:
				if !emit(genCtx, out, randomRequest(genCtx, cfg)) {
					return
				}
			}
		}
	}()
	return out
}

// PoissonGenerator emits with exponentially distributed inter-arrival
// times with mean 1s/rps, producing a Poisson arrival process.
type PoissonGenerator struct {
	RPS    int
	Config GeneratorConfig
}

func NewPoissonGenerator(rps int, cfg GeneratorConfig) *PoissonGenerator {
	return &PoissonGenerator{RPS: rps, Config: cfg}
}

func (g *PoissonGenerator) Generate(ctx context.Context) <-chan *models.InferenceRequest {
	out := make(chan *models.InferenceRequest)
	cfg := g.Config.defaults()
	rps := float64(g.RPS)
	if rps <= 0 {
		rps = 1
	}
	go func() {
		defer close(out)
		genCtx, cancel := withDuration(ctx, cfg.Duration)
		defer cancel()
		for {
			interval := exponentialInterval(cfg.Rand, rps)
			select {
			case <-genCtx.Done():
				return
			case <-time.After(interval):
				if !emit(genCtx, out, randomRequest(genCtx, cfg)) {
					return
				}
			}
		}
	}()
	return out
}

// RampGenerator linearly ramps RPS from StartRPS to EndRPS over
// Config.Duration, recalculating the inter-arrival interval each tick.
type RampGenerator struct {
	StartRPS int
	EndRPS   int
	Config   GeneratorConfig
}

func NewRampGenerator(startRPS, endRPS int, cfg GeneratorConfig) *RampGenerator {
	return &RampGenerator{StartRPS: startRPS, EndRPS: endRPS, Config: cfg}
}

func (g *RampGenerator) Generate(ctx context.Context) <-chan *models.InferenceRequest {
	out := make(chan *models.InferenceRequest)
	cfg := g.Config.defaults()
	start := float64(g.StartRPS)
	end := float64(g.EndRPS)
	if start < 1 {
		start = 1
	}
	if end < 1 {
		end = 1
	}
	duration := cfg.Duration
	if duration <= 0 {
		duration = 60 * time.Second
	}
	go func() {
		defer close(out)
		genCtx, cancel := withDuration(ctx, duration)
		defer cancel()
		startedAt := time.Now()
		for {
			elapsed := time.Since(startedAt)
			frac := float64(elapsed) / float64(duration)
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			rps := start + (end-start)*frac
			if rps < 1 {
				rps = 1
			}
			interval := time.Duration(float64(time.Second) / rps)
			select {
			case <-genCtx.Done():
				return
			case <-time.After(interval):
				if !emit(genCtx, out, randomRequest(genCtx, cfg)) {
					return
				}
			}
		}
	}()
	return out
}

// BurstyGenerator emits at BaseRPS most of the time and switches to
// BurstMultiplier*BaseRPS for BurstDuration windows spaced BurstInterval
// apart. The first burst begins after the first BurstInterval.
type BurstyGenerator struct {
	BaseRPS         int
	BurstMultiplier int
	BurstInterval   time.Duration
	BurstDuration   time.Duration
	Config          GeneratorConfig
}

func NewBurstyGenerator(baseRPS, burstMultiplier int, burstInterval, burstDuration time.Duration, cfg GeneratorConfig) *BurstyGenerator {
	return &BurstyGenerator{
		BaseRPS:         baseRPS,
		BurstMultiplier: burstMultiplier,
		BurstInterval:   burstInterval,
		BurstDuration:   burstDuration,
		Config:          cfg,
	}
}

func (g *BurstyGenerator) Generate(ctx context.Context) <-chan *models.InferenceRequest {
	out := make(chan *models.InferenceRequest)
	cfg := g.Config.defaults()
	base := g.BaseRPS
	if base < 1 {
		base = 1
	}
	mult := g.BurstMultiplier
	if mult < 1 {
		mult = 1
	}
	burst := base * mult
	baseInterval := time.Second / time.Duration(base)
	burstInterval := time.Second / time.Duration(burst)
	go func() {
		defer close(out)
		genCtx, cancel := withDuration(ctx, cfg.Duration)
		defer cancel()
		startedAt := time.Now()
		for {
			elapsed := time.Since(startedAt)
			interval := baseInterval
			if g.BurstInterval > 0 && g.BurstDuration > 0 && elapsed >= g.BurstInterval {
				within := (elapsed - g.BurstInterval) % (g.BurstInterval + g.BurstDuration)
				if within < g.BurstDuration {
					interval = burstInterval
				}
			}
			select {
			case <-genCtx.Done():
				return
			case <-time.After(interval):
				if !emit(genCtx, out, randomRequest(genCtx, cfg)) {
					return
				}
			}
		}
	}()
	return out
}

// randomRequest builds an InferenceRequest whose Priority, RequestType
// and prompt size are drawn from cfg's distributions. The returned
// request carries reqCtx so the batcher's Submit and the worker's
// context checkpoints treat simulated requests identically to real
// HTTP-borne requests (invariant I1).
func randomRequest(reqCtx context.Context, cfg GeneratorConfig) *models.InferenceRequest {
	cfg = cfg.defaults()
	reqType := models.RequestTypeCompletion
	if cfg.Rand.Float64() >= cfg.CompletionFraction {
		reqType = models.RequestTypeEmbedding
	}
	priority := samplePriority(cfg)
	tokens := sampleTokens(cfg)
	prompt := buildPrompt(cfg.Rand, tokens)
	return models.NewInferenceRequest(reqCtx, prompt, tokens, priority, reqType)
}

func samplePriority(cfg GeneratorConfig) models.Priority {
	r := cfg.Rand.Float64()
	critical := cfg.CriticalFraction
	high := critical + cfg.HighFraction
	normal := high + cfg.NormalFraction
	switch {
	case r < critical:
		return models.PriorityCritical
	case r < high:
		return models.PriorityHigh
	case r < normal:
		return models.PriorityNormal
	default:
		return models.PriorityLow
	}
}

func sampleTokens(cfg GeneratorConfig) int {
	if cfg.BimodalLongTail {
		if cfg.Rand.Float64() < 0.7 {
			return clampInt(int(cfg.Rand.NormFloat64()*20+100), 1, 8192)
		}
		return clampInt(int(cfg.Rand.NormFloat64()*300+3000), 1, 8192)
	}
	v := cfg.Rand.NormFloat64()*cfg.StddevTokens + cfg.MeanTokens
	return clampInt(int(math.Round(v)), 1, 8192)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// buildPrompt generates a whitespace-separated string sized to roughly
// produce the requested token estimate. NewInferenceRequest computes
// EstimatedTokens = len(prompt)/4 + maxTokens, so 4 chars/token is the
// target word length.
func buildPrompt(r *rand.Rand, tokens int) string {
	if tokens <= 0 {
		return "hi"
	}
	wordCount := tokens
	if wordCount < 1 {
		wordCount = 1
	}
	var b strings.Builder
	b.Grow(wordCount * 4)
	for i := 0; i < wordCount; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(randomWord(r, 3+r.Intn(5)))
	}
	return b.String()
}

const alphabet = "abcdefghijklmnopqrstuvwxyz"

func randomWord(r *rand.Rand, n int) string {
	if n < 1 {
		n = 1
	}
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(buf)
}

// exponentialInterval returns a sample from Exp(rps). The clamp avoids
// zero / negative intervals from an unlucky Uniform(0).
func exponentialInterval(r *rand.Rand, rps float64) time.Duration {
	u := r.Float64()
	if u < 1e-9 {
		u = 1e-9
	}
	interval := -math.Log(u) / rps
	if interval < 0 {
		interval = 0
	}
	d := time.Duration(interval * float64(time.Second))
	if d < time.Microsecond {
		d = time.Microsecond
	}
	return d
}

// emit performs a ctx-aware send onto the generator's output channel.
// Returns false when the context is done so the caller can exit its
// loop.
func emit(ctx context.Context, out chan<- *models.InferenceRequest, req *models.InferenceRequest) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- req:
		return true
	}
}

// withDuration wraps ctx with a timeout of d. A zero or negative d
// returns a cancellable copy without a deadline — the scenario's outer
// ctx is still respected.
func withDuration(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
