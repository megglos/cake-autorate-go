// SPDX-License-Identifier: GPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// goodProbe is a reflector that replies to all requests with one-way
// delays and the given jitter.
func goodProbe(addr string, jitterMs float64) tsProbe {
	return tsProbe{Addr: addr, Sent: 40, Replies: 40, OWD: 40, JitterP95Ms: jitterMs}
}

func TestProposeFromProbe(t *testing.T) {
	probes := []tsProbe{
		goodProbe("a", 3),
		goodProbe("b", 1),
		{Addr: "echo-only", Sent: 40, Replies: 40}, // no one-way delays
		{Addr: "lossy", Sent: 40, Replies: 30, OWD: 30, JitterP95Ms: 1}, // 75 % replies
		{Addr: "few", Sent: 10, Replies: 10, OWD: 10, JitterP95Ms: 1},   // too few samples
		goodProbe("c", 2),
		goodProbe("noisy", 20), // more than 2 x the median jitter
		goodProbe("d", 4),
		{Addr: "silent", Sent: 40},
		// One third of the replies have standard times: the others give
		// RTT/2 in the program.
		{Addr: "mixed", Sent: 60, Replies: 60, OWD: 20, JitterP95Ms: 1},
	}
	method, refl, why := proposeFromProbe(probes, 2, 14*time.Second)
	if method != PingMethodTimestamp || !slices.Equal(refl, []string{"b", "c", "a", "d"}) {
		t.Errorf("method %q, reflectors %v", method, refl)
	}
	if !strings.Contains(why, "probe of 14 s: 4 of 10 reflectors") {
		t.Errorf("why: %s", why)
	}

	// pinger_count 6: the list has up to 8 reflectors, here all 4.
	if _, refl, _ := proposeFromProbe(probes, 6, 14*time.Second); len(refl) != 4 {
		t.Errorf("reflectors with pinger_count 6: %v", refl)
	}

	// Three reflectors with ICMP timestamps are not sufficient.
	method, refl, why = proposeFromProbe(probes[:3], 2, 14*time.Second)
	if method != PingMethodEcho || refl != nil || !strings.Contains(why, "2 of 3 reflectors reply to ICMP timestamps") {
		t.Errorf("method %q, reflectors %v, why %s", method, refl, why)
	}
}

func TestProbeCandidates(t *testing.T) {
	link := &LinkConfig{Reflectors: []string{"1.1.1.1", defaultTimestampReflectors[0]}}
	got := probeCandidates(link)
	want := append([]string{"1.1.1.1"}, defaultTimestampReflectors...)
	if !slices.Equal(got, want) {
		t.Errorf("candidates: %v", got)
	}
	link.Reflectors = nil
	for i := range 20 {
		link.Reflectors = append(link.Reflectors, string(rune('a'+i)))
	}
	if n := len(probeCandidates(link)); n != calProbeMaxReflectors {
		t.Errorf("%d candidates", n)
	}
}

// With ICMP echo, the recordings have no one-way delays. The probe tests
// ICMP timestamps in the idle time of the first block, and its result
// selects the ping method and the reflectors.
func TestCalibrationProbe(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, _ := newTestCalibrator(t, times)
	target.cfg.PingMethod = PingMethodEcho
	target.cfg.PingerCount = 2
	var phases []string
	k.probe = func(ctx context.Context, cfg *Config, _ *Logger) ([]tsProbe, error) {
		st := k.Status()
		phases = append(phases, fmt.Sprintf("%d %s %s", st.Block, st.Mode, st.Policy))
		if cfg.Links[0].Name != "wan" {
			t.Errorf("probe config of link %q", cfg.Links[0].Name)
		}
		return []tsProbe{goodProbe("10.0.0.1", 1), goodProbe("t1", 2), goodProbe("t2", 3), goodProbe("t3", 4),
			{Addr: "10.0.0.2", Sent: 40}}, nil
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	st := k.Status()
	if st.State != "done" {
		t.Fatalf("status: %+v", st)
	}
	// The probe runs one time, in the idle phase of the first block.
	if !slices.Equal(phases, []string{"1 dl unshaped"}) {
		t.Errorf("probe phases: %v", phases)
	}
	r := st.Result
	if r.PingMethod != PingMethodTimestamp || !slices.Equal(r.Reflectors, []string{"10.0.0.1", "t1", "t2", "t3"}) {
		t.Errorf("ping method %q, reflectors %v", r.PingMethod, r.Reflectors)
	}
	if !strings.Contains(r.Why["reflectors"], "probe of") {
		t.Errorf("why: %v", r.Why)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "calibrate again") {
		t.Errorf("notes: %v", r.Notes)
	}
}

// A probe without ICMP timestamps proposes ICMP echo, with the probe as
// the reason.
func TestCalibrationProbeNoTimestamps(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, _ := newTestCalibrator(t, times)
	target.cfg.PingMethod = PingMethodTimestamp
	k.probe = func(context.Context, *Config, *Logger) ([]tsProbe, error) {
		return []tsProbe{{Addr: "10.0.0.1", Sent: 40}, {Addr: "10.0.0.2", Sent: 40, Replies: 40}}, nil
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	r := k.Status().Result
	if r == nil || r.PingMethod != PingMethodEcho || len(r.Reflectors) != 0 {
		t.Fatalf("result: %+v", r)
	}
	if !strings.Contains(r.Why["ping_method"], "0 of 2 reflectors reply to ICMP timestamps") {
		t.Errorf("why: %v", r.Why)
	}
}

// If the probe cannot run, the analysis of the recordings selects the ping
// method, and a note tells why.
func TestCalibrationProbeError(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, _, _ := newTestCalibrator(t, times)
	k.probe = func(context.Context, *Config, *Logger) ([]tsProbe, error) {
		return nil, errors.New("operation not permitted")
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	r := k.Status().Result
	if r == nil || r.PingMethod != PingMethodTimestamp || len(r.Reflectors) != 4 {
		t.Fatalf("result: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "probe did not run: operation not permitted") {
		t.Errorf("notes: %v", r.Notes)
	}
}

// A stop in the idle time of the first block also stops the probe.
func TestCalibrationStopDuringProbe(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, _, _ := newTestCalibrator(t, times)
	k.tick = time.Millisecond
	k.now = time.Now // the idle phase lasts 15 s of real time
	started := make(chan struct{})
	k.probe = func(ctx context.Context, _ *Config, _ *Logger) ([]tsProbe, error) {
		close(started)
		<-ctx.Done()
		return nil, nil
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	<-started
	done := make(chan error)
	go func() { done <- k.Stop("") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop waits for the probe")
	}
	if st := k.Status(); st.State != "stopped" {
		t.Errorf("status: %+v", st)
	}
}

// The probe has its own interval: with ping_interval_ms 1000, the idle
// time of a block still gives enough requests to each reflector.
func TestProbeIntervalIndependentOfPingInterval(t *testing.T) {
	if n := int(defaultCalTimes(calMinLoad).Idle / calProbeInterval); n < calMinIdleSamples+calMinIdleSamples/2 {
		t.Errorf("%d requests in the idle time, want more than %d", n, calMinIdleSamples)
	}
}

// A probe with too few requests (for example, it stopped early) has no
// result: the recordings select the ping method.
func TestCalibrationProbeTooFewRequests(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, _, _ := newTestCalibrator(t, times)
	k.probe = func(context.Context, *Config, *Logger) ([]tsProbe, error) {
		return []tsProbe{{Addr: "10.0.0.1", Sent: 15, Replies: 15, OWD: 15}}, nil
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	r := k.Status().Result
	if r == nil || r.PingMethod != PingMethodTimestamp || len(r.Reflectors) != 4 {
		t.Fatalf("result: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "probe did not run: fewer than 20 requests to a reflector") {
		t.Errorf("notes: %v", r.Notes)
	}
}

// The end of the context also ends the DNS lookups of the probe: a stop
// does not wait for a slow DNS server.
func TestProbeStopsDuringDNSLookup(t *testing.T) {
	old := resolveIPv4
	t.Cleanup(func() { resolveIPv4 = old })
	resolveIPv4 = func(ctx context.Context, host string) (*net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links = cfg.Links[:1]
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := probeTimestamps(ctx, cfg, NewDiscardLogger())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a probe without addresses must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe waits for the DNS lookups after the end of the context")
	}
}

// The jitter is the p95 of the larger direction of each reply. Spikes in
// different replies of the two directions count: 4 of 60 replies with a
// spike are more than 5 %.
func TestPairedJitterP95(t *testing.T) {
	ul, dl := make([]float64, 60), make([]float64, 60)
	for i := range ul {
		ul[i], dl[i] = 10, 20 // the clock offset is in the minimum
	}
	ul[5], ul[15] = 110, 110
	dl[25], dl[35] = 120, 120
	if got := pairedJitterP95(ul, dl); got != 100 {
		t.Errorf("jitter %v ms, want 100", got)
	}
	if got := pairedJitterP95(nil, nil); got != 0 {
		t.Errorf("jitter without samples: %v", got)
	}
}

// Each reflector with requests needs calMinIdleSamples of them: if some
// pingers started late (slow DNS), the probe has no result. A reflector
// without requests (no address) does not count.
func TestCalProbeEnough(t *testing.T) {
	for _, tc := range []struct {
		sent []int
		want bool
	}{
		{[]int{40, 40}, true},
		{[]int{40, 0}, true},
		{[]int{40, 15, 15}, false},
		{[]int{15}, false},
		{[]int{0, 0}, false},
	} {
		p := &calProbe{}
		for i, n := range tc.sent {
			p.Reflectors = append(p.Reflectors, tsProbe{Addr: fmt.Sprint(i), Sent: n})
		}
		if got := p.enough(); got != tc.want {
			t.Errorf("sent %v: enough %v, want %v", tc.sent, got, tc.want)
		}
	}
}

// With a long deadline, the lost requests have no timeout result at the
// end of the probe. They count as lost: a reflector with replies to half
// of its requests is not selected.
func TestSummarizeProbeCountsLostRequests(t *testing.T) {
	var results []PingResult
	for i := range 30 {
		results = append(results, PingResult{Reflector: "a", OWD: true,
			UlOWD: time.Duration(10+i%3) * time.Millisecond, DlOWD: 10 * time.Millisecond})
	}
	probes := summarizeProbe([]string{"a", "b"}, map[string]int{"a": 60}, results)
	if probes[0].Sent != 60 || probes[0].Replies != 30 || probes[0].OWD != 30 {
		t.Errorf("a: %+v", probes[0])
	}
	if probes[1].Sent != 0 {
		t.Errorf("b: %+v", probes[1])
	}
	if _, refl, _ := proposeFromProbe(append(probes, goodProbe("c", 1), goodProbe("d", 1), goodProbe("e", 1)), 2, 15*time.Second); slices.Contains(refl, "a") {
		t.Errorf("a reflector with 50 %% loss is selected: %v", refl)
	}
}
