// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- Trace building blocks (t in ms) ---

func constant(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// steps returns a piecewise-constant function. values[i] applies from
// at[i] seconds until the next step. at[0] must be 0.
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

// randomWalk returns a capacity trace from a seeded random walk. The value
// changes every stepMs by a random factor and stays in [lo, hi]. fadeProb is
// the probability for each step of a deep fade to lo for 1-3s (cell edge,
// handover, blockage).
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

// radioStalls adds short stalls to a capacity trace (the 5G link of
// 2026-10-18 showed them under download load): every 3 to 10 s, the
// capacity falls to factor times the capacity for 150 to 450 ms.
func radioStalls(seed uint64, dur time.Duration, capacity func(float64) float64, factor float64) func(float64) float64 {
	rng := rand.New(rand.NewPCG(seed, 3))
	type window struct{ from, to float64 }
	var ws []window
	for t := 2000 + 3000*rng.Float64(); t < float64(dur.Milliseconds()); t += 3000 + 7000*rng.Float64() {
		ws = append(ws, window{t, t + 150 + 300*rng.Float64()})
	}
	return func(t float64) float64 {
		for _, w := range ws {
			if t >= w.from && t < w.to {
				return capacity(t) * factor
			}
		}
		return capacity(t)
	}
}

// starlinkHandovers returns a capacity trace and a base RTT for each
// reflector. The two change at each Starlink reconfiguration boundary
// (seconds 12/27/42/57 of each minute). At each boundary, the base RTT
// changes to a new floor.
func starlinkHandovers(seed uint64, dur time.Duration, lo, hi, baseLo, baseHi float64) (capacity func(float64) float64, baseRTT func(float64, int) float64) {
	rng := rand.New(rand.NewPCG(seed, 2))
	slots := int(dur.Seconds()/15) + 2
	caps := make([]float64, slots)
	bases := make([]float64, slots)
	for i := range caps {
		caps[i] = lo + (hi-lo)*rng.Float64()
		bases[i] = baseLo + (baseHi-baseLo)*rng.Float64()
	}
	slot := func(t float64) int { return int((t/1000 + 3) / 15) } // the first boundary is at 12s
	capacity = func(t float64) float64 { return caps[slot(t)] }
	baseRTT = func(t float64, i int) float64 { return bases[slot(t)] + 2*float64(i) }
	return capacity, baseRTT
}

// recordedTrace reads a capacity trace (CSV "t_ms,dl_kbps,ul_kbps", from
// scripts/trace-from-session.sh or scripts/gen-trace.sh) and returns the
// capacity of each direction. Each line applies until the next line. After
// the last line, the trace starts again.
func recordedTrace(path string) (dl, ul func(float64) float64) {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var at, dv, uv []float64
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if i == 0 {
			continue // header
		}
		var t, d, u float64
		if _, err := fmt.Sscanf(line, "%g,%g,%g", &t, &d, &u); err != nil {
			panic(fmt.Sprintf("%s line %d: %v", path, i+1, err))
		}
		at, dv, uv = append(at, t), append(dv, d), append(uv, u)
	}
	switch len(at) {
	case 0:
		panic(fmt.Sprintf("%s: no data lines", path))
	case 1:
		// One line: the capacity is constant.
		return constant(dv[0]), constant(uv[0])
	}
	period := at[len(at)-1] + (at[len(at)-1] - at[len(at)-2])
	lookup := func(v []float64) func(float64) float64 {
		return func(t float64) float64 {
			i, _ := slices.BinarySearch(at, math.Mod(t, period))
			if i == len(at) || at[i] > math.Mod(t, period) {
				i--
			}
			return v[max(i, 0)]
		}
	}
	return lookup(dv), lookup(uv)
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

// simSeeds is the number of seed variants for each scenario in the
// scorecard. More variants give a result that depends less on one random
// trace. To use it: go test -run TestSimScorecard -v -sim.seeds 10
var simSeeds = flag.Int("sim.seeds", 1, "number of seed variants for each scenario in TestSimScorecard")

// simScenarios returns the scenarios with their default seeds (variant 0).
// The baseline and the tests use these.
func simScenarios() []simScenario { return simScenariosVariant(0) }

// simScenariosVariant returns the scenarios with different seeds for the
// random traces and the jitter. Variant 0 gives the default seeds.
func simScenariosVariant(variant uint64) []simScenario {
	const dur = 120 * time.Second
	v := 100 * variant
	link5G := LinkConfig{
		Download: dirConfig(10000, 100000, 300000),
		Upload:   dirConfig(2000, 20000, 60000),
	}
	recADl, recAUl := recordedTrace("testdata/traces/5g-a.csv")
	recBDl, recBUl := recordedTrace("testdata/traces/5g-b.csv")
	link5GRec := LinkConfig{
		Download: dirConfig(30000, 150000, 800000),
		Upload:   dirConfig(4000, 18000, 45000),
	}
	slCap, slBase := starlinkHandovers(4+v, dur, 60000, 250000, 25, 55)
	slUlCap, _ := starlinkHandovers(5+v, dur, 8000, 25000, 25, 55)

	return []simScenario{
		{
			// Basic check: constant capacity, load on download only.
			name: "steady-dl", duration: dur, seed: 1 + v,
			dlCapacity: constant(120000), ulCapacity: constant(25000),
			dlDemand: between(2, 118), ulDemand: never,
			bufferMs: 300, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// The capacity decreases from 200 to 40 Mbit/s and then increases
			// again (finding F2: cuts from the shaper rate converge slowly).
			name: "capacity-drop", duration: dur, seed: 2 + v,
			dlCapacity: steps([]float64{0, 40, 80}, []float64{200000, 40000, 200000}),
			ulCapacity: constant(25000),
			dlDemand:   between(2, 118), ulDemand: never,
			bufferMs: 500, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// Upload and download are both saturated (finding F4: with RTT/2,
			// UL bloat also cuts DL).
			name: "bidir-saturated", duration: dur, seed: 3 + v,
			dlCapacity: constant(150000), ulCapacity: constant(20000),
			dlDemand: between(2, 118), ulDemand: between(2, 118),
			bufferMs: 300, baseRTT: reflectorBase(20), jitterMs: 1,
			link: link5G,
		},
		{
			// Similar to 5G: random walk capacity with some deep fades, load in
			// the two directions, large cellular buffers.
			name: "5g-variable", duration: dur, seed: 4 + v,
			dlCapacity: randomWalk(10+v, dur, 200, 30000, 300000, 0.08, 0.004),
			ulCapacity: randomWalk(11+v, dur, 200, 5000, 50000, 0.08, 0.004),
			dlDemand:   between(2, 118), ulDemand: between(30, 90),
			bufferMs: 800, baseRTT: reflectorBase(25), jitterMs: 3,
			link: link5G,
		},
		{
			// Similar to Starlink: the capacity and the RTT floor change at the
			// 15s reconfiguration boundaries, with large jitter (findings F3, F5).
			name: "starlink-like", duration: dur, seed: 5 + v,
			dlCapacity: slCap, ulCapacity: slUlCap,
			dlDemand: between(2, 118), ulDemand: between(30, 90),
			bufferMs: 300, baseRTT: slBase, jitterMs: 6,
			link: LinkConfig{
				Download: dirConfig(20000, 100000, 300000),
				Upload:   dirConfig(3000, 15000, 40000),
			},
		},
		{
			// The capacity of a real 5G link (unshaped blocks of a session,
			// scripts/trace-from-session.sh -r: each 200 ms sample, because
			// the mean of each second hides short capacity decreases). Load in
			// the two directions, large cellular buffers (the unshaped blocks
			// showed up to 1.1 s), and the upload delays of the same link
			// (as 5g-coupled).
			name: "5g-recorded-a", duration: dur, seed: 7 + v,
			dlCapacity: recADl, ulCapacity: recAUl,
			dlDemand: between(2, 118), ulDemand: between(30, 90),
			bufferMs: 1000, baseRTT: reflectorBase(25), jitterMs: 3,
			ulCouplingMs: 10, ulCouplingJitterMs: 12, ulGrantMs: 8, ulGrantJitterMs: 6,
			link: link5GRec,
		},
		{
			// The same, with a trace of a different session.
			name: "5g-recorded-b", duration: dur, seed: 8 + v,
			dlCapacity: recBDl, ulCapacity: recBUl,
			dlDemand: between(2, 118), ulDemand: between(30, 90),
			bufferMs: 1000, baseRTT: reflectorBase(25), jitterMs: 3,
			ulCouplingMs: 10, ulCouplingJitterMs: 12, ulGrantMs: 8, ulGrantJitterMs: 6,
			link: link5GRec,
		},
		{
			// Similar to the 5G link in the recordings: download activity
			// and upload activity delay the upload (the 5G scheduler). Load
			// in the two directions. The upload shaper cannot remove these
			// delays.
			name: "5g-coupled", duration: dur, seed: 9 + v,
			dlCapacity: constant(150000), ulCapacity: constant(30000),
			dlDemand: between(2, 118), ulDemand: between(30, 100),
			bufferMs: 800, baseRTT: reflectorBase(25), jitterMs: 3, ulCouplingMs: 10, ulCouplingJitterMs: 12, ulGrantMs: 8, ulGrantJitterMs: 6,
			link: link5G,
		},
		{
			// A 5G link with short capacity stalls and a stable capacity
			// between them (the link of 2026-10-18: calibrated rates, a
			// large buffer). Each stall decreases the achieved rate, and
			// the decreases from the achieved rate can set the rate to the
			// minimum.
			name: "5g-stalls", duration: dur, seed: 10 + v,
			dlCapacity: radioStalls(20+v, dur, randomWalk(21+v, dur, 200, 120000, 230000, 0.05, 0), 0.1),
			ulCapacity: randomWalk(22+v, dur, 200, 90000, 110000, 0.03, 0),
			dlDemand:   between(2, 118), ulDemand: between(30, 90),
			bufferMs: 1000, baseRTT: reflectorBase(25), jitterMs: 3,
			link: LinkConfig{
				Download: dirConfig(34000, 130000, 245000),
				Upload:   dirConfig(19000, 94500, 110000),
			},
		},
		{
			// A 300ms latency spike that queues do not cause (finding F1: each
			// ping during the spike cuts the rate again).
			name: "latency-spike", duration: dur, seed: 6 + v,
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

// TestSimScorecard runs each scenario with each policy and shows a
// scorecard. To run it, use: go test -run TestSimScorecard -v
func TestSimScorecard(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation scorecard skipped in -short mode")
	}
	if *simSeeds < 1 {
		t.Fatalf("-sim.seeds must be 1 or more, got %d", *simSeeds)
	}
	var b strings.Builder
	if *simSeeds > 1 {
		fmt.Fprintf(&b, "\nmean of %d seed variants for each scenario", *simSeeds)
	}
	fmt.Fprintf(&b, "\n%-16s %-12s %8s %8s %8s %8s %7s %7s %7s\n",
		"scenario", "policy", "p50 ms", "p95 ms", "p99 ms", "bloat s", "dl %", "ul %", "sets")
	for i := range simScenarios() {
		for _, p := range []simPolicy{policyUnshaped, policyFixed, policyAutorate, policyAutorateOWD} {
			r, sets := runSimSeeds(i, p, *simSeeds)
			fmt.Fprintf(&b, "%-16s %-12s %8.1f %8.1f %8.1f %8.1f %7.1f %7.1f %7.1f\n",
				r.scenario, r.policy, r.latP50, r.latP95, r.latP99, r.bloatS,
				100*r.dlUtil, 100*r.ulUtil, sets)
		}
	}
	t.Log(b.String())
	if path := os.Getenv("SIM_SCORECARD_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runSimSeeds runs scenario i of simScenariosVariant with n seed variants
// (n >= 1). It returns the mean of the scores and the mean number of rate
// changes (rateChanges of the result is not used, because it is an int).
func runSimSeeds(i int, p simPolicy, n int) (simResult, float64) {
	var m simResult
	var sets float64
	for v := range n {
		r := runSim(simScenariosVariant(uint64(v))[i], p)
		m.scenario, m.policy = r.scenario, r.policy
		m.latP50 += r.latP50
		m.latP95 += r.latP95
		m.latP99 += r.latP99
		m.bloatS += r.bloatS
		m.dlUtil += r.dlUtil
		m.ulUtil += r.ulUtil
		sets += float64(r.rateChanges)
	}
	k := float64(n)
	m.latP50 /= k
	m.latP95 /= k
	m.latP99 /= k
	m.bloatS /= k
	m.dlUtil /= k
	m.ulUtil /= k
	return m, sets / k
}

// Variant 0 gives the default seeds, and other variants give other traces.
func TestSimScenariosVariant(t *testing.T) {
	a, b := simScenariosVariant(0), simScenarios()
	for i := range a {
		if a[i].seed != b[i].seed || a[i].dlCapacity(50000) != b[i].dlCapacity(50000) {
			t.Errorf("%s: variant 0 is not the default scenario", a[i].name)
		}
	}
	c := simScenariosVariant(1)
	for i := range a {
		if a[i].seed == c[i].seed {
			t.Errorf("%s: variant 1 has the same seed as variant 0", a[i].name)
		}
	}
	if runSim(c[3], policyAutorate).latP95 == runSim(a[3], policyAutorate).latP95 {
		t.Errorf("%s: variant 1 gives the same result as variant 0", a[3].name)
	}
}

func TestRecordedTrace(t *testing.T) {
	path := t.TempDir() + "/trace.csv"
	if err := os.WriteFile(path, []byte("t_ms,dl_kbps,ul_kbps\n0,100,10\n200,200,20\n400,300,30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One data line: a constant capacity.
	one := t.TempDir() + "/one.csv"
	if err := os.WriteFile(one, []byte("t_ms,dl_kbps,ul_kbps\n0,100,10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, u := recordedTrace(one); d(0) != 100 || d(5000) != 100 || u(5000) != 10 {
		t.Errorf("one-line trace: %v/%v, want 100/10", d(5000), u(5000))
	}

	dl, ul := recordedTrace(path)
	for _, c := range []struct{ t, dl, ul float64 }{
		{0, 100, 10}, {199, 100, 10}, {200, 200, 20}, {599, 300, 30},
		{600, 100, 10}, // the trace starts again after 600 ms
		{1000, 300, 30},
	} {
		if dl(c.t) != c.dl || ul(c.t) != c.ul {
			t.Errorf("t=%v: %v/%v, want %v/%v", c.t, dl(c.t), ul(c.t), c.dl, c.ul)
		}
	}
}

// --- Basic checks of the simulator ---

func TestSim_Deterministic(t *testing.T) {
	sc := scenarioByName(t, "5g-variable")
	sc.duration = 30 * time.Second
	a, b := runSim(sc, policyAutorate), runSim(sc, policyAutorate)
	if a.latP99 != b.latP99 || a.dlUtil != b.dlUtil || a.rateChanges != b.rateChanges {
		t.Errorf("runs diverged: %+v vs %+v", a, b)
	}
}

func TestSim_UnshapedBloats(t *testing.T) {
	// Without a shaper, a bulk sender fills the bottleneck buffer.
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
	// A fixed shaper rate below the capacity keeps the bottleneck queue empty.
	sc := scenarioByName(t, "steady-dl")
	sc.duration = 30 * time.Second
	sc.dlDemand = between(2, 30)
	r := runSim(sc, policyFixed) // base 100M < capacity 120M
	// p99 includes the slow-start overshoot at the start of the flows,
	// before CoDel in CAKE reacts. p95 shows the steady state.
	if r.latP95 > 5 {
		t.Errorf("expected low latency with shaper below capacity, got p95=%.1fms", r.latP95)
	}
	if r.dlUtil < 0.75 || r.dlUtil > 0.85 {
		t.Errorf("expected utilization ~100/120, got %.2f", r.dlUtil)
	}
}

// --- Known issues ---
//
// Each test examines the behavior that we want. While the issue occurs, the
// test logs the issue and passes. When a fix makes the check true, the test
// fails. Then, change the test into a usual regression test. The IDs refer
// to GitHub issues (knownIssueURL). The tests use the default config. Each
// issue tells its status and the options that fix it, if there are any.

// knownIssueURL links each known issue to its GitHub issue: the problem, the
// scenario, the status and the options that fix it, if there are any.
var knownIssueURL = map[string]string{
	"F1": "https://github.com/megglos/cake-autorate-go/issues/30",
	"F2": "https://github.com/megglos/cake-autorate-go/issues/31",
	"F4": "https://github.com/megglos/cake-autorate-go/issues/32",
	"F5": "https://github.com/megglos/cake-autorate-go/issues/33",
}

func knownIssue(t *testing.T, id string, ok bool, format string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(format, args...)
	const defaults = "default config: ping_method icmp, refractory_on_decrease off"
	if ok {
		t.Errorf("known issue %s appears fixed (%s, %s): %s: turn this into a regular assertion", id, knownIssueURL[id], defaults, detail)
		return
	}
	t.Logf("known issue %s still present (%s, %s): %s", id, knownIssueURL[id], defaults, detail)
}

// sliceS returns the samples (100ms resolution) between fromS and toS.
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

// F1: a short latency spike that queues do not cause must cut the rate only
// a little. Each ping cuts the rate again while the detection window is full.
// Thus, a 300ms spike causes many cuts.
func TestKnownIssue1_LatencySpikeOvercut(t *testing.T) {
	r := runSim(scenarioByName(t, "latency-spike"), policyAutorate)
	low := minOf(sliceS(r.dlShaper, 60, 63))
	const capacity = 120000.0
	knownIssue(t, "F1", low >= 0.75*capacity,
		"300ms spike cut DL shaper to %.0f kbps (%.0f%% of %.0f kbps capacity)", low, 100*low/capacity, capacity)
}

// With refractory_on_decrease, the same spike cuts the rate only a little.
func TestSimRefractoryOnDecrease_LatencySpike(t *testing.T) {
	sc := scenarioByName(t, "latency-spike")
	sc.tune = func(cfg *Config) { cfg.RefractoryOnDecrease = true }
	r := runSim(sc, policyAutorate)
	low := minOf(sliceS(r.dlShaper, 60, 63))
	const capacity = 120000.0
	if low < 0.75*capacity {
		t.Errorf("300ms spike cut DL shaper to %.0f kbps (%.0f%% of capacity), want 75%% or more", low, 100*low/capacity)
	}
}

// With stall_recovery_ms (the default), the short stalls of 5g-stalls do
// not keep the download rate low: more throughput for a small increase of
// the delay.
func TestSimOWD_StallRecovery(t *testing.T) {
	const seeds = 4
	var dlOff, dlOn, p95Off, p95On float64
	for v := range seeds {
		var sc simScenario
		for _, s := range simScenariosVariant(uint64(v)) {
			if s.name == "5g-stalls" {
				sc = s
			}
		}
		sc.tune = func(cfg *Config) { cfg.StallRecoveryMs = 0 }
		off := runSim(sc, policyAutorateOWD)
		sc.tune = func(cfg *Config) { cfg.StallRecoveryMs = 500 }
		on := runSim(sc, policyAutorateOWD)
		dlOff, p95Off = dlOff+off.dlUtil/seeds, p95Off+off.latP95/seeds
		dlOn, p95On = dlOn+on.dlUtil/seeds, p95On+on.latP95/seeds
	}
	t.Logf("5g-stalls (%d seeds): DL %.1f%% -> %.1f%%, p95 %.1f -> %.1f ms", seeds, 100*dlOff, 100*dlOn, p95Off, p95On)
	if dlOn < dlOff+0.05 {
		t.Errorf("DL utilization %.1f%% with stall recovery, want 5 points more than %.1f%%", 100*dlOn, 100*dlOff)
	}
	if p95On > p95Off+10 {
		t.Errorf("p95 %.1f ms with stall recovery, want not more than 10 ms above %.1f ms", p95On, p95Off)
	}
}

// With one-way delays, the cut starts from the achieved rate (F2).
func TestSimOWD_CutFromAchievedRate(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.lastPingOWD = true
	c.dl.shaperRateKbps, c.dl.achievedKbps = 40000, 20000
	c.adjustRateBufferbloat(DL, 31000, 30000)
	if c.dl.shaperRateKbps > 20000 {
		t.Errorf("rate %.0f after a cut with achieved 20000, want 20000 or less", c.dl.shaperRateKbps)
	}

	// Without one-way delays, the cut starts from the shaper rate.
	c.lastPingOWD = false
	c.dl.shaperRateKbps = 40000
	c.adjustRateBufferbloat(DL, 31000, 30000)
	if c.dl.shaperRateKbps < 39000 {
		t.Errorf("rate %.0f after a small cut without one-way delays, want approximately 40000", c.dl.shaperRateKbps)
	}
}

// A link outage of 20 s (with 2 % ping loss before and after it): Core must
// find the stall (global_ping_response_timeout_s is 10 s), and after the
// outage it must go back to normal operation and get the capacity again.
func TestSim_OutageAndPingLoss(t *testing.T) {
	sc := scenarioByName(t, "steady-dl")
	sc.name = "outage"
	sc.pingLoss = 0.02
	sc.outage = between(40, 60)
	for _, p := range []simPolicy{policyAutorate, policyAutorateOWD} {
		r := runSim(sc, p)
		// Core waits for global_ping_response_timeout_s (10 s) before the
		// stall. Thus, the stall is approximately the last 10 s of the outage.
		if r.stallS < 8 || r.stallS > 12 {
			t.Errorf("%s: %.1f s in stall during a 20 s outage, want approximately 10 s", p, r.stallS)
		}
		if r.stallEnd < 60000 || r.stallEnd > 65000 {
			t.Errorf("%s: end of the stall at %.1f s, want in the 5 s after the outage (60 s)", p, r.stallEnd/1000)
		}
		const capacity = 120000.0
		if after := mean(sliceS(r.dlShaper, 80, 118)); after < 0.75*capacity {
			t.Errorf("%s: mean DL shaper %.0f kbps after the outage, want 75%% of the capacity or more", p, after)
		}
	}

	// Ping loss alone (5 %) must not cause a stall or a large loss of
	// throughput.
	sc = scenarioByName(t, "steady-dl")
	sc.pingLoss = 0.05
	for _, p := range []simPolicy{policyAutorate, policyAutorateOWD} {
		base := runSim(scenarioByName(t, "steady-dl"), p)
		r := runSim(sc, p)
		if r.stallS > 0 {
			t.Errorf("%s: %.1f s in stall with 5 %% ping loss, want none", p, r.stallS)
		}
		if r.dlUtil < base.dlUtil-0.03 {
			t.Errorf("%s: DL utilization %.1f %% with 5 %% ping loss, %.1f %% without", p, 100*r.dlUtil, 100*base.dlUtil)
		}
	}
}

// With one-way delays, the upload delay from download activity is near the
// threshold for rate increases, and the upload rate stays low. With
// learn_cross_delay, the upload gets its capacity. The latency must not
// increase much: more upload throughput gives a small upload queue.
func TestSimOWD_LearnCrossDelay_Coupled(t *testing.T) {
	sc := scenarioByName(t, "5g-coupled")
	run := func(learn bool) simResult {
		sc.tune = func(cfg *Config) { cfg.LearnCrossDelay = learn }
		return runSim(sc, policyAutorateOWD)
	}
	off, on := run(false), run(true)
	t.Logf("p95 %.1f / %.1f ms, bloat %.1f / %.1f s, DL %.0f / %.0f %%, UL %.0f / %.0f %% (off / on)",
		off.latP95, on.latP95, off.bloatS, on.bloatS, 100*off.dlUtil, 100*on.dlUtil, 100*off.ulUtil, 100*on.ulUtil)
	if on.ulUtil < 0.8 || on.ulUtil < off.ulUtil+0.5 {
		t.Errorf("UL utilization %.0f%% with learn_cross_delay, %.0f%% without, want 80%% or more and +50 points", 100*on.ulUtil, 100*off.ulUtil)
	}
	if on.dlUtil < 0.9 {
		t.Errorf("DL utilization %.0f%% with learn_cross_delay, want 90%% or more", 100*on.dlUtil)
	}
	if on.latP95 > off.latP95+15 {
		t.Errorf("p95 %.1f ms with learn_cross_delay, %.1f ms without, want not more than +15 ms", on.latP95, off.latP95)
	}
}

// The upload delay from upload activity (5G grant delay) is a lower limit
// for the upload delay threshold: learn_cross_delay cannot remove it,
// because it occurs exactly when the upload is loaded. On the 5G link, an
// upload threshold of 15 ms gave 30 to 43 % of the upload capacity, and
// 25 ms gave 66 to 68 %. The simulator must show the same limit.
func TestSimOWD_UploadThresholdAboveGrantDelay(t *testing.T) {
	sc := scenarioByName(t, "5g-coupled")
	run := func(thrMs float64) simResult {
		sc.tune = func(cfg *Config) {
			cfg.LearnCrossDelay = true
			cfg.Links[0].Upload.OWDDeltaDelayThrMs = thrMs
			cfg.Links[0].Upload.AvgOWDDeltaMaxAdjDownThrMs = 2 * thrMs
		}
		return runSim(sc, policyAutorateOWD)
	}
	low, high := run(15), run(25)
	t.Logf("UL utilization: threshold 15 ms %.0f %%, 25 ms %.0f %%", 100*low.ulUtil, 100*high.ulUtil)
	if low.ulUtil > 0.3 {
		t.Errorf("UL utilization %.0f%% with a 15 ms threshold, want 30%% or less (the grant delay is above the threshold)", 100*low.ulUtil)
	}
	if high.ulUtil < 0.7 {
		t.Errorf("UL utilization %.0f%% with a 25 ms threshold, want 70%% or more", 100*high.ulUtil)
	}
	if high.dlUtil < 0.9 {
		t.Errorf("DL utilization %.0f%% with a 25 ms threshold, want 90%% or more", 100*high.dlUtil)
	}
}

// F1/F2: after a large capacity decrease, the shaper rate must quickly go
// near the new capacity. It must not go far below the new capacity.
func TestKnownIssue2_CapacityDropConvergence(t *testing.T) {
	r := runSim(scenarioByName(t, "capacity-drop"), policyAutorate)
	const capacity = 40000.0
	low := minOf(sliceS(r.dlShaper, 40, 50))
	after1s := r.dlShaper[41*10]
	ok := low >= 0.5*capacity && after1s <= 1.25*capacity
	knownIssue(t, "F2", ok,
		"after 200→40 Mbit/s drop: shaper %.0f kbps 1s later, minimum %.0f kbps in the next 10s", after1s, low)
}

// F4: a saturated upload must not decrease the download rate. With RTT/2,
// the UL queue delay applies to the two directions.
func TestKnownIssue4_UploadBloatThrottlesDownload(t *testing.T) {
	r := runSim(scenarioByName(t, "bidir-saturated"), policyAutorate)
	knownIssue(t, "F4", r.dlUtil >= 0.75,
		"DL utilization %.0f%% while UL is saturated", 100*r.dlUtil)
}

// With one-way delays, a saturated upload must not decrease the download
// rate (fix for F4).
func TestSimOWD_UploadBloatDoesNotThrottleDownload(t *testing.T) {
	r := runSim(scenarioByName(t, "bidir-saturated"), policyAutorateOWD)
	if r.dlUtil < 0.75 {
		t.Errorf("DL utilization %.0f%% while UL is saturated, want 75%% or more", 100*r.dlUtil)
	}
	if r.ulUtil < 0.75 {
		t.Errorf("UL utilization %.0f%%, want 75%% or more", 100*r.ulUtil)
	}
}

// F5: a step in the RTT floor (Starlink reconfiguration, route change) is not
// a queue. After Core learns the new floor, the step must not decrease the
// throughput.
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
	knownIssue(t, "F5", after >= 0.9*before,
		"mean DL shaper %.0f kbps before a +40ms RTT floor step, %.0f kbps after", before, after)
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// learn_cross_delay must not learn the queue of the upload shaper. After an
// upload capacity decrease, the upload load is less than half of the shaper
// rate while the queue increases. Core first learns the real cross delay
// (no upload load until 20 s), then the upload capacity decreases at 40 s.
// The learned delay must not increase after the decrease.
func TestSimOWD_LearnCrossDelay_UploadCapacityDrop(t *testing.T) {
	sc := scenarioByName(t, "5g-coupled")
	sc.name = "5g-coupled-ul-drop"
	sc.ulCapacity = steps([]float64{0, 40, 80}, []float64{30000, 3000, 30000})
	sc.ulDemand = between(20, 118)
	sc.tune = func(cfg *Config) { cfg.LearnCrossDelay = true }
	r := runSim(sc, policyAutorateOWD)
	before := r.ulCrossDelay[39*10]
	after := 0.0
	for _, v := range sliceS(r.ulCrossDelay, 40, 80) {
		after = math.Max(after, v)
	}
	t.Logf("learned cross delay: %.1f ms before the decrease, maximum %.1f ms after it", before/1000, after/1000)
	if before < 10000 {
		t.Errorf("learned cross delay %.1f ms before the decrease, want 10 ms or more", before/1000)
	}
	if after > before+2000 {
		t.Errorf("learned cross delay increased from %.1f to %.1f ms after the upload capacity decrease", before/1000, after/1000)
	}
}

// The simulator must give results near the results of the 5G link for the
// same capacity trace. 5g-recorded-b uses the unshaped blocks 10 (dl) and
// 13 (ul) of the 5G session of 2026-10-11 12:45. In the autorate blocks of
// this session (one-way delays, refractory_on_decrease, 3 rounds), the
// download got 22 to 50 % of the unshaped rate with load on the download
// only, and the upload got 56 to 63 % with load on the upload only.
// Before the calibration (trace with the mean of each second, no upload
// delays), the simulator gave 87 % and 95 %.
func TestSimCalibration_5GRecorded(t *testing.T) {
	run := func(dl, ul bool) simResult {
		sc := scenarioByName(t, "5g-recorded-b")
		sc.dlDemand, sc.ulDemand = never, never
		if dl {
			sc.dlDemand = between(2, 118)
		}
		if ul {
			sc.ulDemand = between(2, 118)
		}
		sc.tune = func(cfg *Config) { cfg.RefractoryOnDecrease = true }
		return runSim(sc, policyAutorateOWD)
	}
	dlOnly, ulOnly := run(true, false), run(false, true)
	t.Logf("download only: DL %.0f %%, p95 %.1f ms; upload only: UL %.0f %%, p95 %.1f ms",
		100*dlOnly.dlUtil, dlOnly.latP95, 100*ulOnly.ulUtil, ulOnly.latP95)
	if dlOnly.dlUtil < 0.2 || dlOnly.dlUtil > 0.6 {
		t.Errorf("download only: DL utilization %.0f%%, want 20 to 60%% (link: 22 to 50%%)", 100*dlOnly.dlUtil)
	}
	if ulOnly.ulUtil < 0.45 || ulOnly.ulUtil > 0.75 {
		t.Errorf("upload only: UL utilization %.0f%%, want 45 to 75%% (link: 56 to 63%%)", 100*ulOnly.ulUtil)
	}
}

// TestSimOWD_IncreaseToDelayThr_DownloadDelay: on the 5G link of
// 2026-10-21, the download delay under load was 14 to 17 ms, also without a
// queue. With the default scale, the increase is zero at a delay of
// avg_owd_delta_max_adjust_up_thr_ms (10 ms), and the rate increases only
// slowly. increase_to_delay_thr increases up to the delay threshold. The
// scenario is 5g-stalls with a download delay of 12 ms (mean) under load.
func TestSimOWD_IncreaseToDelayThr_DownloadDelay(t *testing.T) {
	run := func(on bool) simResult {
		sc := scenarioByName(t, "5g-stalls")
		sc.dlGrantMs = 12
		sc.tune = func(cfg *Config) { cfg.IncreaseToDelayThr = on }
		return runSim(sc, policyAutorateOWD)
	}
	off, on := run(false), run(true)
	t.Logf("default: DL %.0f %%, p95 %.1f ms; increase_to_delay_thr: DL %.0f %%, p95 %.1f ms",
		100*off.dlUtil, off.latP95, 100*on.dlUtil, on.latP95)
	if on.dlUtil < off.dlUtil+0.05 {
		t.Errorf("DL utilization %.0f%%, want at least 5 points more than the default (%.0f%%)", 100*on.dlUtil, 100*off.dlUtil)
	}
}
