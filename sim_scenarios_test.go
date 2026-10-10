// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Trace building blocks (t in ms) ---

func constant(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// steps returns a piecewise-constant function: values[i] applies from
// at[i] seconds onward (at[0] must be 0).
func steps(at []float64, values []float64) func(float64) float64 {
	return func(t float64) float64 {
		v := values[0]
		for i := range at {
			if t >= at[i]*1000 {
				v = values[i]
			}
		}
		return v
	}
}

// randomWalk returns a seeded, precomputed capacity trace that changes every
// stepMs, moving multiplicatively within [lo, hi]. fadeProb is the per-step
// chance of a deep fade to lo for 1-3s (cell edge, handover, blockage).
func randomWalk(seed uint64, dur time.Duration, stepMs, lo, hi, volatility, fadeProb float64) func(float64) float64 {
	rng := rand.New(rand.NewPCG(seed, 1))
	n := int(float64(dur.Milliseconds())/stepMs) + 1
	vals := make([]float64, n)
	v := math.Sqrt(lo * hi)
	fade := 0
	for i := range vals {
		v *= math.Exp(volatility * rng.NormFloat64())
		v = math.Max(lo, math.Min(hi, v))
		if fade == 0 && rng.Float64() < fadeProb {
			fade = int((1000 + 2000*rng.Float64()) / stepMs)
		}
		if fade > 0 {
			fade--
			vals[i] = lo
		} else {
			vals[i] = v
		}
	}
	return func(t float64) float64 { return vals[min(int(t/stepMs), n-1)] }
}

// starlinkHandovers returns a capacity trace that changes at each Starlink
// reconfiguration boundary (seconds 12/27/42/57 of every minute), and a
// matching per-reflector base RTT that steps to a new floor at each boundary.
func starlinkHandovers(seed uint64, dur time.Duration, lo, hi, baseLo, baseHi float64) (capacity func(float64) float64, baseRTT func(float64, int) float64) {
	rng := rand.New(rand.NewPCG(seed, 2))
	slots := int(dur.Seconds()/15) + 2
	caps := make([]float64, slots)
	bases := make([]float64, slots)
	for i := range caps {
		caps[i] = lo + (hi-lo)*rng.Float64()
		bases[i] = baseLo + (baseHi-baseLo)*rng.Float64()
	}
	slot := func(t float64) int { return int((t/1000 + 3) / 15) } // first boundary at 12s
	capacity = func(t float64) float64 { return caps[slot(t)] }
	baseRTT = func(t float64, i int) float64 { return bases[slot(t)] + 2*float64(i) }
	return capacity, baseRTT
}

func between(fromS, toS float64) func(float64) bool {
	return func(t float64) bool { return t >= fromS*1000 && t < toS*1000 }
}

func never(float64) bool { return false }

func reflectorBase(ms float64) func(float64, int) float64 {
	return func(_ float64, i int) float64 { return ms + 3*float64(i) }
}

func dirConfig(minKbps, baseKbps, maxKbps int) DirectionConfig {
	return DirectionConfig{
		MinRateKbps:                minKbps,
		BaseRateKbps:               baseKbps,
		MaxRateKbps:                maxKbps,
		OWDDeltaDelayThrMs:         30.0,
		AvgOWDDeltaMaxAdjUpThrMs:   10.0,
		AvgOWDDeltaMaxAdjDownThrMs: 60.0,
	}
}

// --- Scenarios ---

func simScenarios() []simScenario {
	const dur = 120 * time.Second
	link5G := LinkConfig{
		Download: dirConfig(10000, 100000, 300000),
		Upload:   dirConfig(2000, 20000, 60000),
	}
	slCap, slBase := starlinkHandovers(4, dur, 60000, 250000, 25, 55)
	slUlCap, _ := starlinkHandovers(5, dur, 8000, 25000, 25, 55)

	return []simScenario{
		{
			// Sanity check: constant capacity, download load only.
			name: "steady-dl", duration: dur, seed: 1,
			dlCapacity: constant(120000), ulCapacity: constant(25000),
			dlDemand: between(2, 118), ulDemand: never,
			bufferMs: 300, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// Capacity collapses 200 → 40 Mbit/s and recovers (finding #2:
			// cuts from shaper rate converge slowly).
			name: "capacity-drop", duration: dur, seed: 2,
			dlCapacity: steps([]float64{0, 40, 80}, []float64{200000, 40000, 200000}),
			ulCapacity: constant(25000),
			dlDemand:   between(2, 118), ulDemand: never,
			bufferMs: 500, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// Upload saturated alongside download (finding #4: RTT/2 makes
			// UL bloat cut DL as well).
			name: "bidir-saturated", duration: dur, seed: 3,
			dlCapacity: constant(150000), ulCapacity: constant(20000),
			dlDemand: between(2, 118), ulDemand: between(2, 118),
			bufferMs: 300, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// 5G-like: capacity random walk with occasional deep fades, both
			// directions loaded, large cellular buffers.
			name: "5g-variable", duration: dur, seed: 4,
			dlCapacity: randomWalk(10, dur, 200, 30000, 300000, 0.08, 0.004),
			ulCapacity: randomWalk(11, dur, 200, 5000, 50000, 0.08, 0.004),
			dlDemand:   between(2, 118), ulDemand: between(30, 90),
			bufferMs: 800, baseRTT: reflectorBase(25), jitterMs: 3,
			link: link5G,
		},
		{
			// Starlink-like: capacity and RTT floor change at the 15s
			// reconfiguration boundaries, heavy jitter (findings #3, #5).
			name: "starlink-like", duration: dur, seed: 5,
			dlCapacity: slCap, ulCapacity: slUlCap,
			dlDemand: between(2, 118), ulDemand: between(30, 90),
			bufferMs: 300, baseRTT: slBase, jitterMs: 6,
			link: LinkConfig{
				Download: dirConfig(20000, 100000, 300000),
				Upload:   dirConfig(3000, 15000, 40000),
			},
		},
		{
			// A 300ms latency spike that is not caused by queueing (finding #1:
			// every ping during the spike cuts the rate again).
			name: "latency-spike", duration: dur, seed: 6,
			dlCapacity: constant(120000), ulCapacity: constant(25000),
			dlDemand: between(2, 118), ulDemand: never,
			bufferMs: 300, jitterMs: 1,
			baseRTT: func(t float64, i int) float64 {
				base := 20 + 3*float64(i)
				if t >= 60000 && t < 60300 {
					base += 120
				}
				return base
			},
			link: link5G,
		},
	}
}

func scenarioByName(t *testing.T, name string) simScenario {
	t.Helper()
	for _, sc := range simScenarios() {
		if sc.name == name {
			return sc
		}
	}
	t.Fatalf("unknown scenario %q", name)
	return simScenario{}
}

// TestSimScorecard runs every scenario under each policy and prints a
// scorecard. Run with: go test -run TestSimScorecard -v
func TestSimScorecard(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation scorecard skipped in -short mode")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-16s %-10s %8s %8s %8s %8s %7s %7s %7s\n",
		"scenario", "policy", "p50 ms", "p95 ms", "p99 ms", "bloat s", "dl %", "ul %", "sets")
	for _, sc := range simScenarios() {
		for _, p := range []simPolicy{policyUnshaped, policyFixed, policyAutorate} {
			r := runSim(sc, p)
			fmt.Fprintf(&b, "%-16s %-10s %8.1f %8.1f %8.1f %8.1f %7.1f %7.1f %7d\n",
				r.scenario, r.policy, r.latP50, r.latP95, r.latP99, r.bloatS,
				100*r.dlUtil, 100*r.ulUtil, r.rateChanges)
		}
	}
	t.Log(b.String())
	if path := os.Getenv("SIM_SCORECARD_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// --- Simulator sanity checks ---

func TestSim_Deterministic(t *testing.T) {
	sc := scenarioByName(t, "5g-variable")
	sc.duration = 30 * time.Second
	a, b := runSim(sc, policyAutorate), runSim(sc, policyAutorate)
	if a.latP99 != b.latP99 || a.dlUtil != b.dlUtil || a.rateChanges != b.rateChanges {
		t.Errorf("runs diverged: %+v vs %+v", a, b)
	}
}

func TestSim_UnshapedBloats(t *testing.T) {
	// Without a shaper, a greedy sender fills the bottleneck buffer.
	sc := scenarioByName(t, "steady-dl")
	sc.duration = 30 * time.Second
	sc.dlDemand = between(2, 30)
	r := runSim(sc, policyUnshaped)
	if r.latP50 < 100 {
		t.Errorf("expected heavy bufferbloat unshaped (buffer 300ms), got p50=%.1fms", r.latP50)
	}
	if r.dlUtil < 0.9 {
		t.Errorf("expected near-full utilization unshaped, got %.2f", r.dlUtil)
	}
}

func TestSim_ShapedBelowCapacityStaysLow(t *testing.T) {
	// A fixed shaper below capacity keeps the bottleneck queue empty.
	sc := scenarioByName(t, "steady-dl")
	sc.duration = 30 * time.Second
	sc.dlDemand = between(2, 30)
	r := runSim(sc, policyFixed) // base 100M < capacity 120M
	// p99 includes the slow-start overshoot at flow start, before CAKE's
	// CoDel reacts; p95 reflects steady state.
	if r.latP95 > 5 {
		t.Errorf("expected low latency with shaper below capacity, got p95=%.1fms", r.latP95)
	}
	if r.dlUtil < 0.75 || r.dlUtil > 0.85 {
		t.Errorf("expected utilization ~100/120, got %.2f", r.dlUtil)
	}
}

// --- Known issues ---
//
// Each test asserts the behavior we want. While the issue is present the test
// logs it and passes; once a fix makes the assertion hold, the test fails to
// force flipping it into a regular regression test. Numbers refer to the
// findings in the improvement plan.

func knownIssue(t *testing.T, id string, ok bool, format string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(format, args...)
	if ok {
		t.Errorf("known issue %s appears fixed (%s): turn this into a regular assertion", id, detail)
		return
	}
	t.Logf("known issue %s still present: %s", id, detail)
}

// sliceS returns timeline samples (100ms resolution) between fromS and toS.
func sliceS(v []float64, fromS, toS float64) []float64 {
	return v[int(fromS*10):min(int(toS*10), len(v))]
}

func minOf(v []float64) float64 {
	m := math.Inf(1)
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}

// #1: a short latency spike that is not queueing should cost little rate.
// Every ping while the detection window is full cuts again, so a 300ms spike
// stacks several cuts.
func TestKnownIssue1_LatencySpikeOvercut(t *testing.T) {
	r := runSim(scenarioByName(t, "latency-spike"), policyAutorate)
	low := minOf(sliceS(r.dlShaper, 60, 63))
	const capacity = 120000.0
	knownIssue(t, "#1", low >= 0.75*capacity,
		"300ms spike cut DL shaper to %.0f kbps (%.0f%% of %.0f kbps capacity)", low, 100*low/capacity, capacity)
}

// #1/#2: after capacity collapses, the shaper should converge near the new
// capacity quickly without plunging far below it.
func TestKnownIssue2_CapacityDropConvergence(t *testing.T) {
	r := runSim(scenarioByName(t, "capacity-drop"), policyAutorate)
	const capacity = 40000.0
	low := minOf(sliceS(r.dlShaper, 40, 50))
	after1s := r.dlShaper[41*10]
	ok := low >= 0.5*capacity && after1s <= 1.25*capacity
	knownIssue(t, "#2", ok,
		"after 200→40 Mbit/s drop: shaper %.0f kbps 1s later, minimum %.0f kbps in the next 10s", after1s, low)
}

// #4: saturating upload should not throttle download. RTT/2 attributes UL
// queueing to both directions.
func TestKnownIssue4_UploadBloatThrottlesDownload(t *testing.T) {
	r := runSim(scenarioByName(t, "bidir-saturated"), policyAutorate)
	knownIssue(t, "#4", r.dlUtil >= 0.75,
		"DL utilization %.0f%% while UL is saturated", 100*r.dlUtil)
}

// #5: a step in the RTT floor (Starlink reconfiguration, route change) is not
// queueing and should not cost throughput once the new floor is learned.
func TestKnownIssue5_BaselineStepCostsThroughput(t *testing.T) {
	sc := scenarioByName(t, "steady-dl")
	sc.name = "baseline-step"
	sc.baseRTT = func(t float64, i int) float64 {
		base := 20 + 3*float64(i)
		if t >= 60000 {
			base += 40
		}
		return base
	}
	r := runSim(sc, policyAutorate)
	before := mean(sliceS(r.dlShaper, 30, 60))
	after := mean(sliceS(r.dlShaper, 70, 118))
	knownIssue(t, "#5", after >= 0.9*before,
		"mean DL shaper %.0f kbps before a +40ms RTT floor step, %.0f kbps after", before, after)
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
