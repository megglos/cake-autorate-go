package main

import (
	"context"
	"testing"
	"time"
)

// testLogger returns a silent logger for tests (all output discarded).
func testLogger(t *testing.T) *Logger {
	t.Helper()
	l := NewDiscardLogger()
	t.Cleanup(func() { l.Close() })
	return l
}

// testConfig returns a minimal valid config for testing.
func testConfig() *Config {
	cfg := DefaultConfig()
	cfg.Links = []LinkConfig{{
		Name: "test",
		Download: DirectionConfig{
			Interface:                  "lo",
			Adjust:                     false,
			MinRateKbps:                1000,
			BaseRateKbps:               10000,
			MaxRateKbps:                50000,
			OWDDeltaDelayThrMs:         30.0,
			AvgOWDDeltaMaxAdjUpThrMs:   10.0,
			AvgOWDDeltaMaxAdjDownThrMs: 60.0,
		},
		Upload: DirectionConfig{
			Interface:                  "lo",
			Adjust:                     false,
			MinRateKbps:                1000,
			BaseRateKbps:               10000,
			MaxRateKbps:                50000,
			OWDDeltaDelayThrMs:         30.0,
			AvgOWDDeltaMaxAdjUpThrMs:   10.0,
			AvgOWDDeltaMaxAdjDownThrMs: 60.0,
		},
		Reflectors: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "208.67.222.222"},
	}}
	return cfg
}

// newTestController creates a LinkController suitable for unit tests.
// The shaper has Adjust=false so no real tc/netlink calls occur.
func newTestController(t *testing.T) *LinkController {
	t.Helper()
	cfg := testConfig()
	logger := testLogger(t)
	shaper := NewShaper(logger)
	t.Cleanup(func() { shaper.Close() })
	return NewLinkController(&cfg.Links[0], cfg, shaper, logger)
}

func TestHandlePingResult_SuccessUpdatesLastPingTime(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning

	// Set up baseline so handlePingResult doesn't reject the result
	c.pingerMgr.UpdateBaseline("1.1.1.1", 10000)

	ts := time.Now()
	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		RTT:       10 * time.Millisecond,
		Timestamp: ts,
		Timeout:   false,
	})

	if c.lastPingTime != ts {
		t.Errorf("expected lastPingTime=%v, got %v", ts, c.lastPingTime)
	}
}

func TestHandlePingResult_TimeoutDoesNotUpdateLastPingTime(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning

	// Set a known lastPingTime
	knownTime := time.Now().Add(-5 * time.Second)
	c.lastPingTime = knownTime

	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.lastPingTime != knownTime {
		t.Errorf("timeout should not update lastPingTime: expected %v, got %v", knownTime, c.lastPingTime)
	}
}

func TestHandlePingResult_IgnoredWhenNotRunning(t *testing.T) {
	c := newTestController(t)
	c.state = StateIdle
	before := c.lastPingTime

	ts := time.Now().Add(time.Second)
	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		RTT:       10 * time.Millisecond,
		Timestamp: ts,
		Timeout:   false,
	})

	if c.lastPingTime != before {
		t.Error("ping result should be ignored when not in StateRunning")
	}
}

func TestHandleTimeout_TriggersStallWhenNoRecentPings(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001 // 1ms for fast test
	c.cfg.StallDetectionThr = 3

	// Set lastPingTime far in the past
	c.lastPingTime = time.Now().Add(-10 * time.Second)

	// Need StallDetectionThr consecutive timeouts to trigger stall
	for i := 0; i < 2; i++ {
		c.handlePingResult(PingResult{
			Reflector: "1.1.1.1",
			Timestamp: time.Now(),
			Timeout:   true,
		})
	}
	if c.state != StateRunning {
		t.Error("should not stall before reaching StallDetectionThr")
	}

	// One more timeout reaches the threshold
	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})
	if c.state != StateStall {
		t.Errorf("expected StateStall after %d timeouts, got %s", c.cfg.StallDetectionThr, c.state)
	}
}

func TestHandleTimeout_ConsecutiveCounterResetOnSuccess(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001
	c.cfg.StallDetectionThr = 3
	c.lastPingTime = time.Now().Add(-10 * time.Second)
	c.pingerMgr.UpdateBaseline("1.1.1.1", 10000)

	// 2 timeouts, then a success, then 2 more timeouts → should NOT stall
	for i := 0; i < 2; i++ {
		c.handlePingResult(PingResult{Reflector: "1.1.1.1", Timestamp: time.Now(), Timeout: true})
	}
	c.handlePingResult(PingResult{Reflector: "1.1.1.1", RTT: 10 * time.Millisecond, Timestamp: time.Now(), Timeout: false})
	c.lastPingTime = time.Now().Add(-10 * time.Second) // reset for stall check
	for i := 0; i < 2; i++ {
		c.handlePingResult(PingResult{Reflector: "1.1.1.1", Timestamp: time.Now(), Timeout: true})
	}

	if c.state != StateRunning {
		t.Error("success should reset consecutive timeout counter, preventing stall")
	}
}

func TestHandleTimeout_NoStallWhenRecentPingsExist(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 10.0

	// Recent lastPingTime
	c.lastPingTime = time.Now()

	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning (recent ping exists), got %s", c.state)
	}
}

func TestHandleRateStats_IdleTransition(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001 // 1ms for fast test

	ctx := context.Background()

	// First call sets idleSince
	c.handleRateStats(ctx, RateStats{
		DlRateKbps: 100,
		UlRateKbps: 100,
		Timestamp:  time.Now().Add(-1 * time.Second),
	})
	if c.state != StateRunning {
		t.Error("should still be running after first idle sample")
	}

	// Second call with enough elapsed time triggers idle
	c.handleRateStats(ctx, RateStats{
		DlRateKbps: 100,
		UlRateKbps: 100,
		Timestamp:  time.Now(),
	})
	if c.state != StateIdle {
		t.Errorf("expected StateIdle, got %s", c.state)
	}
}

func TestHandleRateStats_IdleToRunning(t *testing.T) {
	c := newTestController(t)
	c.state = StateIdle
	c.cfg.ConnectionActiveThrKbps = 2000

	ctx := context.Background()
	c.handleRateStats(ctx, RateStats{
		DlRateKbps: 5000,
		UlRateKbps: 0,
		Timestamp:  time.Now(),
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning on traffic resume, got %s", c.state)
	}
}

func TestHandleRateStats_StallToRunning(t *testing.T) {
	c := newTestController(t)
	c.state = StateStall
	c.cfg.ConnectionStallThrKbps = 10

	ctx := context.Background()
	c.handleRateStats(ctx, RateStats{
		DlRateKbps: 100,
		UlRateKbps: 0,
		Timestamp:  time.Now(),
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning on traffic resume from stall, got %s", c.state)
	}
}

func TestProcessDelay_BufferbloatDetection(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 2

	// Re-init direction state with new window size
	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		delayWindow:    make([]bool, 4),
	}

	// Feed enough high-delta samples to trigger bufferbloat detection
	for i := 0; i < 3; i++ {
		c.processDelay(DL, 100000) // 100ms delta — way above threshold
	}

	if !c.dl.bbDetected {
		t.Error("expected bufferbloat detection after exceeding threshold count")
	}
}

func TestProcessDelay_NoFalsePositive(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.BufferbloatDetectionWindow = 6
	c.cfg.BufferbloatDetectionThr = 3

	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		delayWindow:    make([]bool, 6),
	}

	// Single high-delta sample shouldn't trigger
	c.processDelay(DL, 100000)
	if c.dl.bbDetected {
		t.Error("single high-delta sample should not trigger bufferbloat detection")
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		v, min, max, want float64
	}{
		{5, 0, 10, 5},
		{-1, 0, 10, 0},
		{15, 0, 10, 10},
		{0, 0, 0, 0},
	}
	for _, tc := range tests {
		got := clamp(tc.v, tc.min, tc.max)
		if got != tc.want {
			t.Errorf("clamp(%v, %v, %v) = %v, want %v", tc.v, tc.min, tc.max, got, tc.want)
		}
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateRunning, "RUNNING"},
		{StateIdle, "IDLE"},
		{StateStall, "STALL"},
		{State(99), "UNKNOWN"},
	}
	for _, tc := range tests {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// --- Rate adjustment algorithm tests ---

func TestAdjustRateBufferbloat_ReducesRate(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = 40000

	// deltaUs well above threshold → should reduce rate
	thresholdUs := 30.0 * 1000 // 30ms
	deltaUs := 80.0 * 1000     // 80ms — significantly above threshold
	c.adjustRateBufferbloat(DL, deltaUs, thresholdUs)

	if c.dl.shaperRateKbps >= 40000 {
		t.Errorf("expected rate reduction from 40000, got %.0f", c.dl.shaperRateKbps)
	}
	if c.dl.shaperRateKbps < float64(c.link.Download.MinRateKbps) {
		t.Errorf("rate should not drop below min: %.0f < %d", c.dl.shaperRateKbps, c.link.Download.MinRateKbps)
	}
}

func TestAdjustRateBufferbloat_ClampsToMin(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps) + 1

	// Extreme delta to force maximum reduction
	c.adjustRateBufferbloat(DL, 1000000, 1000)

	if c.dl.shaperRateKbps < float64(c.link.Download.MinRateKbps) {
		t.Errorf("rate clamped below min: %.0f < %d", c.dl.shaperRateKbps, c.link.Download.MinRateKbps)
	}
}

func TestAdjustRateHighLoad_IncreasesRate(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = 20000
	c.dl.loadPercent = 0.9

	// deltaUs below threshold → plenty of headroom to increase
	thresholdUs := 30.0 * 1000
	deltaUs := 1.0 * 1000 // 1ms — very low delay
	c.adjustRateHighLoad(DL, deltaUs, thresholdUs)

	if c.dl.shaperRateKbps <= 20000 {
		t.Errorf("expected rate increase from 20000, got %.0f", c.dl.shaperRateKbps)
	}
}

func TestAdjustRateHighLoad_ClampsToMax(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)

	c.adjustRateHighLoad(DL, 0, 30000)

	if c.dl.shaperRateKbps > float64(c.link.Download.MaxRateKbps) {
		t.Errorf("rate should not exceed max: %.0f > %d", c.dl.shaperRateKbps, c.link.Download.MaxRateKbps)
	}
}

func TestAdjustRateHighLoad_NoChangeAtMaxRate(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)

	beforeRate := c.dl.shaperRateKbps
	c.adjustRateHighLoad(DL, 0, 30000)

	if c.dl.shaperRateKbps != beforeRate {
		t.Errorf("rate should not change when already at max: %.0f != %.0f", c.dl.shaperRateKbps, beforeRate)
	}
}

func TestAdjustRateDecay_TowardBaseFromAbove(t *testing.T) {
	c := newTestController(t)
	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl.shaperRateKbps = baseRate * 2 // well above base

	c.adjustRateDecay(DL)

	if c.dl.shaperRateKbps >= baseRate*2 {
		t.Errorf("expected decay from %.0f toward base %.0f, got %.0f", baseRate*2, baseRate, c.dl.shaperRateKbps)
	}
	if c.dl.shaperRateKbps < baseRate {
		t.Errorf("decay should not undershoot base rate: %.0f < %.0f", c.dl.shaperRateKbps, baseRate)
	}
}

func TestAdjustRateDecay_TowardBaseFromBelow(t *testing.T) {
	c := newTestController(t)
	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl.shaperRateKbps = baseRate / 2 // well below base

	c.adjustRateDecay(DL)

	if c.dl.shaperRateKbps <= baseRate/2 {
		t.Errorf("expected increase from %.0f toward base %.0f, got %.0f", baseRate/2, baseRate, c.dl.shaperRateKbps)
	}
	if c.dl.shaperRateKbps > baseRate {
		t.Errorf("decay should not overshoot base rate: %.0f > %.0f", c.dl.shaperRateKbps, baseRate)
	}
}

func TestAdjustRateDecay_NoChangeAtBase(t *testing.T) {
	c := newTestController(t)
	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl.shaperRateKbps = baseRate

	c.adjustRateDecay(DL)

	if c.dl.shaperRateKbps != baseRate {
		t.Errorf("no decay expected at base rate: %.0f != %.0f", c.dl.shaperRateKbps, baseRate)
	}
}

func TestProcessDelay_HighLoadIncrease(t *testing.T) {
	c := newTestController(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.BufferbloatRefractoryPeriodMs = 0

	c.dl = dirState{
		shaperRateKbps: 20000,
		loadPercent:    0.9, // high load
		delayWindow:    make([]bool, 4),
	}

	// Low delta with high load → should increase rate
	c.processDelay(DL, 100) // very low delay

	if c.dl.shaperRateKbps <= 20000 {
		t.Errorf("expected rate increase under high load with low delay, got %.0f", c.dl.shaperRateKbps)
	}
}

func TestProcessDelay_LowLoadDecay(t *testing.T) {
	c := newTestController(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.DecayRefractoryPeriodMs = 0

	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl = dirState{
		shaperRateKbps: baseRate * 2, // above base
		loadPercent:    0.1,          // low load
		delayWindow:    make([]bool, 4),
	}

	c.processDelay(DL, 100) // low delay, low load → decay

	if c.dl.shaperRateKbps >= baseRate*2 {
		t.Errorf("expected decay toward base rate, got %.0f", c.dl.shaperRateKbps)
	}
}

func TestHandleTimeout_MinShaperRatesEnforcement(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001
	c.cfg.StallDetectionThr = 1 // trigger on first timeout
	c.cfg.MinShaperRatesEnforcement = true
	c.lastPingTime = time.Now().Add(-10 * time.Second)

	c.dl.shaperRateKbps = 40000
	c.ul.shaperRateKbps = 30000

	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.state != StateStall {
		t.Fatalf("expected StateStall, got %s", c.state)
	}
	if c.dl.shaperRateKbps != float64(c.link.Download.MinRateKbps) {
		t.Errorf("dl rate should be min on stall with enforcement: got %.0f, want %d",
			c.dl.shaperRateKbps, c.link.Download.MinRateKbps)
	}
	if c.ul.shaperRateKbps != float64(c.link.Upload.MinRateKbps) {
		t.Errorf("ul rate should be min on stall with enforcement: got %.0f, want %d",
			c.ul.shaperRateKbps, c.link.Upload.MinRateKbps)
	}
}

func TestHandleRateStats_IdleWithMinShaperEnforcement(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001
	c.cfg.MinShaperRatesEnforcement = true

	c.dl.shaperRateKbps = 40000
	c.ul.shaperRateKbps = 30000

	ctx := context.Background()

	// First call sets idleSince
	c.handleRateStats(ctx, RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now().Add(-1 * time.Second)})
	// Second call triggers idle
	c.handleRateStats(ctx, RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now()})

	if c.state != StateIdle {
		t.Fatalf("expected StateIdle, got %s", c.state)
	}
	if c.dl.shaperRateKbps != float64(c.link.Download.MinRateKbps) {
		t.Errorf("dl rate should be min on idle with enforcement: got %.0f, want %d",
			c.dl.shaperRateKbps, c.link.Download.MinRateKbps)
	}
	if c.ul.shaperRateKbps != float64(c.link.Upload.MinRateKbps) {
		t.Errorf("ul rate should be min on idle with enforcement: got %.0f, want %d",
			c.ul.shaperRateKbps, c.link.Upload.MinRateKbps)
	}
}

func TestHandleRateStats_IdleToRunningResetsToBase(t *testing.T) {
	c := newTestController(t)
	c.state = StateIdle
	c.cfg.ConnectionActiveThrKbps = 2000

	c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps)
	c.ul.shaperRateKbps = float64(c.link.Upload.MinRateKbps)

	ctx := context.Background()
	c.handleRateStats(ctx, RateStats{DlRateKbps: 5000, UlRateKbps: 0, Timestamp: time.Now()})

	if c.dl.shaperRateKbps != float64(c.link.Download.BaseRateKbps) {
		t.Errorf("dl rate should reset to base on resume: got %.0f, want %d",
			c.dl.shaperRateKbps, c.link.Download.BaseRateKbps)
	}
	if c.ul.shaperRateKbps != float64(c.link.Upload.BaseRateKbps) {
		t.Errorf("ul rate should reset to base on resume: got %.0f, want %d",
			c.ul.shaperRateKbps, c.link.Upload.BaseRateKbps)
	}
}

func TestHandleRateStats_IdleResetOnTraffic(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 60 // long wait

	ctx := context.Background()

	// Start idle timer
	c.handleRateStats(ctx, RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now()})
	if c.idleSince.IsZero() {
		t.Fatal("idleSince should be set after low-traffic sample")
	}

	// Traffic spike resets idle timer
	c.handleRateStats(ctx, RateStats{DlRateKbps: 5000, UlRateKbps: 0, Timestamp: time.Now()})
	if !c.idleSince.IsZero() {
		t.Error("idleSince should be reset when traffic exceeds threshold")
	}
}

func TestProcessDelay_SlidingWindowCorrectness(t *testing.T) {
	c := newTestController(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.DecayRefractoryPeriodMs = 0

	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		delayWindow:    make([]bool, 4),
		loadPercent:    0.1,
	}

	// Fill window with 2 high + 2 low → no BB (2 < 3)
	c.processDelay(DL, 100000) // high
	c.processDelay(DL, 100000) // high
	c.processDelay(DL, 0)      // low
	c.processDelay(DL, 0)      // low

	if c.dl.bbDetected {
		t.Error("2/4 exceeded should not trigger BB detection (thr=3)")
	}
	if c.dl.delayWindowCount != 2 {
		t.Errorf("expected delayWindowCount=2, got %d", c.dl.delayWindowCount)
	}

	// Now wrap: push 2 more highs, evicting the 2 lows won't help but evicting old entries will
	// Window was: [high, high, low, low] → next writes at idx 0,1 (wrapping)
	c.processDelay(DL, 100000) // replaces high→high at [0]: count stays 2... wait let me think
	// Actually idx advanced to 4 which wraps to 0. So:
	// After 4 entries: idx=0, window=[high, high, low, low]
	// 5th entry at idx=0: was high, now high → no change → count=2
	// 6th entry at idx=1: was high, now high → no change → count=2
	// We need to push high entries where lows were
	c.processDelay(DL, 100000) // at idx=1: was high→high, count still 2

	// Push high to replace the lows at idx 2,3
	c.processDelay(DL, 100000) // at idx=2: was low→high, count=3 → BB!

	if !c.dl.bbDetected {
		t.Error("expected BB detection after filling window with highs")
	}
}

func TestLoadPercent_ComputedFromRateStats(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.dl.shaperRateKbps = 10000
	c.ul.shaperRateKbps = 5000

	ctx := context.Background()
	c.handleRateStats(ctx, RateStats{
		DlRateKbps: 8000,
		UlRateKbps: 4000,
		Timestamp:  time.Now(),
	})

	if c.dl.loadPercent != 0.8 {
		t.Errorf("expected dl loadPercent=0.8, got %f", c.dl.loadPercent)
	}
	if c.ul.loadPercent != 0.8 {
		t.Errorf("expected ul loadPercent=0.8, got %f", c.ul.loadPercent)
	}
}

func TestGetDirState(t *testing.T) {
	c := newTestController(t)
	c.dl.shaperRateKbps = 111
	c.ul.shaperRateKbps = 222

	if c.getDirState(DL).shaperRateKbps != 111 {
		t.Error("getDirState(DL) returned wrong state")
	}
	if c.getDirState(UL).shaperRateKbps != 222 {
		t.Error("getDirState(UL) returned wrong state")
	}
}
