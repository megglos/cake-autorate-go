// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Offline calibration: analyze a session of scripts/record-session.sh and
// propose config values for the link.
//
// The analysis uses these parts of the session:
//   - Unshaped dl, ul and both blocks: the load fills the link. The
//     achieved rate is the capacity (both: the upload capacity with
//     download load).
//   - Autorate dl blocks: the upload delay is the upload delay from download
//     activity. The download shaper keeps the ACK rate low, thus the upload
//     has no queue. Do not use the unshaped dl blocks for this: there, the
//     ACKs can fill the upload, and the upload delay is a queue.
//   - The idle time at the start of each block: the delay jitter of each
//     reflector without load.
//   - Autorate ul blocks: the upload delay while the shaper rate is less
//     than half of the upload capacity. Then the upload queue is small, and
//     the delay is the delay of the upload grants (5G).

// calSkipLoadStart is the time at the start of the load that the analysis
// does not use (TCP slow start).
const calSkipLoadStart = 3 * time.Second

// calSkipIdleStart is the time at the start of a block that the analysis
// does not use (the baselines start).
const calSkipIdleStart = 2 * time.Second

// calBlock is one block of a session (one line of blocks.tsv).
type calBlock struct {
	num       int
	mode      string // dl, ul or both
	policy    string // unshaped, fixed, autorate, autorate-NAME
	start     time.Time
	idleEnd   time.Time
	loadStart time.Time
	loadEnd   time.Time
	recording string
}

// calSample is one ping of a recording with one-way delays.
type calSample struct {
	t         time.Time
	reflector string
	dlUs      float64
	ulUs      float64
	// The achieved rates and the upload shaper rate (kbit/s) of the last
	// events before the ping in the file. The controller merges the ping
	// and the rate events in this order, also if the time stamps are not
	// in order.
	dlKbps, ulKbps, ulShaperKbps float64
}

// calRecording contains the events of one block that the analysis needs.
type calRecording struct {
	config *Config
	pings  []calSample
	echo   map[string]int // reflector -> replies without one-way delays
	sent   []calSent      // each ping (replies and timeouts)
	rates  []calRate
}

// calSent is one ping: a reply (with or without one-way delays) or a
// timeout.
type calSent struct {
	t         time.Time
	reflector string
}

type calRate struct {
	t        time.Time
	dl, ul   float64
	rateKbps float64
}

// calDist is a distribution summary.
type calDist struct {
	N   int     `json:"n"`
	P5  float64 `json:"p5"`
	P10 float64 `json:"p10"`
	P25 float64 `json:"p25"`
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
}

func newCalDist(v []float64) calDist {
	if len(v) == 0 {
		return calDist{}
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return calDist{N: len(s), P5: q(0.05), P10: q(0.10), P25: q(0.25), P50: q(0.50), P75: q(0.75), P90: q(0.90), P95: q(0.95)}
}

// calReflector is the result for one reflector.
type calReflector struct {
	Addr      string
	OWD       bool    // replies with ICMP timestamps
	IdleP95Ms float64 // jitter without load (p95 of the delta, larger direction)
	Replies   int     // replies with one-way delays
	Sent      int     // pings (replies and timeouts)
}

// calResult is the result of the analysis.
type calResult struct {
	DlCapacity, UlCapacity calDist // kbit/s, mean of each second
	// UlCapacityBoth is the upload capacity with load in the two
	// directions. On 5G, it is much lower.
	UlCapacityBoth calDist
	IdleJitterMs   calDist // p95 of each reflector, larger direction
	CrossDelayMs   calDist // upload delta while the download is saturated
	GrantDelayMs   calDist // upload delta with upload load, low shaper rate
	Reflectors     []calReflector
	// Probed has each reflector with a ping in the session: a reply or a
	// timeout.
	Probed map[string]bool
	Notes  []string
}

// calibrateSession analyzes the session in dir.
func calibrateSession(dir string) (*calResult, *Config, error) {
	blocks, err := readBlocks(dir)
	if err != nil {
		return nil, nil, err
	}
	res := &calResult{}
	var cfg *Config
	var dlCap, ulCap, ulCapBoth, cross, grant []float64
	var nCrossBlocks, nGrantBlocks int
	// Seconds and pings without throughput in the load.
	var dlZeros, ulZeros, bothZeros, crossNoLoad int
	idle := map[string][]calSample{} // key: block number and reflector
	echo := map[string]int{}
	sent, replied := map[string]int{}, map[string]int{}
	probed := map[string]bool{} // each reflector with a ping (reply or timeout)

	for _, b := range blocks {
		rec, err := readCalRecording(filepath.Join(dir, b.recording))
		if err != nil {
			return nil, nil, fmt.Errorf("block %d: %w", b.num, err)
		}
		// Each block can have its own config (record-session.sh -A).
		if rec.config == nil || len(rec.config.Links) != 1 {
			return nil, nil, fmt.Errorf("block %d: the config must have exactly one link (calibrate one link at a time)", b.num)
		}
		if cfg == nil {
			// Use the settings that apply to the link: the link overrides win.
			cfg = rec.config.ForLink(&rec.config.Links[0])
		}
		// A variant config (record-session.sh -A) must be for the same link.
		first, l := cfg.Links[0], rec.config.Links[0]
		if l.Download.Interface != first.Download.Interface || l.Upload.Interface != first.Upload.Interface {
			return nil, nil, fmt.Errorf("block %d: interfaces %s/%s, the first block has %s/%s (calibrate one link at a time)",
				b.num, l.Download.Interface, l.Upload.Interface, first.Download.Interface, first.Upload.Interface)
		}
		for r, n := range rec.echo {
			echo[r] += n
		}
		// The reply ratio: not the load of the unshaped blocks. There, the
		// queue is intentionally full, and pings time out (more than 1 s of
		// delay) for all reflectors.
		counts := func(t time.Time) bool { return b.policy != "unshaped" || t.Before(b.idleEnd) }
		for _, p := range rec.sent {
			probed[p.reflector] = true
			if counts(p.t) {
				sent[p.reflector]++
			}
		}
		for _, p := range rec.pings {
			if counts(p.t) {
				replied[p.reflector]++
			}
		}
		loadFrom := b.loadStart.Add(calSkipLoadStart)
		for _, p := range rec.pings {
			if !p.t.Before(b.start.Add(calSkipIdleStart)) && p.t.Before(b.idleEnd) {
				k := fmt.Sprintf("%d %s", b.num, p.reflector)
				idle[k] = append(idle[k], p)
			}
		}
		inLoad := func(t time.Time) bool { return !t.Before(loadFrom) && t.Before(b.loadEnd) }

		switch {
		case b.policy == "unshaped" && b.mode == "dl":
			v, z := secondMeans(rec.rates, inLoad, func(r calRate) float64 { return r.dl })
			dlCap, dlZeros = append(dlCap, v...), dlZeros+z
		case b.policy == "unshaped" && b.mode == "ul":
			v, z := secondMeans(rec.rates, inLoad, func(r calRate) float64 { return r.ul })
			ulCap, ulZeros = append(ulCap, v...), ulZeros+z
		case b.policy == "unshaped" && b.mode == "both":
			v, z := secondMeans(rec.rates, inLoad, func(r calRate) float64 { return r.ul })
			ulCapBoth, bothZeros = append(ulCapBoth, v...), bothZeros+z
		case strings.HasPrefix(b.policy, "autorate") && b.mode == "dl":
			nCrossBlocks++
			// The download shaper keeps the ACK rate low. Thus, the upload
			// has no queue (unshaped, the ACKs can fill the upload).
			// Only the pings with download throughput: without it (the load
			// generator failed), the delay is not a cross delay. Any
			// throughput counts (not connection_active_thr_kbps): the pings
			// are in the load time, and a slow link can be below the
			// threshold.
			d, used := loadDeltas(rec.pings, b, inLoad, func(p calSample) float64 { return p.ulUs })
			for i, p := range used {
				if p.dlKbps > 0 {
					cross = append(cross, d[i])
				} else {
					crossNoLoad++
				}
			}
		case strings.HasPrefix(b.policy, "autorate") && b.mode == "ul":
			nGrantBlocks++
			grant = append(grant, b.grantDeltas(rec, inLoad)...)
		}
	}
	if cfg == nil {
		return nil, nil, fmt.Errorf("%s: no blocks", dir)
	}

	res.DlCapacity, res.UlCapacity, res.UlCapacityBoth = newCalDist(dlCap), newCalDist(ulCap), newCalDist(ulCapBoth)
	res.CrossDelayMs = newCalDist(cross)

	// The grant analysis needs the upload capacity: use only the pings
	// with a shaper rate below half of the median capacity.
	var g []float64
	for i := 0; i+1 < len(grant); i += 2 {
		if res.UlCapacity.N > 0 && grant[i+1] < 0.5*res.UlCapacity.P50 {
			g = append(g, grant[i])
		}
	}
	res.GrantDelayMs = newCalDist(g)

	// The clock offset of a reflector changes slowly. Thus, the baseline
	// is the minimum in each block.
	deltas := map[string][]float64{}
	for k, v := range idle {
		r := k[strings.IndexByte(k, ' ')+1:]
		if len(v) < calMinBlockSamples {
			continue
		}
		base := baselinesOf(v)
		for _, p := range v {
			deltas[r] = append(deltas[r], math.Max(p.dlUs-base[0], p.ulUs-base[1])/1000)
		}
	}
	var jitter []float64
	for r, d := range deltas {
		dist := newCalDist(d)
		rf := calReflector{Addr: r, IdleP95Ms: dist.P95, Replies: replied[r], Sent: sent[r]}
		// Only a reflector with enough idle samples and replies is a
		// candidate. A reflector with few replies has a too low jitter
		// (one sample is its own baseline) and is not usable at run time.
		// Replies without one-way delays do not count: at run time, they
		// give RTT/2.
		rf.OWD = len(d) >= calMinIdleSamples && float64(rf.Replies) >= calMinReplyRatio*float64(rf.Sent)
		res.Reflectors = append(res.Reflectors, rf)
		if rf.OWD {
			jitter = append(jitter, dist.P95)
		}
	}
	for r := range echo {
		if _, ok := deltas[r]; !ok {
			res.Reflectors = append(res.Reflectors, calReflector{Addr: r, Sent: sent[r]})
		}
	}
	sort.Slice(res.Reflectors, func(i, j int) bool {
		a, b := res.Reflectors[i], res.Reflectors[j]
		if a.OWD != b.OWD {
			return a.OWD
		}
		if a.IdleP95Ms != b.IdleP95Ms {
			return a.IdleP95Ms < b.IdleP95Ms
		}
		return a.Addr < b.Addr
	})
	res.IdleJitterMs = newCalDist(jitter)
	res.Probed = probed

	// Seconds without throughput (the load generator failed, or the link
	// was down) are not a capacity measurement (secondMeans).
	for _, c := range []struct {
		n     int
		zeros int
		name  string
	}{{res.DlCapacity.N, dlZeros, "dl"}, {res.UlCapacity.N, ulZeros, "ul"}, {res.UlCapacityBoth.N, bothZeros, "both"}} {
		if c.zeros > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("the unshaped %s blocks have %d s without throughput (load generator or link failure): these seconds are not used (%d s used)", c.name, c.zeros, c.n))
		}
	}
	if res.DlCapacity.N == 0 {
		res.Notes = append(res.Notes, "no unshaped dl block with throughput: the download rates are not measured")
	}
	if res.UlCapacity.N == 0 {
		res.Notes = append(res.Notes, "no unshaped ul block with throughput: the upload rates are not measured")
	}
	if res.IdleJitterMs.N == 0 {
		res.Notes = append(res.Notes, "no idle pings with one-way delays: the idle jitter is not measured, the delay thresholds are the default values")
	}
	if res.UlCapacity.N > 0 && res.UlCapacityBoth.N == 0 {
		res.Notes = append(res.Notes, "no unshaped both block: the upload minimum and base rates come from the upload capacity without download load and can be too high")
	}
	// The delay analysis needs one-way delays (ping_method icmp-timestamp).
	switch {
	case res.CrossDelayMs.N == 0 && nCrossBlocks == 0:
		res.Notes = append(res.Notes, "no autorate dl block: the upload delay from download activity is not measured, learn_cross_delay keeps the value of the session config")
	case res.CrossDelayMs.N == 0 && crossNoLoad > 0:
		res.Notes = append(res.Notes, "the autorate dl blocks have no download throughput (load generator or link failure): the upload delay from download activity is not measured, learn_cross_delay keeps the value of the session config")
	case res.CrossDelayMs.N == 0:
		res.Notes = append(res.Notes, "the autorate dl blocks have no one-way delays (use ping_method icmp-timestamp): the upload delay from download activity is not measured, learn_cross_delay keeps the value of the session config")
	}
	switch {
	case res.GrantDelayMs.N == 0 && nGrantBlocks == 0:
		res.Notes = append(res.Notes, "no autorate ul block: the upload grant delay is not measured")
	case res.GrantDelayMs.N == 0 && res.UlCapacity.N == 0:
		res.Notes = append(res.Notes, "the upload grant delay is not measured: it needs the upload capacity of an unshaped ul block")
	case res.GrantDelayMs.N == 0:
		res.Notes = append(res.Notes, "the autorate ul blocks have no one-way delays or no low shaper rate: the upload grant delay is not measured")
	}
	return res, cfg, nil
}

// secondMeans returns the mean rate of each second of the load, and the
// number of seconds without throughput. A second without throughput (load
// generator or link failure) is not a capacity measurement.
func secondMeans(rates []calRate, in func(time.Time) bool, get func(calRate) float64) ([]float64, int) {
	var out []float64
	var zeros int
	var sec int64 = -1
	var sum float64
	var n int
	add := func() {
		if m := sum / float64(n); m > 0 {
			out = append(out, m)
		} else {
			zeros++
		}
	}
	for _, r := range rates {
		if !in(r.t) {
			continue
		}
		s := r.t.Unix()
		if s != sec && n > 0 {
			add()
			sum, n = 0, 0
		}
		sec = s
		sum += get(r)
		n++
	}
	if n > 0 {
		add()
	}
	return out, zeros
}

// baselinesOf returns the minimum download and upload delay of the samples.
func baselinesOf(v []calSample) [2]float64 {
	b := [2]float64{math.Inf(1), math.Inf(1)}
	for _, p := range v {
		b[0], b[1] = math.Min(b[0], p.dlUs), math.Min(b[1], p.ulUs)
	}
	return b
}

// loadDeltas returns the deltas (ms) during the load, and the pings that
// they are for. The baseline of each reflector is its minimum in the idle
// time of the same block. Pings of a reflector without a baseline are not
// used.
func loadDeltas(pings []calSample, b calBlock, in func(time.Time) bool, get func(calSample) float64) ([]float64, []calSample) {
	base := map[string]float64{}
	for _, p := range pings {
		if p.t.Before(b.idleEnd) {
			if v, ok := base[p.reflector]; !ok || get(p) < v {
				base[p.reflector] = get(p)
			}
		}
	}
	var out []float64
	var used []calSample
	for _, p := range pings {
		if bv, ok := base[p.reflector]; ok && in(p.t) {
			out = append(out, (get(p)-bv)/1000)
			used = append(used, p)
		}
	}
	return out, used
}

// grantDeltas returns pairs (upload delta in ms, upload shaper rate in
// kbit/s) for each ping in the load with upload throughput.
func (b calBlock) grantDeltas(rec *calRecording, in func(time.Time) bool) []float64 {
	d, pings := loadDeltas(rec.pings, b, in, func(p calSample) float64 { return p.ulUs })
	var out []float64
	for i, p := range pings {
		// Without upload throughput (the load generator failed), the
		// delay is not a grant delay. Any throughput counts, as for the
		// cross delay.
		if p.ulShaperKbps > 0 && p.ulKbps > 0 {
			out = append(out, d[i], p.ulShaperKbps)
		}
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// readBlocks reads blocks.tsv and the idle time from session.txt.
func readBlocks(dir string) ([]calBlock, error) {
	// The idle time of each block starts at the start of the block. The
	// load (or the download warm-up of the mode both) starts after it.
	// Sessions of an older script have no warm-up value.
	data, err := os.ReadFile(filepath.Join(dir, "session.txt"))
	if err != nil {
		return nil, err
	}
	m := regexp.MustCompile(`(?m)^block: idle (\d+)s, load \d+s(?:, download warm-up in the mode both (\d+)s)?$`).FindSubmatch(data)
	if m == nil {
		return nil, fmt.Errorf("%s: no valid \"block:\" line", filepath.Join(dir, "session.txt"))
	}
	idleS, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return nil, fmt.Errorf("session.txt: idle time %q: %w", m[1], err)
	}
	warmup := time.Duration(0)
	if len(m[2]) > 0 {
		s, err := strconv.Atoi(string(m[2]))
		if err != nil {
			return nil, fmt.Errorf("session.txt: warm-up %q: %w", m[2], err)
		}
		warmup = time.Duration(s) * time.Second
	}
	f, err := os.Open(filepath.Join(dir, "blocks.tsv"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var blocks []calBlock
	sc := bufio.NewScanner(f)
	for line := 0; sc.Scan(); line++ {
		col := strings.Split(sc.Text(), "\t")
		if line == 0 {
			continue
		}
		if len(col) < 11 {
			return nil, fmt.Errorf("blocks.tsv line %d: %d columns, want 11", line+1, len(col))
		}
		num, err := strconv.Atoi(col[0])
		if err != nil {
			return nil, fmt.Errorf("blocks.tsv line %d: block number: %w", line+1, err)
		}
		ts := make([]time.Time, 4)
		for i := range ts {
			s, err := strconv.ParseInt(col[4+i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("blocks.tsv line %d: %w", line+1, err)
			}
			ts[i] = time.Unix(s, 0)
		}
		b := calBlock{num: num, mode: col[2], policy: col[3], start: ts[0], loadStart: ts[1], loadEnd: ts[2], recording: col[10]}
		// The script starts the load (or the warm-up) after the idle time,
		// but load_start is written after the warm-up program stops, with
		// its setup and cleanup time. Thus, the end of the idle time is
		// start + idle time, not load_start - warm-up.
		b.idleEnd = b.start.Add(time.Duration(idleS) * time.Second)
		if b.mode == "both" {
			b.idleEnd = minTime(b.idleEnd, b.loadStart.Add(-warmup))
		} else {
			b.idleEnd = minTime(b.idleEnd, b.loadStart)
		}
		blocks = append(blocks, b)
	}
	return blocks, sc.Err()
}

// readCalRecording reads a recording (JSONL, gzip if the name ends with .gz).
func readCalRecording(path string) (*calRecording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	if !sc.Scan() {
		return nil, fmt.Errorf("%s: empty recording", path)
	}
	var h recHeaderEvent
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Type != recHeader {
		return nil, fmt.Errorf("%s: no header", path)
	}
	if h.FormatVersion != recordFormatVersion {
		return nil, fmt.Errorf("%s: recording format version %d, this program reads version %d", path, h.FormatVersion, recordFormatVersion)
	}
	start, err := time.Parse(time.RFC3339Nano, h.Start)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rec := &calRecording{config: h.Config, echo: map[string]int{}}
	var dlKbps, ulKbps, ulShaperKbps float64 // the last values in the file
	for sc.Scan() {
		var ev struct {
			T         int64     `json:"t"`
			Type      string    `json:"type"`
			Reflector string    `json:"reflector"`
			Timeout   bool      `json:"timeout"`
			DlOWDUs   *int64    `json:"dl_owd_us"`
			UlOWDUs   *int64    `json:"ul_owd_us"`
			DlKbps    float64   `json:"dl_kbps"`
			UlKbps    float64   `json:"ul_kbps"`
			Dir       Direction `json:"dir"`
			RateKbps  int       `json:"rate_kbps"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		t := start.Add(time.Duration(ev.T) * time.Microsecond)
		switch ev.Type {
		case recPing:
			rec.sent = append(rec.sent, calSent{t: t, reflector: ev.Reflector})
			if ev.Timeout {
				continue
			}
			if ev.DlOWDUs == nil || ev.UlOWDUs == nil {
				rec.echo[ev.Reflector]++
				continue
			}
			rec.pings = append(rec.pings, calSample{t: t, reflector: ev.Reflector, dlUs: float64(*ev.DlOWDUs), ulUs: float64(*ev.UlOWDUs),
				dlKbps: dlKbps, ulKbps: ulKbps, ulShaperKbps: ulShaperKbps})
		case recRate:
			rec.rates = append(rec.rates, calRate{t: t, dl: ev.DlKbps, ul: ev.UlKbps})
			dlKbps, ulKbps = ev.DlKbps, ev.UlKbps
		case recShaper:
			// The recorder writes the ping before the shaper change that
			// it causes. Thus, the ping gets the rate that it saw.
			if ev.Dir == UL {
				ulShaperKbps = float64(ev.RateKbps)
			}
		}
	}
	return rec, sc.Err()
}

// calProposal is the proposed config for one link, with a reason for each
// value.
type calProposal struct {
	DlMin, DlBase, DlMax int
	UlMin, UlBase, UlMax int
	DlThrMs, UlThrMs     float64
	LearnCrossDelay      bool
	PingMethod           string
	Reflectors           []string
	Why                  map[string]string
}

// Rules for the proposal. They come from the 5G sessions of 2026-10-10 and
// 2026-10-11 (see the pull request of this change).
const (
	// calDefaultThrMs is the default delay threshold.
	calDefaultThrMs = 30.0
	// calJitterFactor: the threshold is at least this factor times the p95
	// of the idle jitter. Else the jitter alone shows bufferbloat.
	calJitterFactor = 2.0
	// calStableRatio: a direction has a stable capacity (for example
	// fiber or cable, not 5G) if the p25 of its unshaped capacity is at
	// least this ratio of the p75. The p5 and the p95 are not robust: a
	// session has few 1 s means, and one dip (a new connection of the
	// load) is the p5.
	calStableRatio = 0.9
	// calStableBaseFactor: on a stable link, the base rate is this factor
	// times the p25 of the capacity. Then the shaper is below the
	// bottleneck, and the queue is in CAKE, not in the network.
	calStableBaseFactor = 0.95
	// calStableThrMs is the minimum delay threshold on a stable link. A
	// fiber line with 3 ms idle delay and a queue of +12 ms in the
	// network did not get a rate decrease with 30 ms. With 10 ms, the
	// delay under load was +5 ms at a higher rate.
	calStableThrMs = 10.0
	// calCrossDelayMinMs: above this median upload delay from download
	// activity, learn_cross_delay is useful.
	calCrossDelayMinMs = 10.0
	// calCrossMarginMs: without learn_cross_delay, the upload threshold is
	// at least the p95 of the cross delay plus this margin. On a fiber
	// line (cross delay p50 1 ms, p95 14 ms), an upload threshold of 10 ms
	// pushed the upload rate to the minimum during the download: 246
	// instead of 334 Mbit/s in the upload test.
	calCrossMarginMs = 10.0
	// calGrantMarginMs: the upload threshold is at least the median of the
	// grant delay plus this margin. Below it, the upload rate collapses. On
	// the 5G link, the median was 13 to 15 ms; a threshold of 15 ms gave
	// 30 to 43 % of the upload capacity, 25 ms gave 66 to 68 %.
	calGrantMarginMs = 10.0
	// calMinReflectors is the minimum number of reflectors with ICMP
	// timestamps for ping_method icmp-timestamp.
	calMinReflectors = 4
	// calMinIdleSamples and calMinReplyRatio: a reflector needs this number
	// of idle samples and this ratio of replies to pings.
	calMinIdleSamples = 20
	// calMinBlockSamples is the number of idle samples of a reflector in a
	// block, for the jitter of that block. Each block has its own
	// baseline (the minimum): with few samples, the jitter is too low.
	calMinBlockSamples = 10
	calMinReplyRatio   = 0.9
	// calSpareReflectors is the number of reflectors in the list more than
	// pinger_count (spares for misbehaving reflectors).
	calSpareReflectors = 2
	// calReflectorMaxJitter: a reflector with an idle jitter of more than
	// this factor times the median is not in the list.
	calReflectorMaxJitter = 2.0
)

// mbit formats a rate in kbit/s as Mbit/s for the comments: 3 decimals
// below 1 Mbit/s (a slow link), 1 decimal below 100 Mbit/s, else none.
func mbit(kbps float64) string {
	switch {
	case kbps < 1000:
		return fmt.Sprintf("%.3f", kbps/1000)
	case kbps < 100000:
		return fmt.Sprintf("%.1f", kbps/1000)
	}
	return fmt.Sprintf("%.0f", kbps/1000)
}

// propose makes the config proposal from the result.
func (r *calResult) propose(cfg *Config) calProposal {
	p := calProposal{Why: map[string]string{}, LearnCrossDelay: cfg.LearnCrossDelay}
	pingerCount := cfg.PingerCount
	// Round to the step. Below one step (a slow link), keep the kbit/s:
	// a floor at the step would give a rate above the measured capacity.
	round := func(kbps float64, step int) int {
		if kbps < float64(step) {
			return max(1, int(math.Round(kbps)))
		}
		return int(math.Round(kbps/float64(step))) * step
	}

	// Rates: max = p95 of the capacity, base = p25 (a rate that the link
	// usually has), min = p5 / 2 (the shaper must be able to go below the
	// low capacity values; the 1 s means hide short decreases). On a
	// stable link, p25 is the capacity: base = calStableBaseFactor x p25.
	stable := func(c calDist) bool { return c.N > 0 && c.P25 >= calStableRatio*c.P75 }
	base := func(c calDist, step int) (int, string) {
		if stable(c) {
			// A coarse step can round the base up to the capacity (for
			// example 9500 to 10000 kbit/s): then use a finer step.
			target := calStableBaseFactor * c.P25
			b := int(math.Floor(target))
			for _, st := range []int{step, step / 10} {
				if v := round(target, st); float64(v) < c.P25 {
					b = v
					break
				}
			}
			return b, fmt.Sprintf("; stable capacity (p25 %s >= %.0f %% of p75 %s): base = %.0f %% of p25", mbit(c.P25), 100*calStableRatio, mbit(c.P75), 100*calStableBaseFactor)
		}
		return round(c.P25, step), ""
	}
	if c := r.DlCapacity; c.N > 0 {
		b, why := base(c, 1000)
		p.DlMin, p.DlBase, p.DlMax = round(c.P5/2, 1000), b, round(c.P95, 1000)
		p.Why["download rates"] = fmt.Sprintf("unshaped capacity (1 s means, %d s): p5 %s, p25 %s, p50 %s, p95 %s Mbit/s", c.N, mbit(c.P5), mbit(c.P25), mbit(c.P50), mbit(c.P95)) + why
	}
	if c := r.UlCapacity; c.N > 0 {
		b, why := base(c, 500)
		p.UlMin, p.UlBase, p.UlMax = round(c.P5/2, 500), b, round(c.P95, 500)
		p.Why["upload rates"] = fmt.Sprintf("unshaped capacity (1 s means, %d s): p5 %s, p25 %s, p50 %s, p95 %s Mbit/s", c.N, mbit(c.P5), mbit(c.P25), mbit(c.P50), mbit(c.P95)) + why
		// With download load, the upload capacity can be much lower. The
		// minimum must be below it.
		if b := r.UlCapacityBoth; b.N > 0 {
			p.UlMin = min(p.UlMin, round(b.P5/2, 500))
			p.UlBase = min(p.UlBase, round(b.P50, 500))
			p.Why["upload rates"] += fmt.Sprintf("; with download load: p5 %s, p50 %s Mbit/s (min = min(p5/2), base = min(base, p50 with download load))", mbit(b.P5), mbit(b.P50))
		}
		p.UlBase = max(p.UlBase, p.UlMin)
	}

	// Thresholds. A stable direction has a lower minimum.
	floor := func(c calDist) float64 {
		if stable(c) {
			return calStableThrMs
		}
		return calDefaultThrMs
	}
	dlFloor, ulFloor := floor(r.DlCapacity), floor(r.UlCapacity)
	p.DlThrMs, p.UlThrMs = dlFloor, ulFloor
	if j := r.IdleJitterMs; j.N > 0 {
		jit := math.Ceil(calJitterFactor * j.P50)
		p.DlThrMs, p.UlThrMs = math.Max(dlFloor, jit), math.Max(ulFloor, jit)
		p.Why["download threshold"] = fmt.Sprintf("idle jitter (p95 of each reflector): median %.1f ms; threshold = max(%.0f, %.0f x jitter)", j.P50, dlFloor, calJitterFactor)
		p.Why["upload threshold"] = fmt.Sprintf("idle jitter (p95 of each reflector): median %.1f ms; threshold = max(%.0f, %.0f x jitter)", j.P50, ulFloor, calJitterFactor)
	}
	if c := r.CrossDelayMs; c.N > 0 {
		p.LearnCrossDelay = c.P50 > calCrossDelayMinMs
		p.Why["learn_cross_delay"] = fmt.Sprintf("upload delay from download activity: p50 %.1f, p95 %.1f ms (on above %.0f ms)", c.P50, c.P95, calCrossDelayMinMs)
	}
	if g := r.GrantDelayMs; g.N > 0 {
		floor := math.Ceil(g.P50 + calGrantMarginMs)
		grant := fmt.Sprintf("upload grant delay: p50 %.1f, p95 %.1f ms (%d pings), floor p50 + %.0f ms = %.0f ms", g.P50, g.P95, g.N, calGrantMarginMs, floor)
		if w := p.Why["upload threshold"]; w != "" {
			p.Why["upload threshold"] = w + "; " + grant + "; threshold = max of the two"
		} else {
			p.Why["upload threshold"] = grant + fmt.Sprintf("; threshold = max(%.0f, floor)", ulFloor)
		}
		p.UlThrMs = math.Max(p.UlThrMs, floor)
	}
	// Without learn_cross_delay, the upload delay from download activity
	// stays in the upload delta. Below its p95, the download pushes the
	// upload rate down to the minimum.
	// With learn_cross_delay, core removes a learned delay of approximately
	// the p50, but not more than the upload threshold. Thus, the threshold
	// must be above the p50, and above the rest at the p95 (p95 - p50).
	if c := r.CrossDelayMs; c.N > 0 {
		floor := math.Ceil(c.P95 + calCrossMarginMs)
		why := fmt.Sprintf("floor p95 + %.0f ms = %.0f ms", calCrossMarginMs, floor)
		if p.LearnCrossDelay {
			floor = math.Ceil(math.Max(c.P50, c.P95-c.P50) + calCrossMarginMs)
			why = fmt.Sprintf("learned delay p50, floor max(p50, p95 - p50) + %.0f ms = %.0f ms", calCrossMarginMs, floor)
		}
		if floor > p.UlThrMs {
			p.UlThrMs = floor
			p.Why["upload threshold"] += fmt.Sprintf("; upload delay from download activity: p50 %.1f, p95 %.1f ms, %s", c.P50, c.P95, why)
		}
	}

	// Ping method and reflectors.
	// The program selects the active reflectors at random from the list
	// (and the spares from the rest). Thus, the list must not contain the
	// reflectors with a high jitter: keep the best pinger_count + 2, and
	// leave out the ones with more than 2 x the median jitter.
	var owd []string
	for _, rf := range r.Reflectors {
		if rf.OWD && (r.IdleJitterMs.N == 0 || rf.IdleP95Ms <= calReflectorMaxJitter*r.IdleJitterMs.P50) {
			owd = append(owd, rf.Addr)
		}
	}
	p.PingMethod = "icmp"
	if len(owd) >= calMinReflectors {
		p.PingMethod = "icmp-timestamp"
		n := len(owd)
		p.Reflectors = owd[:min(len(owd), max(pingerCount+calSpareReflectors, calMinReflectors))]
		p.Why["reflectors"] = fmt.Sprintf("%d reflectors reply to ICMP timestamps with a low idle jitter; the %d with the lowest jitter (the program selects %d of them at random)", n, len(p.Reflectors), min(pingerCount, len(p.Reflectors)))
	} else {
		p.Why["ping_method"] = fmt.Sprintf("only %d reflectors reply to ICMP timestamps (need %d)", len(owd), calMinReflectors)
	}
	return p
}

// writeCalConfig writes the proposal as a config file, based on the
// config of the session (interfaces). Each value has a comment with the
// measured data.
// errWriter keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(b []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(b)
	e.err = err
	return n, err
}

// yamlString returns s as a YAML scalar. It quotes s if necessary (for
// example "wan # backup" or "yes").
func yamlString(s string) string {
	// yaml.Marshal writes a block scalar for a string with a line break.
	// The output needs one line. A Go quoted string is a valid YAML
	// double-quoted scalar.
	if strings.ContainsAny(s, "\n\r") {
		return strconv.Quote(s)
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func writeCalConfig(out io.Writer, dir string, cfg *Config, r *calResult, p calProposal) error {
	w := &errWriter{w: out}
	link := cfg.Links[0]
	fmt.Fprintf(w, "# cake-autorate-go config from the calibration of %s\n", strings.NewReplacer("\n", " ", "\r", " ").Replace(dir))
	fmt.Fprintf(w, "# Generated %s. Examine each value before use.\n", time.Now().Format(time.DateOnly))
	for _, n := range r.Notes {
		fmt.Fprintf(w, "# NOTE: %s\n", n)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "links:")
	fmt.Fprintf(w, "  - name: %s\n", yamlString(link.Name))
	dir2 := func(name string, dc DirectionConfig, minR, baseR, maxR int, thr float64, why, whyThr string) {
		fmt.Fprintf(w, "    %s:\n", name)
		fmt.Fprintf(w, "      interface: %s\n", yamlString(dc.Interface))
		fmt.Fprintf(w, "      adjust: true\n")
		if why != "" {
			fmt.Fprintf(w, "      # %s\n", why)
			fmt.Fprintf(w, "      min_rate_kbps: %d\n      base_rate_kbps: %d\n      max_rate_kbps: %d\n", minR, baseR, maxR)
		} else {
			fmt.Fprintf(w, "      # not measured: values of the session config\n")
			fmt.Fprintf(w, "      min_rate_kbps: %d\n      base_rate_kbps: %d\n      max_rate_kbps: %d\n", dc.MinRateKbps, dc.BaseRateKbps, dc.MaxRateKbps)
		}
		if whyThr != "" {
			fmt.Fprintf(w, "      # %s\n", whyThr)
		}
		fmt.Fprintf(w, "      owd_delta_delay_thr_ms: %g\n", thr)
		fmt.Fprintf(w, "      avg_owd_delta_max_adjust_down_thr_ms: %g\n", 2*thr)
	}
	dir2("download", link.Download, p.DlMin, p.DlBase, p.DlMax, p.DlThrMs, p.Why["download rates"], p.Why["download threshold"])
	dir2("upload", link.Upload, p.UlMin, p.UlBase, p.UlMax, p.UlThrMs, p.Why["upload rates"], p.Why["upload threshold"])
	if link.PingInterfaceName != "" {
		fmt.Fprintf(w, "    ping_interface: %s\n", yamlString(link.PingInterfaceName))
	}
	if link.PingSourceAddr != "" {
		fmt.Fprintf(w, "    ping_source_addr: %s\n", yamlString(link.PingSourceAddr))
	}
	if len(p.Reflectors) > 0 {
		fmt.Fprintf(w, "    # %s\n", p.Why["reflectors"])
		fmt.Fprintln(w, "    reflectors:")
		for _, a := range p.Reflectors {
			j := ""
			for _, rf := range r.Reflectors {
				if rf.Addr == a {
					j = fmt.Sprintf("  # idle jitter p95 %.1f ms", rf.IdleP95Ms)
				}
			}
			fmt.Fprintf(w, "      - %s%s\n", yamlString(a), j)
		}
	} else if len(link.Reflectors) > 0 {
		fmt.Fprintln(w, "    # not measured: the reflectors of the session config")
		fmt.Fprintln(w, "    reflectors:")
		for _, a := range link.Reflectors {
			fmt.Fprintf(w, "      - %s\n", yamlString(a))
		}
	}
	fmt.Fprintln(w)
	if why, ok := p.Why["ping_method"]; ok {
		fmt.Fprintf(w, "# %s\n", why)
	}
	fmt.Fprintf(w, "ping_method: %s\n", p.PingMethod)
	fmt.Fprintln(w, "# from the session config: the proposal of the reflectors uses it")
	fmt.Fprintf(w, "pinger_count: %d\n", cfg.PingerCount)
	if why, ok := p.Why["learn_cross_delay"]; ok {
		fmt.Fprintf(w, "# %s\n", why)
	}
	fmt.Fprintf(w, "learn_cross_delay: %v\n", p.LearnCrossDelay)
	// Core uses the threshold for idle and sleep. If it is not below the
	// minimum rates, the traffic of a slow link stays below it, and the
	// controller cannot leave the idle state.
	thr := activeThrKbps(cfg, p)
	if thr < cfg.ConnectionActiveThrKbps {
		fmt.Fprintf(w, "# session value %d kbit/s, lowered to half of the lowest minimum rate\n", cfg.ConnectionActiveThrKbps)
	} else {
		fmt.Fprintln(w, "# from the session config")
	}
	fmt.Fprintf(w, "connection_active_thr_kbps: %d\n", thr)
	return w.err
}

// activeThrKbps returns connection_active_thr_kbps for the output: the
// session value, but not more than half of the lowest minimum rate of the
// output (minimum 1).
func activeThrKbps(cfg *Config, p calProposal) int {
	link := cfg.Links[0]
	lowest := math.MaxInt
	for _, m := range []struct{ proposed, session int }{
		{p.DlMin, link.Download.MinRateKbps}, {p.UlMin, link.Upload.MinRateKbps},
	} {
		v := m.proposed
		if v == 0 {
			v = m.session
		}
		if v > 0 {
			lowest = min(lowest, v)
		}
	}
	if lowest == math.MaxInt {
		return cfg.ConnectionActiveThrKbps
	}
	return max(1, min(cfg.ConnectionActiveThrKbps, lowest/2))
}

// runCalibrate is the "calibrate" command.
func runCalibrate(args []string) int {
	fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	session := fs.String("session", "", "session directory of scripts/record-session.sh (required)")
	out := fs.String("out", "", "write the config to this file (default: standard output)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: cake-autorate-go calibrate -session DIR [-out FILE]")
		fmt.Fprintln(fs.Output(), "Analyze a recorded session and propose a config for the link.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *session == "" || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	res, cfg, err := calibrateSession(*session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if *out == "" {
		if err := writeCalConfig(os.Stdout, *session, cfg, res, res.propose(cfg)); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	err = writeCalConfig(f, *session, cfg, res, res.propose(cfg))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", *out, err)
		return 1
	}
	return 0
}
