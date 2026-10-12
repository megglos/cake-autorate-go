// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// calTestLink describes the link of a synthetic session.
type calTestLink struct {
	dlCapKbps, ulCapKbps float64 // capacity without load in the other direction
	ulCapBothKbps        float64 // upload capacity with download load
	idleJitterMs         float64 // maximum idle jitter
	crossDelayMs         float64 // upload delay from download activity
	grantDelayMs         float64 // upload delay from upload activity
	drift                bool    // the clock offset changes by 50 ms in each block
	echo                 bool    // ICMP echo: no one-way delays
	noisyReflector       bool    // reflector r5 has 5 times the idle jitter
	warmupS              int     // download warm-up in the mode both (s)
	flakyReflector       bool    // reflector r4 replies to 1 of 10 pings
	echoReflector        bool    // reflector r4 replies without one-way delays under load
	sparseReflector      bool    // reflector r4 gets 1 of 3 pings (a long ping interval)
	unshapedTimeouts     bool    // all pings time out in the load of the unshaped blocks
	loadStartDelayS      int     // load_start in blocks.tsv is this time late (warm-up program setup)
	noSessionTxt         bool    // do not write session.txt
}

// writeCalSession writes a session directory like scripts/record-session.sh:
// blocks.tsv, session.txt and one recording for each block (dl, ul and both,
// unshaped and autorate).
func writeCalSession(t *testing.T, l calTestLink) string {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Links[0].Download.Interface = "ifb4wan"
	cfg.Links[0].Upload.Interface = "wan"
	cfg.PingMethod = PingMethodTimestamp
	reflectors := []string{"r1", "r2", "r3", "r4", "r5"}
	rng := rand.New(rand.NewPCG(1, 2))

	const idleS, loadS = 15, 30
	var tsv strings.Builder
	tsv.WriteString("block\tround\tmode\tpolicy\tstart\tload_start\tload_end\tend\tcake_dl_kbps\tcake_ul_kbps\trecording\n")
	start := time.Unix(1_800_000_000, 0)
	num := 0
	for _, mode := range []string{"dl", "ul", "both"} {
		for _, policy := range []string{"unshaped", "autorate"} {
			num++
			name := fmt.Sprintf("%02d-%s-%s.jsonl", num, mode, policy)
			warm := 0
			if mode == "both" {
				warm = l.warmupS
			}
			warmStart := start.Add(idleS * time.Second)
			loadStart := warmStart.Add(time.Duration(warm) * time.Second)
			loadEnd := loadStart.Add(loadS * time.Second)
			rec, err := newRecorderAt(filepath.Join(dir, name), 0, cfg, start, testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			offset := 0.0
			if l.drift {
				offset = 50 * float64(num)
			}
			ulShaper := 0.25 * l.ulCapKbps
			if policy == "autorate" {
				rec.Shaper("wan", UL, int(ulShaper), start)
			}
			for ms := 0; ms < (idleS+warm+loadS)*1000; ms += 50 {
				ts := start.Add(time.Duration(ms) * time.Millisecond)
				load := !ts.Before(loadStart)
				dlActive := !ts.Before(warmStart) && mode != "ul"
				ulActive := load && mode != "dl"
				var dl, ul float64
				switch {
				case dlActive && ulActive:
					dl, ul = l.dlCapKbps, l.ulCapBothKbps
				case dlActive:
					dl = l.dlCapKbps
				case ulActive:
					ul = l.ulCapKbps
				}
				if ms%200 == 0 {
					rec.Rate("wan", RateStats{Timestamp: ts, DlRateKbps: dl, UlRateKbps: ul})
				}
				if ms%150 != 0 {
					continue
				}
				r := reflectors[(ms/150)%len(reflectors)]
				jit := l.idleJitterMs * rng.Float64()
				if l.noisyReflector && r == "r5" {
					jit *= 5
				}
				ulMs := 10 + offset + jit
				dlMs := 10 - offset + jit
				if dlActive {
					ulMs += l.crossDelayMs
				}
				if ulActive {
					ulMs += l.grantDelayMs
				}
				ping := PingResult{
					Reflector: r, RTT: 20 * time.Millisecond, Timestamp: ts, OWD: !l.echo,
					UlOWD: time.Duration(ulMs * float64(time.Millisecond)),
					DlOWD: time.Duration(dlMs * float64(time.Millisecond)),
				}
				if l.echo {
					ping.UlOWD, ping.DlOWD = 0, 0
				}
				if l.flakyReflector && r == "r4" && (ms/150)%50 != 3 {
					rec.Ping("wan", PingResult{Reflector: r, Timeout: true, Timestamp: ts})
					continue
				}
				if l.unshapedTimeouts && policy == "unshaped" && load {
					rec.Ping("wan", PingResult{Reflector: r, Timeout: true, Timestamp: ts})
					continue
				}
				if l.sparseReflector && r == "r4" && (ms/150/len(reflectors))%3 != 0 {
					continue
				}
				if l.echoReflector && r == "r4" && !ts.Before(warmStart) {
					ping.OWD, ping.UlOWD, ping.DlOWD = false, 0, 0
				}
				rec.Ping("wan", ping)
			}
			if err := rec.Close(); err != nil {
				t.Fatal(err)
			}
			end := loadEnd.Add(5 * time.Second)
			fmt.Fprintf(&tsv, "%d\t1\t%s\t%s\t%d\t%d\t%d\t%d\tx\tx\t%s\n", num, mode, policy,
				start.Unix(), loadStart.Unix()+int64(l.loadStartDelayS), loadEnd.Unix(), end.Unix(), name)
			start = end
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks.tsv"), []byte(tsv.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if !l.noSessionTxt {
		txt := fmt.Sprintf("block: idle %ds, load %ds\n", idleS, loadS)
		if l.warmupS > 0 {
			txt = fmt.Sprintf("block: idle %ds, load %ds, download warm-up in the mode both %ds\n", idleS, loadS, l.warmupS)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.txt"), []byte(txt), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCalibrateSession(t *testing.T) {
	dir := writeCalSession(t, calTestLink{
		dlCapKbps: 200000, ulCapKbps: 40000, ulCapBothKbps: 10000,
		idleJitterMs: 4, crossDelayMs: 20, grantDelayMs: 14, drift: true, warmupS: 10,
	})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	near := func(name string, got, want, tol float64) {
		t.Helper()
		if got < want-tol || got > want+tol {
			t.Errorf("%s = %.1f, want %.1f ± %.1f", name, got, want, tol)
		}
	}
	near("download capacity p50 (kbit/s)", res.DlCapacity.P50, 200000, 1)
	near("upload capacity p50 (kbit/s)", res.UlCapacity.P50, 40000, 1)
	near("upload capacity with download load p50 (kbit/s)", res.UlCapacityBoth.P50, 10000, 1)
	near("cross delay p50 (ms)", res.CrossDelayMs.P50, 20, 2)
	near("grant delay p50 (ms)", res.GrantDelayMs.P50, 14, 2)
	// With the drift of the clock offset, one baseline for all blocks
	// gives a jitter of hundreds of milliseconds.
	near("idle jitter (ms)", res.IdleJitterMs.P50, 3.8, 0.5)
	if len(res.Reflectors) != 5 || !res.Reflectors[0].OWD {
		t.Errorf("reflectors = %+v, want 5 with ICMP timestamps", res.Reflectors)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none", res.Notes)
	}

	p := res.propose(cfg)
	if p.DlMax != 200000 || p.DlBase != 200000 || p.DlMin != 100000 {
		t.Errorf("download rates %d/%d/%d, want 100000/200000/200000", p.DlMin, p.DlBase, p.DlMax)
	}
	// Upload: min and base from the capacity with download load.
	if p.UlMax != 40000 || p.UlBase != 10000 || p.UlMin != 5000 {
		t.Errorf("upload rates %d/%d/%d, want 5000/10000/40000", p.UlMin, p.UlBase, p.UlMax)
	}
	if p.DlThrMs != 30 || p.UlThrMs != 30 {
		t.Errorf("thresholds %.0f/%.0f ms, want 30/30", p.DlThrMs, p.UlThrMs)
	}
	if !p.LearnCrossDelay {
		t.Error("learn_cross_delay = false, want true (cross delay 20 ms)")
	}
	if p.PingMethod != PingMethodTimestamp {
		t.Errorf("ping_method = %s, want %s", p.PingMethod, PingMethodTimestamp)
	}

	// The output must be a valid config without unknown keys.
	var buf bytes.Buffer
	if err := writeCalConfig(&buf, dir, cfg, res, p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("generated config: %v\n%s", err, buf.String())
	}
	if len(got.unknownKeys) != 0 {
		t.Errorf("generated config has unknown keys %v", got.unknownKeys)
	}
	if !got.LearnCrossDelay || got.Links[0].Upload.MinRateKbps != 5000 || len(got.Links[0].Reflectors) != 5 {
		t.Errorf("generated config does not contain the proposal:\n%s", buf.String())
	}
}

// A high grant delay increases the upload threshold. A high idle jitter
// increases the thresholds. Without a cross delay, learn_cross_delay is off.
func TestCalibrateSession_Thresholds(t *testing.T) {
	dir := writeCalSession(t, calTestLink{
		dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000,
		idleJitterMs: 20, crossDelayMs: 0, grantDelayMs: 30,
	})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := res.propose(cfg)
	// Idle jitter p95 approximately 19 ms → 2 x 19 = 38 ms.
	if p.DlThrMs < 36 || p.DlThrMs > 40 {
		t.Errorf("download threshold %.0f ms, want approximately 38 (2 x idle jitter)", p.DlThrMs)
	}
	// Grant delay p50 approximately 30 ms → floor = p50 + 10 ms.
	if p.UlThrMs < 40 {
		t.Errorf("upload threshold %.0f ms, want 40 or more (grant delay)", p.UlThrMs)
	}
	if p.LearnCrossDelay {
		t.Error("learn_cross_delay = true, want false (no cross delay)")
	}
}

// A session without unshaped blocks gives notes, and the output keeps the
// rates of the session config.
func TestCalibrateSession_MissingBlocks(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	data, err := os.ReadFile(filepath.Join(dir, "blocks.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, "\tunshaped\t") {
			keep = append(keep, line)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks.tsv"), []byte(strings.Join(keep, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) < 2 {
		t.Errorf("notes = %v, want notes for the missing download and upload capacity", res.Notes)
	}
	var buf bytes.Buffer
	if err := writeCalConfig(&buf, dir, cfg, res, res.propose(cfg)); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("min_rate_kbps: %d", cfg.Links[0].Download.MinRateKbps)
	if !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), "# NOTE:") {
		t.Errorf("output does not keep the session rates or has no notes:\n%s", buf.String())
	}
}

// A session with more than one link is rejected: the recordings contain
// the events of all links. Each block can have its own config, thus the
// last block has the second link.
func TestCalibrateSession_MultiLink(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	cfg := testConfig()
	cfg.Links = append(cfg.Links, cfg.Links[0])
	cfg.Links[1].Name = "second"
	rec, err := newRecorderAt(filepath.Join(dir, "06-both-autorate.jsonl"), 0, cfg, time.Unix(1_800_000_000, 0), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := calibrateSession(dir); err == nil || !strings.Contains(err.Error(), "exactly one link") {
		t.Errorf("err = %v, want an error for more than one link", err)
	}
}

// Strings that YAML reads differently without quotes stay the same after
// a reload of the generated config.
func TestWriteCalConfig_QuotesStrings(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Links[0].Name = "wan # backup\nsecond line"
	cfg.Links[0].PingInterfaceName = "yes"
	cfg.Links[0].PingSourceAddr = "192.0.2.1"
	var buf bytes.Buffer
	if err := writeCalConfig(&buf, dir, cfg, res, res.propose(cfg)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Links[0].Name != "wan # backup\nsecond line" || got.Links[0].PingInterfaceName != "yes" || got.Links[0].PingSourceAddr != "192.0.2.1" {
		t.Errorf("name %q, ping_interface %q, ping_source_addr %q after reload", got.Links[0].Name, got.Links[0].PingInterfaceName, got.Links[0].PingSourceAddr)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("disk full") }

func TestWriteCalConfig_ReturnsWriteError(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCalConfig(failWriter{}, dir, cfg, res, res.propose(cfg)); err == nil {
		t.Error("no error from a failing writer")
	}
}

// Without autorate dl blocks, learn_cross_delay keeps the session value.
func TestCalibrateSession_NoCrossDelayKeepsSessionValue(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	data, err := os.ReadFile(filepath.Join(dir, "blocks.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, "\tdl\tautorate\t") {
			keep = append(keep, line)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks.tsv"), []byte(strings.Join(keep, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LearnCrossDelay = true
	if p := res.propose(cfg); !p.LearnCrossDelay {
		t.Error("learn_cross_delay = false, want the session value true")
	}
	found := false
	for _, n := range res.Notes {
		found = found || strings.Contains(n, "learn_cross_delay")
	}
	if !found {
		t.Errorf("notes = %v, want a note for learn_cross_delay", res.Notes)
	}
}

// With ICMP echo, the autorate blocks have no one-way delays. The notes
// tell this (and not that the blocks are missing), and the ping method
// stays icmp.
func TestCalibrateSession_EchoNotes(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2, echo: true})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Join(res.Notes, "\n")
	if strings.Contains(n, "no autorate") || strings.Count(n, "no one-way delays") != 2 || !strings.Contains(n, "idle jitter is not measured") {
		t.Errorf("notes = %v, want notes for missing one-way delays and the idle jitter", res.Notes)
	}
	if p := res.propose(cfg); p.PingMethod != "icmp" || len(p.Reflectors) != 0 {
		t.Errorf("ping_method %s, %d reflectors; want icmp and the session reflectors", p.PingMethod, len(p.Reflectors))
	}
}

// The program selects the active reflectors at random from the list. Thus,
// a reflector with a high idle jitter is not in the list, and the list has
// at most pinger_count + 2 reflectors.
func TestCalibrateSession_ReflectorList(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, noisyReflector: true})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PingerCount = 2
	p := res.propose(cfg)
	if len(p.Reflectors) != 4 || slices.Contains(p.Reflectors, "r5") {
		t.Errorf("reflectors = %v, want 4 without r5", p.Reflectors)
	}
	// The output keeps pinger_count: the reflector list depends on it.
	var buf bytes.Buffer
	if err := writeCalConfig(&buf, dir, cfg, res, p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PingerCount != 2 {
		t.Errorf("pinger_count %d after reload, want 2", got.PingerCount)
	}
}

// With pinger_count 1, the list has at least 4 reflectors (the minimum for
// icmp-timestamp).
func TestCalibrateSession_MinReflectors(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PingerCount = 1
	if p := res.propose(cfg); len(p.Reflectors) < calMinReflectors {
		t.Errorf("reflectors = %v, want %d or more", p.Reflectors, calMinReflectors)
	}
}

// A recording of another format version is rejected.
func TestReadCalRecording_FormatVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"header","format_version":99,"start":"2026-01-01T00:00:00Z","config":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCalRecording(path); err == nil || !strings.Contains(err.Error(), "format version") {
		t.Errorf("err = %v, want a format version error", err)
	}
}

// Without a valid session.txt, the warm-up is unknown: calibrate stops.
func TestCalibrateSession_NeedsSessionTxt(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2, noSessionTxt: true})
	if _, _, err := calibrateSession(dir); err == nil {
		t.Error("no error without session.txt")
	}
	if err := os.WriteFile(filepath.Join(dir, "session.txt"), []byte("block: idle 15s, load 150s, download warm-up in the mode both xs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := calibrateSession(dir); err == nil || !strings.Contains(err.Error(), "block:") {
		t.Errorf("err = %v, want an error for the invalid block line", err)
	}
}

// The download warm-up of the mode both is not idle time: with a cross
// delay of 20 ms in the warm-up, the idle jitter must stay low.
func TestCalibrateSession_WarmupIsNotIdle(t *testing.T) {
	// load_start is 3 s late (setup of the warm-up program): the idle time
	// must end at start + idle time, not at load_start - warm-up.
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, crossDelayMs: 20, warmupS: 10, loadStartDelayS: 3})
	res, _, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.IdleJitterMs.P50 > 5 {
		t.Errorf("idle jitter %.1f ms, want approximately 4 (the warm-up is not idle time)", res.IdleJitterMs.P50)
	}
}

// A variant config of another link is rejected.
func TestCalibrateSession_OtherInterfaces(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	cfg := testConfig()
	cfg.Links[0].Download.Interface = "ifb4lte"
	cfg.Links[0].Upload.Interface = "lte"
	rec, err := newRecorderAt(filepath.Join(dir, "06-both-autorate.jsonl"), 0, cfg, time.Unix(1_800_000_000, 0), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := calibrateSession(dir); err == nil || !strings.Contains(err.Error(), "interfaces") {
		t.Errorf("err = %v, want an error for other interfaces", err)
	}
}

// A reflector that replies to few pings is not a candidate: its few idle
// samples give a too low jitter.
func TestCalibrateSession_FlakyReflector(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, flakyReflector: true})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := res.propose(cfg); slices.Contains(p.Reflectors, "r4") {
		t.Errorf("reflectors = %v, want no r4 (replies to 1 of 50 pings)", p.Reflectors)
	}
}

// A reflector that gives one-way delays in few replies is not a
// candidate: at run time, most of its replies are RTT/2.
func TestCalibrateSession_EchoReflector(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, echoReflector: true})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := res.propose(cfg); slices.Contains(p.Reflectors, "r4") {
		t.Errorf("reflectors = %v, want no r4 (one-way delays only without load)", p.Reflectors)
	}
}

// A reflector with few idle samples in each block is not a candidate, also
// if all blocks together have enough samples: each block has its own
// baseline, and with few samples the jitter is too low.
func TestCalibrateSession_SparseReflector(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, sparseReflector: true})
	res, _, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rf := range res.Reflectors {
		if rf.Addr == "r4" && rf.OWD {
			t.Errorf("r4 is a candidate (idle p95 %.1f ms), want not (few samples in each block)", rf.IdleP95Ms)
		}
	}
}

// Timeouts in the load of the unshaped blocks (a full queue, more than 1 s
// of delay) do not make the reflectors unreliable.
func TestCalibrateSession_UnshapedTimeouts(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, unshapedTimeouts: true})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := res.propose(cfg); p.PingMethod != "icmp-timestamp" {
		t.Errorf("ping_method %q, want icmp-timestamp (reflectors: %+v)", p.PingMethod, res.Reflectors)
	}
}

// A malformed block number stops the calibration.
func TestCalibrateSession_BadBlockNumber(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	p := filepath.Join(dir, "blocks.tsv")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(b), "\n", 3)
	lines[1] = "x" + lines[1][strings.IndexByte(lines[1], '\t'):]
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := calibrateSession(dir); err == nil || !strings.Contains(err.Error(), "block number") {
		t.Errorf("err = %v, want a block number error", err)
	}
}

// Without an unshaped ul block, the note tells that the upload capacity is
// missing, not the one-way delays or the shaper rate.
func TestCalibrateSession_NoUploadCapacityNote(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, grantDelayMs: 15})
	p := filepath.Join(dir, "blocks.tsv")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, "\tul\tunshaped\t") {
			keep = append(keep, l)
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(keep, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "grant delay is not measured: it needs the upload capacity") }) {
		t.Errorf("notes %q, want the grant delay note about the missing upload capacity", res.Notes)
	}
}

// Arguments that are not options stop the command (for example a typo).
func TestRunCalibrate_ExtraArgs(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	out := filepath.Join(t.TempDir(), "out.yaml")
	if got := runCalibrate([]string{"-session", dir, "extra", "-out", out}); got != 2 {
		t.Errorf("exit code %d, want 2", got)
	}
}

// A download capacity of 0 (load generator failure) is not a measurement:
// the session rates stay, with a note.
func TestCalibrateSession_ZeroCapacity(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 0, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.DlCapacity.N != 0 {
		t.Errorf("download capacity N %d, want 0 (not measured)", res.DlCapacity.N)
	}
	for _, want := range []string{"unshaped dl blocks have", "s without throughput", "autorate dl blocks have no download throughput"} {
		if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, want) }) {
			t.Errorf("notes %q, want a note with %q", res.Notes, want)
		}
	}
	if p := res.propose(cfg); p.DlMax != 0 {
		t.Errorf("download max %d, want 0 (session value stays)", p.DlMax)
	}
}

// Seconds without throughput in a block (a short failure) do not make the
// p5 0: they are not capacity measurements.
func TestSecondMeans_SkipsZeroSeconds(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	var rates []calRate
	for i := 0; i < 10; i++ {
		v := 50000.0
		if i < 2 {
			v = 0
		}
		rates = append(rates, calRate{t: t0.Add(time.Duration(i) * time.Second), dl: v})
	}
	got, zeros := secondMeans(rates, func(time.Time) bool { return true }, func(r calRate) float64 { return r.dl })
	if len(got) != 8 || zeros != 2 || newCalDist(got).P5 != 50000 {
		t.Errorf("secondMeans = %v, %d zeros; want 8 values of 50000 and 2 zeros", got, zeros)
	}
}

// Pings in an autorate block without throughput (the load generator
// failed) do not give a cross delay or a grant delay.
func TestGrantDeltas_NeedThroughput(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	b := calBlock{start: t0, idleEnd: t0.Add(time.Second), loadStart: t0.Add(time.Second), loadEnd: t0.Add(10 * time.Second)}
	rec := &calRecording{
		config: testConfig(),
		pings: []calSample{
			{t: t0, reflector: "r1", ulUs: 10000},
			{t: t0.Add(5 * time.Second), reflector: "r1", ulUs: 12000, ulShaperKbps: 5000, ulKbps: 0},
			{t: t0.Add(6 * time.Second), reflector: "r1", ulUs: 30000, ulShaperKbps: 5000, ulKbps: 4500},
		},
	}
	got := b.grantDeltas(rec, func(t time.Time) bool { return !t.Before(b.loadStart) && t.Before(b.loadEnd) })
	if len(got) != 2 || got[0] != 20 || got[1] != 5000 {
		t.Errorf("grantDeltas = %v, want [20 5000] (only the ping with upload throughput)", got)
	}
}

// A carriage return in the session path does not end the comment line.
func TestWriteCalConfig_CarriageReturnInPath(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := writeCalConfig(&b, "/tmp/a\rlinks: x", cfg, res, res.propose(cfg)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\r") {
		t.Errorf("output has a carriage return: %q", strings.SplitN(b.String(), "\n", 2)[0])
	}
}

// On a slow link, the rates are not more than the measured capacity: no
// floor at the rounding step.
func TestPropose_SlowLinkRates(t *testing.T) {
	r := &calResult{
		DlCapacity: calDist{N: 100, P5: 400, P25: 600, P50: 700, P95: 800},
		UlCapacity: calDist{N: 100, P5: 200, P25: 300, P50: 350, P95: 400},
	}
	p := r.propose(testConfig())
	if p.DlMax > 800 || p.DlBase > 600 || p.DlMin > 200 || p.UlMax > 400 || p.UlMin > 100 {
		t.Errorf("rates dl %d/%d/%d, ul %d/%d/%d kbit/s; want not more than the measured values", p.DlMin, p.DlBase, p.DlMax, p.UlMin, p.UlBase, p.UlMax)
	}
	// The activity threshold is below the minimum rates: else the
	// controller cannot leave the idle state.
	if got := activeThrKbps(testConfig(), p); got > min(p.DlMin, p.UlMin)/2 {
		t.Errorf("connection_active_thr_kbps %d, want not more than half of the lowest minimum rate (%d)", got, min(p.DlMin, p.UlMin))
	}
	// The comment shows the capacity, not 0 Mbit/s.
	if w := p.Why["download rates"]; !strings.Contains(w, "p5 0.400") || !strings.Contains(w, "p95 0.800") {
		t.Errorf("download comment %q, want p5 0.400 and p95 0.800 Mbit/s", w)
	}
	if w := p.Why["upload rates"]; !strings.Contains(w, "p5 0.200") {
		t.Errorf("upload comment %q, want p5 0.200 Mbit/s", w)
	}
}

// On a link below connection_active_thr_kbps, the loaded pings still give
// the cross delay and the grant delay: any throughput in the load time is
// load.
func TestCalibrateSession_SlowLinkDelays(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 1000, ulCapKbps: 800, ulCapBothKbps: 800, idleJitterMs: 2, crossDelayMs: 25, grantDelayMs: 15})
	res, _, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.CrossDelayMs.N == 0 || res.GrantDelayMs.N == 0 {
		t.Errorf("cross delay N %d, grant delay N %d; want samples on a 1 Mbit/s link (notes %q)", res.CrossDelayMs.N, res.GrantDelayMs.N, res.Notes)
	}
}

// The output keeps the session activity threshold if it is below the
// minimum rates.
func TestWriteCalConfig_ActiveThreshold(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnectionActiveThrKbps = 1500
	var b strings.Builder
	if err := writeCalConfig(&b, dir, cfg, res, res.propose(cfg)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "connection_active_thr_kbps: 1500\n") {
		t.Errorf("output has no connection_active_thr_kbps: 1500:\n%s", b.String())
	}
}

// calibrate -h is a help request, not an error.
func TestRunCalibrate_Help(t *testing.T) {
	if got := runCalibrate([]string{"-h"}); got != 0 {
		t.Errorf("exit code %d, want 0", got)
	}
}

// The upload threshold comment tells the two reasons: idle jitter and
// grant delay.
func TestCalibrate_UploadThresholdReason(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 4, grantDelayMs: 30})
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := res.propose(cfg).Why["upload threshold"]
	if !strings.Contains(w, "idle jitter") || !strings.Contains(w, "grant delay") {
		t.Errorf("upload threshold reason %q, want idle jitter and grant delay", w)
	}
}

// A ping gets the shaper rate of the last shaper event before it in the
// file, also if the time stamps are not in order. The shaper change that
// the ping causes comes after it in the file: the ping saw the old rate.
func TestReadCalRecording_RateInFileOrder(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	cfg := testConfig()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	rec, err := newRecorderAt(path, 0, cfg, t0, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rec.Shaper("wan", UL, 5000, t0)
	rec.Rate("wan", RateStats{Timestamp: t0.Add(2 * time.Second), UlRateKbps: 4000})
	// The ping has an earlier time than the rate event before it.
	rec.Ping("wan", PingResult{Reflector: "r1", Timestamp: t0.Add(time.Second), OWD: true, UlOWD: 30 * time.Millisecond, DlOWD: 10 * time.Millisecond})
	rec.Shaper("wan", UL, 20000, t0.Add(time.Second))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := readCalRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.pings) != 1 || r.pings[0].ulShaperKbps != 5000 || r.pings[0].ulKbps != 4000 {
		t.Errorf("pings = %+v, want shaper 5000 and upload 4000 kbit/s", r.pings)
	}
}
