// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"
)

// The ICMP timestamp probe of a calibration session. The session records
// the pings of the link with the configured ping method. With ICMP echo,
// the recordings do not show if reflectors reply to ICMP timestamps. Thus,
// in the idle time of the first block, the probe sends ICMP timestamp
// requests to the configured reflectors and to the default reflectors for
// ICMP timestamps. The proposal of the ping method and the reflectors then
// uses the result of the probe.

// calProbeMaxReflectors is the maximum number of reflectors in the probe.
const calProbeMaxReflectors = 16

// calProbeInterval is the time between two requests to a reflector. It
// does not come from ping_interval_ms: with 1 s, the idle time of a block
// (15 s) gives fewer than calMinIdleSamples replies.
const calProbeInterval = 250 * time.Millisecond

// tsProbe is the result of the probe of one reflector.
type tsProbe struct {
	Addr    string
	Sent    int // requests with a reply or a timeout
	Replies int
	// OWD is the number of replies with standard times (one-way delays).
	OWD int
	// JitterP95Ms is the p95 of the one-way delay above its minimum, in
	// the direction with the larger value.
	JitterP95Ms float64
}

// calProbe is the result of the probe of a session.
type calProbe struct {
	Reflectors []tsProbe
	Duration   time.Duration
	// Err is the error if the probe did not run.
	Err error
}

// probeFunc runs the probe for the link of cfg until ctx ends.
type probeFunc func(ctx context.Context, cfg *Config, logger *Logger) ([]tsProbe, error)

// probeCandidates returns the reflectors for the probe: the configured
// reflectors of the link, then the default reflectors for ICMP timestamps.
func probeCandidates(link *LinkConfig) []string {
	var out []string
	for _, r := range append(slices.Clone(link.Reflectors), defaultTimestampReflectors...) {
		if !slices.Contains(out, r) && len(out) < calProbeMaxReflectors {
			out = append(out, r)
		}
	}
	return out
}

// probeTimestamps sends ICMP timestamp requests to each candidate reflector
// until ctx ends. It returns an error only if no reflector got a request
// (for example, the program cannot open a raw socket).
func probeTimestamps(ctx context.Context, cfg *Config, logger *Logger) ([]tsProbe, error) {
	link := &cfg.Links[0]
	candidates := probeCandidates(link)
	if len(candidates) == 0 {
		return nil, errors.New("no reflectors")
	}
	interval := calProbeInterval
	deadline := time.Duration(cfg.ReflectorResponseDeadlineS * float64(time.Second))
	if deadline <= 0 {
		deadline = time.Second
	}
	pm := NewPingerManager(link, cfg, logger)

	type samples struct {
		sent, replies int
		ul, dl        []float64
	}
	data := make(map[string]*samples, len(candidates))
	for _, r := range candidates {
		data[r] = &samples{}
	}
	resultCh := make(chan PingResult, 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, r := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pm.runTimestampPinger(ctx, r, interval, deadline, resultCh); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", r, err))
				mu.Unlock()
			}
		}()
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
	for res := range resultCh {
		s := data[res.Reflector]
		s.sent++
		if res.Timeout {
			continue
		}
		s.replies++
		if res.OWD {
			s.ul = append(s.ul, float64(res.UlOWD)/float64(time.Millisecond))
			s.dl = append(s.dl, float64(res.DlOWD)/float64(time.Millisecond))
		}
	}
	if len(errs) == len(candidates) {
		return nil, errs[0]
	}
	for _, err := range errs {
		logger.Debugf("timestamp probe: %v", err)
	}
	out := make([]tsProbe, 0, len(candidates))
	for _, r := range candidates {
		s := data[r]
		out = append(out, tsProbe{Addr: r, Sent: s.sent, Replies: s.replies, OWD: len(s.ul),
			JitterP95Ms: pairedJitterP95(s.ul, s.dl)})
	}
	return out, nil
}

// pairedJitterP95 returns the jitter of a reflector: for each reply, the
// larger delay above the minimum of the two directions (the minimum
// removes the clock offset), then the p95 of these values. ul[i] and dl[i]
// are of the same reply. This is the rule of the analysis of the
// recordings.
func pairedJitterP95(ul, dl []float64) float64 {
	if len(ul) == 0 || len(ul) != len(dl) {
		return 0
	}
	minUl, minDl := slices.Min(ul), slices.Min(dl)
	d := make([]float64, len(ul))
	for i := range ul {
		d[i] = math.Max(ul[i]-minUl, dl[i]-minDl)
	}
	return newCalDist(d).P95
}

// enough reports whether the probe sent sufficient requests to each
// reflector for a result (calMinIdleSamples). A reflector that got no
// request (its address did not resolve) does not count. A probe that
// stopped early, or that started some pingers late (slow DNS), has no
// result: then a reflector can fail only for too few requests.
func (p *calProbe) enough() bool {
	n := 0
	for _, r := range p.Reflectors {
		if r.Sent == 0 {
			continue
		}
		if r.Sent < calMinIdleSamples {
			return false
		}
		n++
	}
	return n > 0
}

// proposeFromProbe returns the ping method and the reflectors from the
// result of the probe, with the same rules as the analysis of a session:
// a reflector needs calMinIdleSamples replies with one-way delays and
// calMinReplyRatio replies to requests, and not more than
// calReflectorMaxJitter times the median jitter. ICMP timestamps need
// calMinReflectors of these reflectors. The list has the reflectors with
// the lowest jitter, pinger_count plus calSpareReflectors. For ICMP echo,
// the list is empty: the configured list stays.
func proposeFromProbe(probes []tsProbe, pingerCount int, dur time.Duration) (method string, reflectors []string, why string) {
	var good []tsProbe
	for _, p := range probes {
		// Only replies with one-way delays count: the program uses RTT/2
		// for the others.
		if p.OWD >= calMinIdleSamples && p.Sent > 0 && float64(p.OWD) >= calMinReplyRatio*float64(p.Sent) {
			good = append(good, p)
		}
	}
	if len(good) > 0 {
		j := make([]float64, len(good))
		for i, p := range good {
			j[i] = p.JitterP95Ms
		}
		limit := calReflectorMaxJitter * newCalDist(j).P50
		good = slices.DeleteFunc(good, func(p tsProbe) bool { return p.JitterP95Ms > limit })
	}
	sort.SliceStable(good, func(a, b int) bool { return good[a].JitterP95Ms < good[b].JitterP95Ms })
	secs := int(math.Round(dur.Seconds()))
	if len(good) < calMinReflectors {
		return PingMethodEcho, nil, fmt.Sprintf("probe of %d s: %d of %d reflectors reply to ICMP timestamps with a low jitter (need %d)",
			secs, len(good), len(probes), calMinReflectors)
	}
	n := min(len(good), max(pingerCount+calSpareReflectors, calMinReflectors))
	for _, p := range good[:n] {
		reflectors = append(reflectors, p.Addr)
	}
	return PingMethodTimestamp, reflectors, fmt.Sprintf("probe of %d s: %d of %d reflectors reply to ICMP timestamps with a low jitter; the %d with the lowest jitter (the program selects %d of them at random)",
		secs, len(good), len(probes), n, min(pingerCount, n))
}
