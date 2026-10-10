package main

import (
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

func TestHandlePing_UpdatesDeltaEWMAUnderLowLoad(t *testing.T) {
	c, _ := newTestCore(t)
	now := time.Now()

	c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 10 * time.Millisecond, Timestamp: now})
	c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 12 * time.Millisecond, Timestamp: now})

	// delta = (12000 - baseline) / 2 with baseline ≈ 10002
	rd := c.reflectors["1.1.1.1"]
	deltaUs := (12000 - rd.baselineUs) / 2
	expected := c.cfg.AlphaDeltaEWMA * deltaUs
	if rd.deltaEWMA < expected-1 || rd.deltaEWMA > expected+1 {
		t.Errorf("expected deltaEWMA ~%.1f, got %.1f", expected, rd.deltaEWMA)
	}
}

func TestHandlePing_SkipsDeltaEWMAUnderHighLoad(t *testing.T) {
	c, _ := newTestCore(t)
	c.dl.loadPercent = 0.9
	now := time.Now()

	c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 10 * time.Millisecond, Timestamp: now})
	c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 20 * time.Millisecond, Timestamp: now})

	if ewma := c.reflectors["1.1.1.1"].deltaEWMA; ewma != 0 {
		t.Errorf("deltaEWMA should not update under high load, got %f", ewma)
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
