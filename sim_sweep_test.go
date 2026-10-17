// SPDX-License-Identifier: GPL-2.0

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The sweep of the settings that change the trade-off between delay and
// throughput (issue #96). It is slow: set SIM_SWEEP=1.
//
//	SIM_SWEEP=1 go test -run TestSimSweepModes -v -timeout 60m .
//
// SIM_SWEEP=candidates runs only the candidates of the modes.
//
// Each knob changes the link config or the controller config of all
// scenarios. The table shows the delay p95 and the utilization of each
// direction (mean of simSweepSeeds seeds), and the change against the
// config of the scenario.

const simSweepSeeds = 3

// simKnob is one change of the settings.
type simKnob struct {
	name string
	link func(l *LinkConfig)
	cfg  func(c *Config)
}

func scaleThr(f float64) func(l *LinkConfig) {
	return func(l *LinkConfig) {
		for _, d := range []*DirectionConfig{&l.Download, &l.Upload} {
			d.OWDDeltaDelayThrMs *= f
			d.AvgOWDDeltaMaxAdjUpThrMs *= f
			d.AvgOWDDeltaMaxAdjDownThrMs *= f
		}
	}
}

// baseTo sets the base rate to the given part of the way from the base
// rate to the maximum rate (0: base, 1: maximum), or below the base rate
// for a negative value (a part of the way to the minimum rate).
func baseTo(f float64) func(l *LinkConfig) {
	return func(l *LinkConfig) {
		for _, d := range []*DirectionConfig{&l.Download, &l.Upload} {
			if f >= 0 {
				d.BaseRateKbps += int(f * float64(d.MaxRateKbps-d.BaseRateKbps))
			} else {
				d.BaseRateKbps += int(f * float64(d.BaseRateKbps-d.MinRateKbps))
			}
		}
	}
}

func maxRate(f float64) func(l *LinkConfig) {
	return func(l *LinkConfig) {
		for _, d := range []*DirectionConfig{&l.Download, &l.Upload} {
			d.MaxRateKbps = int(f * float64(d.MaxRateKbps))
		}
	}
}

// simCandidateKnobs are the candidates of the modes (SIM_SWEEP=candidates).
func simCandidateKnobs() []simKnob {
	detect2 := func(c *Config) { c.BufferbloatDetectionThr = 2 }
	repeat := func(c *Config) { c.RepeatDecreaseOnRise = true }
	return []simKnob{
		{name: "config"},
		{name: "lat: thr x0.5", link: scaleThr(0.5)},
		{name: "lat: thr x0.6", link: scaleThr(0.6)},
		{name: "lat: thr x0.7", link: scaleThr(0.7)},
		{name: "lat: thr x0.6 detect 2/6", link: scaleThr(0.6), cfg: detect2},
		{name: "lat: thr x0.7 detect 2/6", link: scaleThr(0.7), cfg: detect2},
		{name: "bal: detect 2/6", cfg: detect2},
		{name: "bal: repeat on rise", cfg: repeat},
		{name: "bw: thr x1.5 repeat", link: scaleThr(1.5), cfg: repeat},
		{name: "bw: thr x2 repeat", link: scaleThr(2), cfg: repeat},
		{name: "bw: thr x2.5 repeat", link: scaleThr(2.5), cfg: repeat},
		{name: "bw: thr x2", link: scaleThr(2)},
	}
}

func simSweepKnobs() []simKnob {
	if os.Getenv("SIM_SWEEP") == "candidates" {
		return simCandidateKnobs()
	}
	return []simKnob{
		{name: "config"},
		{name: "thr x0.5", link: scaleThr(0.5)},
		{name: "thr x0.7", link: scaleThr(0.7)},
		{name: "thr x1.5", link: scaleThr(1.5)},
		{name: "thr x2.5", link: scaleThr(2.5)},
		{name: "base -0.3", link: baseTo(-0.3)},
		{name: "base +0.5", link: baseTo(0.5)},
		{name: "base max", link: baseTo(1)},
		{name: "max x1.1", link: maxRate(1.1)},
		{name: "repeat on rise", cfg: func(c *Config) { c.RepeatDecreaseOnRise = true }},
		{name: "cut 0.9", cfg: func(c *Config) { c.ShaperRateMaxAdjustDownBufferbloat = 0.9 }},
		{name: "cut 0.6", cfg: func(c *Config) { c.ShaperRateMaxAdjustDownBufferbloat = 0.6 }},
		{name: "ramp 1.08", cfg: func(c *Config) { c.ShaperRateMaxAdjustUpLoadHigh = 1.08 }},
		{name: "ramp 1.02", cfg: func(c *Config) { c.ShaperRateMaxAdjustUpLoadHigh = 1.02 }},
		{name: "detect 2/6", cfg: func(c *Config) { c.BufferbloatDetectionThr = 2 }},
		{name: "detect 4/6", cfg: func(c *Config) { c.BufferbloatDetectionThr = 4 }},
		{name: "bw: thr x2.5 base max rep", link: func(l *LinkConfig) { scaleThr(2.5)(l); baseTo(1)(l) },
			cfg: func(c *Config) { c.RepeatDecreaseOnRise = true }},
		{name: "lat: thr x0.7 base -0.3", link: func(l *LinkConfig) { scaleThr(0.7)(l); baseTo(-0.3)(l) }},
	}
}

// simFiber is a fiber line (cav1 VM, 2026-10-17): a stable capacity of
// 650/330 Mbit/s, an idle RTT of 3 ms, and a buffer in the network of the
// provider of approximately 15 ms (unshaped: +14/+12 ms). The link has the
// calibrated config (base 95 % of the capacity, thresholds 10/11 ms).
func simFiber(v uint64) simScenario {
	fiberDir := func(minK, baseK, maxK int, thr float64) DirectionConfig {
		d := dirConfig(minK, baseK, maxK)
		d.OWDDeltaDelayThrMs = thr
		return d
	}
	return simScenario{
		name: "fiber", duration: 120 * time.Second, seed: 20 + v,
		dlCapacity: constant(650000), ulCapacity: constant(330000),
		dlDemand: between(2, 118), ulDemand: between(30, 90),
		bufferMs: 15, baseRTT: reflectorBase(3), jitterMs: 0.5,
		link: LinkConfig{
			Download: fiberDir(325000, 618000, 650000, 10),
			Upload:   fiberDir(165000, 313500, 330000, 11),
		},
	}
}

func TestSimSweepModes(t *testing.T) {
	if os.Getenv("SIM_SWEEP") == "" {
		t.Skip("set SIM_SWEEP=1")
	}
	names := []string{"fiber", "steady-dl", "bidir-saturated", "5g-variable", "5g-coupled", "5g-recorded-a", "starlink-like"}
	scenario := func(name string, v uint64) simScenario {
		if name == "fiber" {
			return simFiber(v)
		}
		for _, sc := range simScenariosVariant(v) {
			if sc.name == name {
				return sc
			}
		}
		t.Fatalf("no scenario %q", name)
		return simScenario{}
	}
	var out strings.Builder
	for _, name := range names {
		fmt.Fprintf(&out, "\n%s\n%-28s %8s %8s %8s   %8s %8s %8s\n", name, "knob", "p95 ms", "dl %", "ul %", "d p95", "d dl", "d ul")
		var ref simResult
		for i, k := range simSweepKnobs() {
			var m simResult
			for v := range uint64(simSweepSeeds) {
				sc := scenario(name, v)
				sc.link.Reflectors = append([]string(nil), sc.link.Reflectors...)
				if k.link != nil {
					k.link(&sc.link)
				}
				if k.cfg != nil {
					prev := sc.tune
					sc.tune = func(c *Config) {
						if prev != nil {
							prev(c)
						}
						k.cfg(c)
					}
				}
				r := runSim(sc, policyAutorateOWD)
				m.latP95 += r.latP95 / simSweepSeeds
				m.dlUtil += r.dlUtil / simSweepSeeds
				m.ulUtil += r.ulUtil / simSweepSeeds
			}
			if i == 0 {
				ref = m
			}
			fmt.Fprintf(&out, "%-28s %8.1f %8.1f %8.1f   %+8.1f %+8.1f %+8.1f\n", k.name, m.latP95, 100*m.dlUtil, 100*m.ulUtil,
				m.latP95-ref.latP95, 100*(m.dlUtil-ref.dlUtil), 100*(m.ulUtil-ref.ulUtil))
		}
	}
	t.Log(out.String())
	if p := os.Getenv("SIM_SWEEP_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
