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
			loadStart := start.Add(idleS * time.Second)
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
			for ms := 0; ms < (idleS+loadS)*1000; ms += 50 {
				ts := start.Add(time.Duration(ms) * time.Millisecond)
				load := !ts.Before(loadStart)
				dlActive := load && mode != "ul"
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
				ulMs := 10 + offset + jit
				dlMs := 10 - offset + jit
				if dlActive {
					ulMs += l.crossDelayMs
				}
				if ulActive {
					ulMs += l.grantDelayMs
				}
				rec.Ping("wan", PingResult{
					Reflector: r, RTT: 20 * time.Millisecond, Timestamp: ts, OWD: true,
					UlOWD: time.Duration(ulMs * float64(time.Millisecond)),
					DlOWD: time.Duration(dlMs * float64(time.Millisecond)),
				})
			}
			if err := rec.Close(); err != nil {
				t.Fatal(err)
			}
			end := loadEnd.Add(5 * time.Second)
			fmt.Fprintf(&tsv, "%d\t1\t%s\t%s\t%d\t%d\t%d\t%d\tx\tx\t%s\n", num, mode, policy,
				start.Unix(), loadStart.Unix(), loadEnd.Unix(), end.Unix(), name)
			start = end
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks.tsv"), []byte(tsv.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCalibrateSession(t *testing.T) {
	dir := writeCalSession(t, calTestLink{
		dlCapKbps: 200000, ulCapKbps: 40000, ulCapBothKbps: 10000,
		idleJitterMs: 4, crossDelayMs: 20, grantDelayMs: 14, drift: true,
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
// the events of all links.
func TestCalibrateSession_MultiLink(t *testing.T) {
	dir := writeCalSession(t, calTestLink{dlCapKbps: 100000, ulCapKbps: 20000, ulCapBothKbps: 20000, idleJitterMs: 2})
	cfg := testConfig()
	cfg.Links = append(cfg.Links, cfg.Links[0])
	cfg.Links[1].Name = "second"
	rec, err := newRecorderAt(filepath.Join(dir, "01-dl-unshaped.jsonl"), 0, cfg, time.Unix(1_800_000_000, 0), testLogger(t))
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
	cfg.Links[0].Name = "wan # backup"
	cfg.Links[0].PingInterfaceName = "yes"
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
	if got.Links[0].Name != "wan # backup" || got.Links[0].PingInterfaceName != "yes" {
		t.Errorf("name %q, ping_interface %q after reload", got.Links[0].Name, got.Links[0].PingInterfaceName)
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
