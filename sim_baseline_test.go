// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The baseline file contains the autorate scores for each scenario.
// TestSimBaseline fails when a score is worse than the baseline by more than
// the tolerance. To accept new scores, run:
//
//	go test -run TestSimBaseline -update
//
// Then, add the changed baseline file to the pull request.

const simBaselinePath = "testdata/sim_baseline.json"

var updateBaseline = flag.Bool("update", false, "write the current simulator scores to "+simBaselinePath)

// simScores are the scores that the baseline keeps for one scenario.
type simScores struct {
	LatP95Ms float64 `json:"lat_p95_ms"`
	LatP99Ms float64 `json:"lat_p99_ms"`
	BloatS   float64 `json:"bloat_s"`
	DlUtil   float64 `json:"dl_util"`
	UlUtil   float64 `json:"ul_util"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func scoresOf(r simResult) simScores {
	return simScores{
		LatP95Ms: round2(r.latP95),
		LatP99Ms: round2(r.latP99),
		BloatS:   round2(r.bloatS),
		DlUtil:   round2(r.dlUtil * 100),
		UlUtil:   round2(r.ulUtil * 100),
	}
}

// simMetric is one score with its tolerance.
type simMetric struct {
	name           string
	get            func(simScores) float64
	higherIsBetter bool
	// tolerance returns the permitted change in the bad direction.
	tolerance func(base float64) float64
}

var simMetrics = []simMetric{
	{name: "p95 ms", get: func(s simScores) float64 { return s.LatP95Ms }, tolerance: latencyTolerance},
	{name: "p99 ms", get: func(s simScores) float64 { return s.LatP99Ms }, tolerance: latencyTolerance},
	{name: "bloat s", get: func(s simScores) float64 { return s.BloatS },
		tolerance: func(b float64) float64 { return math.Max(0.1*b, 1) }},
	{name: "dl %", get: func(s simScores) float64 { return s.DlUtil }, higherIsBetter: true,
		tolerance: func(float64) float64 { return 3 }},
	{name: "ul %", get: func(s simScores) float64 { return s.UlUtil }, higherIsBetter: true,
		tolerance: func(float64) float64 { return 3 }},
}

// latencyTolerance permits 10% or 5ms, whichever is larger.
func latencyTolerance(base float64) float64 { return math.Max(0.1*base, 5) }

// compare returns +1 if got is worse than base by more than the tolerance,
// -1 if got is better by more than the tolerance, and 0 if not.
func (m simMetric) compare(base, got float64) int {
	diff := got - base
	if m.higherIsBetter {
		diff = -diff
	}
	switch tol := m.tolerance(base); {
	case diff > tol:
		return 1
	case diff < -tol:
		return -1
	}
	return 0
}

func TestSimBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("simulator baseline skipped in -short mode")
	}

	current := make(map[string]simScores)
	var names []string
	for _, sc := range simScenarios() {
		current[sc.name] = scoresOf(runSim(sc, policyAutorate))
		names = append(names, sc.name)
	}

	if *updateBaseline {
		data, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(simBaselinePath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", simBaselinePath)
		return
	}

	data, err := os.ReadFile(simBaselinePath)
	if err != nil {
		t.Fatalf("reading baseline (to create it, run: go test -run TestSimBaseline -update): %v", err)
	}
	baseline := make(map[string]simScores)
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatalf("parsing %s: %v", simBaselinePath, err)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "| scenario | metric | baseline | this change | result |\n|---|---|---|---|---|\n")
	improved := false
	for _, name := range names {
		base, ok := baseline[name]
		if !ok {
			t.Errorf("scenario %s is not in the baseline: run go test -run TestSimBaseline -update", name)
			continue
		}
		got := current[name]
		for _, m := range simMetrics {
			b, g := m.get(base), m.get(got)
			result := "ok"
			switch m.compare(b, g) {
			case 1:
				result = "**worse**"
				t.Errorf("%s %s: %.2f, baseline %.2f (tolerance %.2f)", name, m.name, g, b, m.tolerance(b))
			case -1:
				result = "better"
				improved = true
			}
			fmt.Fprintf(&report, "| %s | %s | %.2f | %.2f | %s |\n", name, m.name, b, g, result)
		}
	}
	for name := range baseline {
		if _, ok := current[name]; !ok {
			t.Errorf("scenario %s is in the baseline but does not exist: run go test -run TestSimBaseline -update", name)
		}
	}
	if improved && !t.Failed() {
		t.Log("some scores are better than the baseline: run go test -run TestSimBaseline -update and add the baseline to the change")
	}

	if path := os.Getenv("SIM_BASELINE_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSimMetricCompare(t *testing.T) {
	lat, util := simMetrics[0], simMetrics[3]
	tests := []struct {
		m         simMetric
		base, got float64
		want      int
	}{
		{lat, 60, 65, 0},    // +5ms is in the tolerance (max(6, 5) = 6)
		{lat, 60, 67, 1},    // +7ms is worse
		{lat, 60, 53, -1},   // -7ms is better
		{lat, 2, 6.5, 0},    // small values: 5ms tolerance
		{util, 80, 77.5, 0}, // -2.5 points is in the tolerance
		{util, 80, 76, 1},   // -4 points is worse
		{util, 80, 84, -1},  // +4 points is better
	}
	for _, tc := range tests {
		if got := tc.m.compare(tc.base, tc.got); got != tc.want {
			t.Errorf("%s compare(%v, %v) = %d, want %d", tc.m.name, tc.base, tc.got, got, tc.want)
		}
	}
}
