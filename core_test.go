package main

import (
	"slices"
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

// fakeActuator records the decisions of Core.
type fakeActuator struct {
	rates       map[Direction][]int
	starts      int
	stops       int
	invalidates int
}

func (f *fakeActuator) SetRate(dir Direction, rateKbps int) {
	if f.rates == nil {
		f.rates = make(map[Direction][]int)
	}
	f.rates[dir] = append(f.rates[dir], rateKbps)
}
func (f *fakeActuator) StartPingers()     { f.starts++ }
func (f *fakeActuator) StopPingers()      { f.stops++ }
func (f *fakeActuator) InvalidateShaper() { f.invalidates++ }

// newTestCore creates a Core with a fakeActuator.
// testConfig sets Adjust=false. Thus, Core calls SetRate only when a test sets Adjust=true.
func newTestCore(t *testing.T) (*Core, *fakeActuator) {
	t.Helper()
	cfg := testConfig()
	act := &fakeActuator{}
	c := NewCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, testLogger(t))
	c.Start(time.Now())
	return c, act
}

func TestHandlePingResult_SuccessUpdatesLastPingTime(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning

	// Set up baseline so handlePingResult doesn't reject the result
	c.updateBaseline("1.1.1.1", 10000)

	ts := time.Now()
	c.HandlePing(PingResult{
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
	c, _ := newTestCore(t)
	c.state = StateRunning

	// Set a known lastPingTime
	knownTime := time.Now().Add(-5 * time.Second)
	c.lastPingTime = knownTime

	c.HandlePing(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.lastPingTime != knownTime {
		t.Errorf("timeout should not update lastPingTime: expected %v, got %v", knownTime, c.lastPingTime)
	}
}

func TestHandlePingResult_IgnoredWhenNotRunning(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateIdle
	before := c.lastPingTime

	ts := time.Now().Add(time.Second)
	c.HandlePing(PingResult{
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
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001 // 1ms for fast test
	c.cfg.StallDetectionThr = 3

	// Set lastPingTime far in the past
	c.lastPingTime = time.Now().Add(-10 * time.Second)

	// Need StallDetectionThr consecutive timeouts to trigger stall
	for i := 0; i < 2; i++ {
		c.HandlePing(PingResult{
			Reflector: "1.1.1.1",
			Timestamp: time.Now(),
			Timeout:   true,
		})
	}
	if c.state != StateRunning {
		t.Error("should not stall before reaching StallDetectionThr")
	}

	// One more timeout reaches the threshold
	c.HandlePing(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})
	if c.state != StateStall {
		t.Errorf("expected StateStall after %d timeouts, got %s", c.cfg.StallDetectionThr, c.state)
	}
}

func TestHandleTimeout_ConsecutiveCounterResetOnSuccess(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001
	c.cfg.StallDetectionThr = 3
	c.lastPingTime = time.Now().Add(-10 * time.Second)
	c.updateBaseline("1.1.1.1", 10000)

	// 2 timeouts, then a success, then 2 more timeouts → should NOT stall
	for i := 0; i < 2; i++ {
		c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: time.Now(), Timeout: true})
	}
	c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 10 * time.Millisecond, Timestamp: time.Now(), Timeout: false})
	c.lastPingTime = time.Now().Add(-10 * time.Second) // reset for stall check
	for i := 0; i < 2; i++ {
		c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: time.Now(), Timeout: true})
	}

	if c.state != StateRunning {
		t.Error("success should reset consecutive timeout counter, preventing stall")
	}
}

func TestHandleTimeout_NoStallWhenRecentPingsExist(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 10.0

	// Recent lastPingTime
	c.lastPingTime = time.Now()

	c.HandlePing(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning (recent ping exists), got %s", c.state)
	}
}

func TestHandleRateStats_IdleTransition(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001 // 1ms for fast test

	// First call sets idleSince
	c.HandleRate(RateStats{
		DlRateKbps: 100,
		UlRateKbps: 100,
		Timestamp:  time.Now().Add(-1 * time.Second),
	})
	if c.state != StateRunning {
		t.Error("should still be running after first idle sample")
	}

	// Second call with enough elapsed time triggers idle
	c.HandleRate(RateStats{
		DlRateKbps: 100,
		UlRateKbps: 100,
		Timestamp:  time.Now(),
	})
	if c.state != StateIdle {
		t.Errorf("expected StateIdle, got %s", c.state)
	}
}

func TestHandleRateStats_IdleToRunning(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateIdle
	c.cfg.ConnectionActiveThrKbps = 2000

	c.HandleRate(RateStats{
		DlRateKbps: 5000,
		UlRateKbps: 0,
		Timestamp:  time.Now(),
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning on traffic resume, got %s", c.state)
	}
}

func TestHandleRateStats_StallToRunning(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateStall
	c.cfg.ConnectionStallThrKbps = 10

	c.HandleRate(RateStats{
		DlRateKbps: 100,
		UlRateKbps: 0,
		Timestamp:  time.Now(),
	})

	if c.state != StateRunning {
		t.Errorf("expected StateRunning on traffic resume from stall, got %s", c.state)
	}
}

func TestProcessDelay_BufferbloatDetection(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 2

	// Re-init direction state with new window size
	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		mtuBytes:       defaultMTU,
		delayWindow:    make([]bool, 4),
	}

	// Feed enough high-delta samples to trigger bufferbloat detection
	for i := 0; i < 3; i++ {
		c.processDelay(DL, 100000, time.Now()) // 100ms delta — way above threshold
	}

	if !c.dl.bbDetected {
		t.Error("expected bufferbloat detection after exceeding threshold count")
	}
}

func TestProcessDelay_NoFalsePositive(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.BufferbloatDetectionWindow = 6
	c.cfg.BufferbloatDetectionThr = 3

	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		mtuBytes:       defaultMTU,
		delayWindow:    make([]bool, 6),
	}

	// Single high-delta sample shouldn't trigger
	c.processDelay(DL, 100000, time.Now())
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
	c, _ := newTestCore(t)
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
	c, _ := newTestCore(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps) + 1

	// Extreme delta to force maximum reduction
	c.adjustRateBufferbloat(DL, 1000000, 1000)

	if c.dl.shaperRateKbps < float64(c.link.Download.MinRateKbps) {
		t.Errorf("rate clamped below min: %.0f < %d", c.dl.shaperRateKbps, c.link.Download.MinRateKbps)
	}
}

func TestAdjustRateHighLoad_IncreasesRate(t *testing.T) {
	c, _ := newTestCore(t)
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
	c, _ := newTestCore(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)

	c.adjustRateHighLoad(DL, 0, 30000)

	if c.dl.shaperRateKbps > float64(c.link.Download.MaxRateKbps) {
		t.Errorf("rate should not exceed max: %.0f > %d", c.dl.shaperRateKbps, c.link.Download.MaxRateKbps)
	}
}

func TestAdjustRateHighLoad_NoChangeAtMaxRate(t *testing.T) {
	c, _ := newTestCore(t)
	c.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)

	beforeRate := c.dl.shaperRateKbps
	c.adjustRateHighLoad(DL, 0, 30000)

	if c.dl.shaperRateKbps != beforeRate {
		t.Errorf("rate should not change when already at max: %.0f != %.0f", c.dl.shaperRateKbps, beforeRate)
	}
}

func TestAdjustRateDecay_TowardBaseFromAbove(t *testing.T) {
	c, _ := newTestCore(t)
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
	c, _ := newTestCore(t)
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
	c, _ := newTestCore(t)
	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl.shaperRateKbps = baseRate

	c.adjustRateDecay(DL)

	if c.dl.shaperRateKbps != baseRate {
		t.Errorf("no decay expected at base rate: %.0f != %.0f", c.dl.shaperRateKbps, baseRate)
	}
}

func TestProcessDelay_HighLoadIncrease(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.BufferbloatRefractoryPeriodMs = 0

	c.dl = dirState{
		shaperRateKbps: 20000,
		mtuBytes:       defaultMTU,
		loadPercent:    0.9, // high load
		delayWindow:    make([]bool, 4),
	}

	// Low delta with high load → should increase rate
	c.processDelay(DL, 100, time.Now()) // very low delay

	if c.dl.shaperRateKbps <= 20000 {
		t.Errorf("expected rate increase under high load with low delay, got %.0f", c.dl.shaperRateKbps)
	}
}

func TestProcessDelay_LowLoadDecay(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.DecayRefractoryPeriodMs = 0

	baseRate := float64(c.link.Download.BaseRateKbps)
	c.dl = dirState{
		shaperRateKbps: baseRate * 2, // above base
		mtuBytes:       defaultMTU,
		loadPercent:    0.1, // low load
		delayWindow:    make([]bool, 4),
	}

	c.processDelay(DL, 100, time.Now()) // low delay, low load → decay

	if c.dl.shaperRateKbps >= baseRate*2 {
		t.Errorf("expected decay toward base rate, got %.0f", c.dl.shaperRateKbps)
	}
}

func TestHandleTimeout_MinShaperRatesEnforcement(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001
	c.cfg.StallDetectionThr = 1 // trigger on first timeout
	c.cfg.MinShaperRatesEnforcement = true
	c.lastPingTime = time.Now().Add(-10 * time.Second)

	c.dl.shaperRateKbps = 40000
	c.ul.shaperRateKbps = 30000

	c.HandlePing(PingResult{
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
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001
	c.cfg.MinShaperRatesEnforcement = true

	c.dl.shaperRateKbps = 40000
	c.ul.shaperRateKbps = 30000

	// First call sets idleSince
	c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now().Add(-1 * time.Second)})
	// Second call triggers idle
	c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now()})

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
	c, _ := newTestCore(t)
	c.state = StateIdle
	c.cfg.ConnectionActiveThrKbps = 2000

	c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps)
	c.ul.shaperRateKbps = float64(c.link.Upload.MinRateKbps)

	c.HandleRate(RateStats{DlRateKbps: 5000, UlRateKbps: 0, Timestamp: time.Now()})

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
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 60 // long wait

	// Start idle timer
	c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: time.Now()})
	if c.idleSince.IsZero() {
		t.Fatal("idleSince should be set after low-traffic sample")
	}

	// Traffic spike resets idle timer
	c.HandleRate(RateStats{DlRateKbps: 5000, UlRateKbps: 0, Timestamp: time.Now()})
	if !c.idleSince.IsZero() {
		t.Error("idleSince should be reset when traffic exceeds threshold")
	}
}

func TestProcessDelay_SlidingWindowCorrectness(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 3
	c.cfg.HighLoadThr = 0.75
	c.cfg.DecayRefractoryPeriodMs = 0

	c.dl = dirState{
		shaperRateKbps: float64(c.link.Download.BaseRateKbps),
		mtuBytes:       defaultMTU,
		delayWindow:    make([]bool, 4),
		loadPercent:    0.1,
	}

	// Fill window with 2 high + 2 low → no BB (2 < 3)
	c.processDelay(DL, 100000, time.Now()) // high
	c.processDelay(DL, 100000, time.Now()) // high
	c.processDelay(DL, 0, time.Now())      // low
	c.processDelay(DL, 0, time.Now())      // low

	if c.dl.bbDetected {
		t.Error("2/4 exceeded should not trigger BB detection (thr=3)")
	}
	if c.dl.delayWindowCount != 2 {
		t.Errorf("expected delayWindowCount=2, got %d", c.dl.delayWindowCount)
	}

	// Now wrap: push 2 more highs, evicting the 2 lows won't help but evicting old entries will
	// Window was: [high, high, low, low] → next writes at idx 0,1 (wrapping)
	c.processDelay(DL, 100000, time.Now()) // replaces high→high at [0]: count stays 2... wait let me think
	// Actually idx advanced to 4 which wraps to 0. So:
	// After 4 entries: idx=0, window=[high, high, low, low]
	// 5th entry at idx=0: was high, now high → no change → count=2
	// 6th entry at idx=1: was high, now high → no change → count=2
	// We need to push high entries where lows were
	c.processDelay(DL, 100000, time.Now()) // at idx=1: was high→high, count still 2

	// Push high to replace the lows at idx 2,3
	c.processDelay(DL, 100000, time.Now()) // at idx=2: was low→high, count=3 → BB!

	if !c.dl.bbDetected {
		t.Error("expected BB detection after filling window with highs")
	}
}

func TestLoadPercent_ComputedFromRateStats(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.dl.shaperRateKbps = 10000
	c.ul.shaperRateKbps = 5000

	c.HandleRate(RateStats{
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
	c, _ := newTestCore(t)
	c.dl.shaperRateKbps = 111
	c.ul.shaperRateKbps = 222

	if c.getDirState(DL).shaperRateKbps != 111 {
		t.Error("getDirState(DL) returned wrong state")
	}
	if c.getDirState(UL).shaperRateKbps != 222 {
		t.Error("getDirState(UL) returned wrong state")
	}
}

// --- State transition reset tests ---

func TestHandleRateStats_IdleToRunningResetsTimeouts(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateIdle
	c.cfg.ConnectionActiveThrKbps = 2000
	c.consecutiveTimeouts = 10 // stale value from before idle
	c.lastPingTime = time.Now().Add(-1 * time.Hour)

	c.HandleRate(RateStats{DlRateKbps: 5000, Timestamp: time.Now()})

	if c.state != StateRunning {
		t.Fatalf("expected StateRunning, got %s", c.state)
	}
	if c.consecutiveTimeouts != 0 {
		t.Errorf("consecutiveTimeouts should be reset to 0, got %d", c.consecutiveTimeouts)
	}
	if time.Since(c.lastPingTime) > time.Second {
		t.Error("lastPingTime should be refreshed on Idle→Running transition")
	}
}

func TestHandleRateStats_StallToRunningResetsTimeouts(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateStall
	c.cfg.ConnectionStallThrKbps = 10
	c.consecutiveTimeouts = 15 // stale value from stall
	c.lastPingTime = time.Now().Add(-1 * time.Hour)

	c.HandleRate(RateStats{DlRateKbps: 100, Timestamp: time.Now()})

	if c.state != StateRunning {
		t.Fatalf("expected StateRunning, got %s", c.state)
	}
	if c.consecutiveTimeouts != 0 {
		t.Errorf("consecutiveTimeouts should be reset to 0, got %d", c.consecutiveTimeouts)
	}
	if time.Since(c.lastPingTime) > time.Second {
		t.Error("lastPingTime should be refreshed on Stall→Running transition")
	}
}

// --- processDelay uses MTU ---

func TestProcessDelay_UsesMTUForCompensation(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.BufferbloatDetectionWindow = 4
	c.cfg.BufferbloatDetectionThr = 2

	// With large MTU and low rate, compensation is high → higher effective threshold
	c.dl = dirState{
		shaperRateKbps: 1000, // low rate
		mtuBytes:       9000, // jumbo frame
		delayWindow:    make([]bool, 4),
	}

	// Wire packet compensation: 9000*8*1000/1000 = 72000us = 72ms
	// Threshold = 30ms*1000 + 72000 = 102000us
	// A delta of 50000us (50ms) should NOT exceed this threshold
	c.processDelay(DL, 50000, time.Now())
	if c.dl.delayWindowCount != 0 {
		t.Error("50ms delta should not exceed threshold with jumbo MTU compensation")
	}

	// With standard MTU, same rate: compensation = 1500*8*1000/1000 = 12000us
	// Threshold = 30000 + 12000 = 42000us
	// 50000us > 42000us → should exceed
	c.dl = dirState{
		shaperRateKbps: 1000,
		mtuBytes:       1500,
		delayWindow:    make([]bool, 4),
	}
	c.processDelay(DL, 50000, time.Now())
	if c.dl.delayWindowCount != 1 {
		t.Error("50ms delta should exceed threshold with standard MTU compensation")
	}
}

// --- Reflector delay data ---

func TestUpdateBaseline_InitializesOnFirst(t *testing.T) {
	c, _ := newTestCore(t)

	rd := c.updateBaseline("1.1.1.1", 5000)

	if rd.baselineUs != 5000 {
		t.Errorf("expected baseline 5000, got %f", rd.baselineUs)
	}
}

func TestUpdateBaseline_TracksDecrease(t *testing.T) {
	c, _ := newTestCore(t)

	c.updateBaseline("1.1.1.1", 10000)      // init
	rd := c.updateBaseline("1.1.1.1", 5000) // decrease

	// With alpha_baseline_decrease=0.9, new baseline = 0.9*5000 + 0.1*10000 = 5500
	if rd.baselineUs < 5000 || rd.baselineUs > 10000 {
		t.Errorf("baseline after decrease should be between 5000 and 10000, got %f", rd.baselineUs)
	}
}

func TestUpdateBaseline_TracksIncreaseSlowly(t *testing.T) {
	c, _ := newTestCore(t)

	c.updateBaseline("1.1.1.1", 5000)        // init
	rd := c.updateBaseline("1.1.1.1", 10000) // increase

	// With alpha_baseline_increase=0.001, baseline barely moves
	if rd.baselineUs < 5000 || rd.baselineUs > 5100 {
		t.Errorf("baseline should move slowly upward, got %f", rd.baselineUs)
	}
}

func TestResetReflector_ReinitializesBaseline(t *testing.T) {
	c, _ := newTestCore(t)
	c.updateBaseline("1.1.1.1", 5000)

	c.ResetReflector("1.1.1.1")
	rd := c.updateBaseline("1.1.1.1", 20000)

	if rd.baselineUs != 20000 {
		t.Errorf("expected baseline re-initialized to 20000, got %f", rd.baselineUs)
	}
}

// --- Actuator effects ---

func TestStart_AppliesBaseRates(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	act := &fakeActuator{}
	c := NewCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, testLogger(t))

	c.Start(time.Now())

	if got := act.rates[DL]; len(got) != 1 || got[0] != cfg.Links[0].Download.BaseRateKbps {
		t.Errorf("dl rates = %v, want [%d]", got, cfg.Links[0].Download.BaseRateKbps)
	}
	if got := act.rates[UL]; len(got) != 1 || got[0] != cfg.Links[0].Upload.BaseRateKbps {
		t.Errorf("ul rates = %v, want [%d]", got, cfg.Links[0].Upload.BaseRateKbps)
	}
}

func TestApplyRate_SkippedWhenAdjustDisabled(t *testing.T) {
	c, act := newTestCore(t)
	c.dl.shaperRateKbps = 40000

	c.adjustRateBufferbloat(DL, 80000, 30000)

	if len(act.rates) != 0 {
		t.Errorf("SetRate should not be called with adjust=false, got %v", act.rates)
	}
}

func TestStallAndResume_DrivesPingersAndShaper(t *testing.T) {
	c, act := newTestCore(t)
	c.cfg.StallDetectionThr = 1
	c.cfg.GlobalPingResponseTimeoutS = 1
	c.cfg.ConnectionStallThrKbps = 10
	t0 := c.lastPingTime

	c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: t0.Add(2 * time.Second), Timeout: true})
	if c.State() != StateStall || act.stops != 1 {
		t.Fatalf("expected stall with pingers stopped, got state=%s stops=%d", c.State(), act.stops)
	}

	c.HandleRate(RateStats{DlRateKbps: 100, Timestamp: t0.Add(3 * time.Second)})
	if c.State() != StateRunning || act.starts != 1 || act.invalidates != 1 {
		t.Errorf("expected resume with pingers started and shaper invalidated, got state=%s starts=%d invalidates=%d",
			c.State(), act.starts, act.invalidates)
	}
}

func TestHandleTimeout_UsesEventTimestamp(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.StallDetectionThr = 1
	c.cfg.GlobalPingResponseTimeoutS = 1
	t0 := c.lastPingTime

	// Core measures the time between event timestamps, not with the wall clock.
	c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: t0.Add(500 * time.Millisecond), Timeout: true})
	if c.State() != StateRunning {
		t.Fatalf("should not stall 500ms after last ping, got %s", c.State())
	}
	c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: t0.Add(1500 * time.Millisecond), Timeout: true})
	if c.State() != StateStall {
		t.Errorf("expected stall 1.5s after last ping, got %s", c.State())
	}
}

// --- Determinism ---

func TestCore_DeterministicForSameEvents(t *testing.T) {
	run := func() []int {
		cfg := testConfig()
		cfg.Links[0].Download.Adjust = true
		act := &fakeActuator{}
		c := NewCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, testLogger(t))
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		c.Start(t0)
		for i := 0; i < 200; i++ {
			now := t0.Add(time.Duration(i) * 50 * time.Millisecond)
			if i%2 == 0 {
				c.HandleRate(RateStats{DlRateKbps: 9000, UlRateKbps: 1000, Timestamp: now})
			}
			// The queue increases, then decreases
			rtt := 10*time.Millisecond + time.Duration(i%60)*time.Millisecond
			c.HandlePing(PingResult{Reflector: cfg.Links[0].Reflectors[i%4], RTT: rtt, Timestamp: now})
		}
		return act.rates[DL]
	}

	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("expected some rate changes")
	}
	if len(a) != len(b) {
		t.Fatalf("runs diverged: %d vs %d rate changes", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("runs diverged at change %d: %d vs %d", i, a[i], b[i])
		}
	}
}

// owdPing returns a ping result with one-way delays in ms.
func owdPing(reflector string, ts time.Time, ulMs, dlMs float64) PingResult {
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	return PingResult{
		Reflector: reflector,
		RTT:       ms(ulMs + dlMs),
		Timestamp: ts,
		OWD:       true,
		UlOWD:     ms(ulMs),
		DlOWD:     ms(dlMs),
	}
}

// With one-way delays, a queue in the upload direction must decrease only
// the upload rate. The clock offset (here +500 ms upload, -500 ms download)
// must have no effect.
func TestHandlePing_OWDDelayOnlyInUpload(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.dl.loadPercent, c.ul.loadPercent = 0.9, 0.9
	dlBefore, ulBefore := c.dl.shaperRateKbps, c.ul.shaperRateKbps

	ts := time.Now()
	c.HandlePing(owdPing("1.1.1.1", ts, 510, -490)) // baseline: 10 ms each way
	for i := 1; i <= 6; i++ {
		ts = ts.Add(50 * time.Millisecond)
		c.HandlePing(owdPing("1.1.1.1", ts, 510+80, -490)) // +80 ms upload queue
	}

	if c.ul.shaperRateKbps >= ulBefore {
		t.Errorf("upload rate %.0f, want a decrease from %.0f", c.ul.shaperRateKbps, ulBefore)
	}
	if c.dl.shaperRateKbps < dlBefore {
		t.Errorf("download rate %.0f, want no decrease from %.0f", c.dl.shaperRateKbps, dlBefore)
	}
}

// Without one-way delays (RTT/2), the same upload queue also decreases the
// download rate. This is the reason for ICMP timestamps.
func TestHandlePing_RTTDelayAppliesToBothDirections(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	c.dl.loadPercent, c.ul.loadPercent = 0.9, 0.9
	dlBefore := c.dl.shaperRateKbps

	ts := time.Now()
	p := owdPing("1.1.1.1", ts, 10, 10)
	p.OWD = false
	c.HandlePing(p)
	for i := 1; i <= 6; i++ {
		ts = ts.Add(50 * time.Millisecond)
		p := owdPing("1.1.1.1", ts, 10+80, 10)
		p.OWD = false
		c.HandlePing(p)
	}

	if c.dl.shaperRateKbps >= dlBefore {
		t.Errorf("download rate %.0f, want a decrease from %.0f with RTT/2", c.dl.shaperRateKbps, dlBefore)
	}
}

// With learn_cross_delay, an upload delay that comes with download
// activity and a low upload load goes into the learned cross delay. Then
// the same delay does not decrease the upload rate. A larger delay does.
func TestHandlePing_LearnCrossDelay(t *testing.T) {
	// run returns a Core and two functions. ping sends n pings with an
	// upload delay of ulMs, one every 50 ms. rate sends a rate sample.
	run := func(learn bool) (*Core, func(int, float64), func(float64, float64)) {
		c, _ := newTestCore(t)
		c.cfg.LearnCrossDelay = learn
		c.state = StateRunning
		ts := time.Now()
		c.HandlePing(owdPing("1.1.1.1", ts, 10, 10))
		ping := func(n int, ulMs float64) {
			for range n {
				ts = ts.Add(50 * time.Millisecond)
				c.HandlePing(owdPing("1.1.1.1", ts, 10+ulMs, 10))
			}
		}
		rate := func(dlKbps, ulKbps float64) {
			c.HandleRate(RateStats{Timestamp: ts, DlRateKbps: dlKbps, UlRateKbps: ulKbps})
		}
		return c, ping, rate
	}
	// Download active, upload load low (ACKs only): learn 25 ms.
	for _, learn := range []bool{false, true} {
		c, ping, rate := run(learn)
		rate(50000, 500)
		ping(200, 25)
		// The upload is loaded now. 45 ms is above the 30 ms threshold,
		// but not above the learned delay plus the threshold.
		rate(50000, 9000)
		before := c.ul.shaperRateKbps
		ping(20, 45)
		decreased := c.ul.shaperRateKbps < before
		if learn && decreased {
			t.Errorf("with learn_cross_delay: rate %.0f -> %.0f at 45 ms (learned %.0f us), want no decrease", before, c.ul.shaperRateKbps, c.ul.crossDelayUs)
		}
		if !learn && !decreased {
			t.Errorf("without learn_cross_delay: rate %.0f -> %.0f at 45 ms, want a decrease", before, c.ul.shaperRateKbps)
		}
		if learn {
			// A delay above learned + threshold decreases the rate.
			before = c.ul.shaperRateKbps
			ping(20, 25+30+40)
			if c.ul.shaperRateKbps >= before {
				t.Errorf("with learn_cross_delay: rate %.0f -> %.0f at 95 ms, want a decrease", before, c.ul.shaperRateKbps)
			}
		}
	}
}

// learn_cross_delay learns only after the upload load was low for
// crossDelayMinLowLoad. After an upload capacity decrease, the load is low
// for a short time while the queue of the upload shaper increases.
func TestHandlePing_LearnCrossDelay_ShortLowLoad(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.LearnCrossDelay = true
	c.state = StateRunning
	ts := time.Now()
	c.HandlePing(owdPing("1.1.1.1", ts, 10, 10))
	c.HandleRate(RateStats{Timestamp: ts, DlRateKbps: 50000, UlRateKbps: 500})
	// 1.5 s of low load: no learning.
	for range 30 {
		ts = ts.Add(50 * time.Millisecond)
		c.HandlePing(owdPing("1.1.1.1", ts, 35, 10))
	}
	if c.ul.crossDelayUs != 0 {
		t.Fatalf("learned %.0f us after 1.5 s of low load, want 0", c.ul.crossDelayUs)
	}
	// After 2 s or more: learning.
	for range 20 {
		ts = ts.Add(50 * time.Millisecond)
		c.HandlePing(owdPing("1.1.1.1", ts, 35, 10))
	}
	if c.ul.crossDelayUs == 0 {
		t.Errorf("learned 0 us after 2.5 s of low load, want more")
	}
	// High load resets the low-load time.
	c.HandleRate(RateStats{Timestamp: ts, DlRateKbps: 50000, UlRateKbps: c.ul.shaperRateKbps})
	if !c.ul.lowLoadSince.IsZero() {
		t.Errorf("low-load time not reset at high load")
	}
}

// The latency target changes the delay thresholds after each window: down
// if the p95 is above the target, up if it is below. A decrease without a
// lower p95 is undone, and the threshold stays the minimum for some time.
func TestObserveLatency_ChangesThresholds(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 10
	ds := &c.dl
	ds.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	// window sends the deltas (ms) of one window, one each 50 ms.
	window := func(deltaMs float64) {
		for i := 0; i < 200; i++ {
			c.observeLatency(DL, "r1", 10000+deltaMs*1000, 10000, ts)
			ts = ts.Add(50 * time.Millisecond)
		}
		c.observeLatency(DL, "r1", 10000+deltaMs*1000, 10000, ts) // end of the window
	}
	window(50) // first window: starts the window time
	if got := ds.scale(); got != latencyTargetStepDown {
		t.Fatalf("scale after p95 50 ms (target 30) = %.2f, want %.2f", got, latencyTargetStepDown)
	}
	window(50) // the decrease did not decrease the p95
	if got := ds.scale(); got != 1 {
		t.Fatalf("scale after no gain = %.2f, want 1 (undo)", got)
	}
	window(50) // the floor stops more decreases
	if got := ds.scale(); got != 1 {
		t.Fatalf("scale at the floor = %.2f, want 1", got)
	}
	window(10) // p95 below the target: increase
	if got := ds.scale(); got != latencyTargetStepUp {
		t.Fatalf("scale after p95 10 ms = %.2f, want %.2f", got, latencyTargetStepUp)
	}
	// The thresholds of the controller use the factor: a delta 5 % above
	// the configured threshold is below the scaled threshold (× 1.1).
	before := ds.delayWindowCount
	c.processDelay(DL, 1.05*c.link.Download.OWDDeltaDelayThrMs*1000, ts)
	if ds.delayWindowCount > before {
		t.Errorf("delta 5 %% above the configured threshold counted as bufferbloat, want not (scaled threshold × %.1f)", latencyTargetStepUp)
	}
}

// Without high load, the window has too few samples, and the thresholds do
// not change. Without a target, Core does not collect samples.
func TestObserveLatency_NeedsHighLoad(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.LatencyTargetWindowS = 10
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < 1000; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(50 * time.Millisecond)
	}
	if len(c.dl.ltSamples) != 0 || c.dl.scale() != 1 {
		t.Errorf("without a target: %d samples, scale %.2f", len(c.dl.ltSamples), c.dl.scale())
	}
	c.link.Download.LatencyTargetP95Ms = 30
	c.dl.loadPercent = 0.1
	for i := 0; i < 1000; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(50 * time.Millisecond)
	}
	if c.dl.scale() != 1 {
		t.Errorf("at low load: scale %.2f, want 1", c.dl.scale())
	}
}

// The baseline of the controller increases slowly with a constant delay.
// Thus, the delta decreases over time also if the delay does not change.
// The latency target must not take this as a gain of a decrease: with a
// constant delay above the target, a decrease is undone.
func TestObserveLatency_BaselineDriftIsNoGain(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Upload.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < 50; i++ { // baselines without load
		c.HandlePing(owdPing("1.1.1.1", ts, 10, 10))
		ts = ts.Add(300 * time.Millisecond)
	}
	c.ul.loadPercent = 1
	var scales []float64
	for w := 0; w < 3; w++ {
		for i := 0; i <= 100; i++ { // one window (30 s after the first sample): constant upload delay +50 ms
			c.HandlePing(owdPing("1.1.1.1", ts, 60, 10))
			ts = ts.Add(300 * time.Millisecond)
		}
		scales = append(scales, c.ul.scale())
	}
	// Window 1: p95 50 > 30 → decrease. Window 2: the same delay → undo.
	if scales[0] != latencyTargetStepDown || scales[1] != 1 {
		t.Errorf("scales after the windows %v, want [%.1f 1 ...] (the decrease must be undone)", scales, latencyTargetStepDown)
	}
}

// latency_target_min_thr_ms above the configured delay threshold is the
// minimum of the effective threshold.
func TestObserveLatency_MinAboveThreshold(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.link.Download.LatencyTargetMinThrMs = 2 * c.link.Download.OWDDeltaDelayThrMs
	c.observeLatency(DL, "r1", 0, 0, time.Unix(1_800_000_000, 0))
	if got := c.dl.scale(); got != 2 {
		t.Errorf("scale %.2f, want 2 (minimum threshold = 2 x threshold)", got)
	}
}

// A window with too few samples continues until it has enough samples
// (for example with one reflector at 1 s).
func TestObserveLatency_FewPingsContinueWindow(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	// One ping per second: the first window (30 s) has too few samples.
	// The decrease comes after latencyTargetMinSamples pings.
	n := latencyTargetMinSamples + 10
	for i := 0; i < n; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(time.Second)
	}
	if got := c.dl.scale(); got != latencyTargetStepDown {
		t.Errorf("scale %.2f after %d pings at 80 ms, want %.1f", got, n, latencyTargetStepDown)
	}
}

// At moderate load (below high_load_thr), the baseline of the controller
// can contain delay from the load (5G grant delay). The reference of the
// latency target must not follow it: else the measured p95 is too low and
// the thresholds increase.
func TestObserveLatency_ModerateLoadKeepsReference(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 50
	c.cfg.LatencyTargetWindowS = 30
	ts := time.Unix(1_800_000_000, 0)
	c.dl.loadPercent = 0
	c.observeLatency(DL, "r1", 10000, 10000, ts)
	// Moderate load for a long time: the baseline increases to 30 ms.
	c.dl.loadPercent = 0.5
	for i := 0; i < 100; i++ {
		ts = ts.Add(time.Second)
		c.observeLatency(DL, "r1", 30000, 30000, ts)
	}
	// High load: the delay is 60 ms above the reference without load.
	c.dl.loadPercent = 1
	for i := 0; i < 2*latencyTargetMinSamples; i++ {
		ts = ts.Add(300 * time.Millisecond)
		c.observeLatency(DL, "r1", 70000, 30000, ts)
	}
	if got := c.dl.scale(); got >= 1 {
		t.Errorf("scale %.2f, want a decrease (p95 60 ms, target 50 ms)", got)
	}
}

// A window from an earlier time of high load does not continue after a
// stop: its old samples are not in the next window.
func TestObserveLatency_StaleWindowExpires(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	// 15 s of high load with a large delay: enough samples, but the window
	// is not complete.
	for i := 0; i < 3*latencyTargetMinSamples/2; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(100 * time.Millisecond)
	}
	// The pingers stop for 2 minutes, then high load with a small delay.
	ts = ts.Add(2 * time.Minute)
	for i := 0; i < 2*latencyTargetMinSamples; i++ {
		c.observeLatency(DL, "r1", 20000, 10000, ts)
		ts = ts.Add(200 * time.Millisecond)
	}
	if got := c.dl.scale(); got <= 1 {
		t.Errorf("scale %.2f, want an increase (p95 10 ms after the stop, target 30 ms)", got)
	}
}

// With RTT/2, the delay of one direction is also in the other direction.
// The latency target of a direction uses only the pings at high load of
// that direction: upload load does not change the download thresholds.
func TestHandlePing_LatencyTargetRTTOtherDirection(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 20
	c.cfg.LatencyTargetWindowS = 30
	ts := time.Unix(1_800_000_000, 0)
	ping := func(rttMs float64) {
		c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: time.Duration(rttMs * float64(time.Millisecond)), Timestamp: ts})
		ts = ts.Add(100 * time.Millisecond)
	}
	for i := 0; i < 50; i++ {
		ping(20)
	}
	// 2 minutes of upload load with a large upload queue (RTT +100 ms).
	c.ul.loadPercent, c.dl.loadPercent = 1, 0
	for i := 0; i < 1200; i++ {
		ping(120)
	}
	if got := c.dl.scale(); got != 1 {
		t.Errorf("download scale %.2f, want 1 (no download load)", got)
	}
}

// Without one-way delays, the download reference does not follow the
// baseline while the upload is loaded: the RTT then contains the upload
// queue.
func TestObserveLatency_RTTReferenceNeedsTwoLowLoads(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 50
	c.cfg.LatencyTargetWindowS = 30
	ts := time.Unix(1_800_000_000, 0)
	c.observeLatency(DL, "r1", 20000, 20000, ts)
	// Upload load for a long time: the RTT baseline increases to 60 ms.
	c.ul.loadPercent = 1
	for i := 0; i < 100; i++ {
		ts = ts.Add(time.Second)
		c.observeLatency(DL, "r1", 60000, 60000, ts)
	}
	// Then download load too: RTT 100 ms, 80 ms above the reference.
	c.dl.loadPercent = 1
	for i := 0; i < 2*latencyTargetMinSamples; i++ {
		ts = ts.Add(300 * time.Millisecond)
		c.observeLatency(DL, "r1", 100000, 60000, ts)
	}
	if got := c.dl.scale(); got >= 1 {
		t.Errorf("download scale %.2f, want a decrease (p95 80 ms, target 50 ms)", got)
	}
}

// With one-way delays, the upload reference does not follow the baseline
// while the download is loaded: download activity adds upload delay (5G
// cross delay).
func TestObserveLatency_ULReferenceNeedsLowDownload(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Upload.LatencyTargetP95Ms = 50
	c.cfg.LatencyTargetWindowS = 30
	c.lastPingOWD = true
	ts := time.Unix(1_800_000_000, 0)
	c.observeLatency(UL, "r1", 10000, 10000, ts)
	// Download load for a long time: the upload baseline increases to 40 ms.
	c.dl.loadPercent = 1
	for i := 0; i < 100; i++ {
		ts = ts.Add(time.Second)
		c.observeLatency(UL, "r1", 40000, 40000, ts)
	}
	// Then upload load too: 70 ms above the reference without load.
	c.ul.loadPercent = 1
	for i := 0; i < 2*latencyTargetMinSamples; i++ {
		ts = ts.Add(300 * time.Millisecond)
		c.observeLatency(UL, "r1", 80000, 40000, ts)
	}
	if got := c.ul.scale(); got >= 1 {
		t.Errorf("upload scale %.2f, want a decrease (p95 70 ms, target 50 ms)", got)
	}
}

// At a low load fraction but with traffic (achieved rate above
// connection_active_thr_kbps), the reference does not follow the baseline:
// on 5G, the grant delay is already there at a low load.
func TestObserveLatency_ReferenceNeedsIdle(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 50
	c.cfg.LatencyTargetWindowS = 30
	c.lastPingOWD = true
	ts := time.Unix(1_800_000_000, 0)
	c.observeLatency(DL, "r1", 10000, 10000, ts)
	// 10 % load with 5 Mbit/s for a long time: the baseline increases to
	// 40 ms.
	c.dl.loadPercent, c.dl.achievedKbps = 0.1, 5000
	for i := 0; i < 100; i++ {
		ts = ts.Add(time.Second)
		c.observeLatency(DL, "r1", 40000, 40000, ts)
	}
	// High load: 70 ms above the idle reference.
	c.dl.loadPercent, c.dl.achievedKbps = 1, 50000
	for i := 0; i < 2*latencyTargetMinSamples; i++ {
		ts = ts.Add(300 * time.Millisecond)
		c.observeLatency(DL, "r1", 80000, 40000, ts)
	}
	if got := c.dl.scale(); got >= 1 {
		t.Errorf("scale %.2f, want a decrease (p95 70 ms, target 50 ms)", got)
	}
}

// With sparse pings (one reflector at 4 s, 30 s window), a window needs
// 400 s for 100 samples. It must continue until it has them: a cap on the
// window time would discard them again and again, and the target would
// never act.
func TestObserveLatency_SparsePingsReachTarget(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < latencyTargetMinSamples+5; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(4 * time.Second)
	}
	if got := c.dl.scale(); got != latencyTargetStepDown {
		t.Errorf("scale %.2f after %d pings at 4 s, want %.1f", got, latencyTargetMinSamples+5, latencyTargetStepDown)
	}
}

// A ping at low load does not complete a window: also not 20 s after a
// load burst of 15 s (less than one window time, no gap reset).
func TestObserveLatency_LowLoadPingDoesNotComplete(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3*latencyTargetMinSamples/2; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(100 * time.Millisecond)
	}
	c.dl.loadPercent = 0
	c.observeLatency(DL, "r1", 10000, 10000, ts.Add(20*time.Second))
	if got := c.dl.scale(); got != 1 {
		t.Errorf("scale %.2f after a low-load ping, want 1 (no change while idle)", got)
	}
}

// A threshold change clears the bufferbloat detection window: its entries
// were classified with the old threshold.
func TestObserveLatency_ScaleChangeClearsDetectionWindow(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	for i := range c.dl.delayWindow {
		c.dl.delayWindow[i] = true
	}
	c.dl.delayWindowCount = len(c.dl.delayWindow)
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < latencyTargetMinSamples+1; i++ { // p95 5 ms: increase
		c.observeLatency(DL, "r1", 15000, 10000, ts)
		ts = ts.Add(300 * time.Millisecond)
	}
	if c.dl.scale() == 1 {
		t.Fatal("no threshold change")
	}
	if c.dl.delayWindowCount != 0 || slices.Contains(c.dl.delayWindow, true) {
		t.Errorf("detection window count %d after a threshold change, want 0", c.dl.delayWindowCount)
	}
}

// A very small window time (valid, positive) does not make each gap a new
// window: the gap check is in seconds, without a duration overflow.
func TestObserveLatency_ExtremeWindowTime(t *testing.T) {
	for _, w := range []float64{1e-10, 1e12} {
		c, _ := newTestCore(t)
		c.link.Download.LatencyTargetP95Ms = 30
		c.cfg.LatencyTargetWindowS = w
		c.dl.loadPercent = 1
		ts := time.Unix(1_800_000_000, 0)
		for i := 0; i < 3; i++ {
			c.observeLatency(DL, "r1", 90000, 10000, ts)
			ts = ts.Add(time.Second)
		}
		if w > 1 && len(c.dl.ltSamples) != 3 {
			t.Errorf("window %g s: %d samples after 3 pings at 1 s, want 3 (no gap reset)", w, len(c.dl.ltSamples))
		}
	}
}

// After a stop of the pingers, a ping at low load does not complete the
// old window.
func TestObserveLatency_StaleWindowLowLoad(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	c.dl.loadPercent = 1
	ts := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3*latencyTargetMinSamples/2; i++ {
		c.observeLatency(DL, "r1", 90000, 10000, ts)
		ts = ts.Add(100 * time.Millisecond)
	}
	// The pingers stop for 2 minutes, then a ping at low load.
	c.dl.loadPercent = 0
	c.observeLatency(DL, "r1", 10000, 10000, ts.Add(2*time.Minute))
	if got := c.dl.scale(); got != 1 {
		t.Errorf("scale %.2f, want 1 (the old window must not complete)", got)
	}
}

// After an undo, the floor keeps the reference of the latency target.
// Else, the baseline of the controller increases with the constant delay,
// the next window shows a low p95, and the thresholds increase although
// the delay did not change.
func TestObserveLatency_FloorKeepsReference(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Upload.LatencyTargetP95Ms = 30
	c.cfg.LatencyTargetWindowS = 30
	ts := time.Unix(1_800_000_000, 0)
	reflectors := []string{"r1", "r2", "r3", "r4", "r5", "r6"}
	ping := func(ulMs float64) {
		for _, r := range reflectors {
			c.HandlePing(owdPing(r, ts, ulMs, 10))
			ts = ts.Add(50 * time.Millisecond)
		}
	}
	for i := 0; i < 20; i++ {
		ping(10)
	}
	c.ul.loadPercent = 1
	var scales []float64
	for w := 0; w < 6; w++ { // 6 windows of 30 s with a constant +50 ms
		for i := 0; i < 100; i++ {
			ping(60)
		}
		scales = append(scales, c.ul.scale())
	}
	for i, sc := range scales[1:] {
		if sc > 1 {
			t.Errorf("scales %v: increase in window %d, want no increase with a constant delay above the target", scales, i+2)
			break
		}
	}
}

// ResetReflector also removes the reference of the latency target.
func TestResetReflector_LatencyTarget(t *testing.T) {
	c, _ := newTestCore(t)
	c.link.Download.LatencyTargetP95Ms = 30
	c.observeLatency(DL, "r1", 50000, 10000, time.Unix(1_800_000_000, 0))
	c.ResetReflector("r1")
	if _, ok := c.dl.ltBase["r1"]; ok {
		t.Error("reference of r1 is still there after ResetReflector")
	}
}
