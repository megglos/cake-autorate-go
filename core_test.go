package main

import (
	"math"
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

func TestProcessDelay_RepeatDecreaseOnRise(t *testing.T) {
	run := func(onRise bool, deltasMs []float64) int {
		c, _ := newTestCore(t)
		c.state = StateRunning
		c.cfg.BufferbloatDetectionWindow = 4
		c.cfg.BufferbloatDetectionThr = 2
		c.cfg.RepeatDecreaseOnRise = onRise
		c.dl = dirState{
			shaperRateKbps: float64(c.link.Download.BaseRateKbps),
			mtuBytes:       defaultMTU,
			delayWindow:    make([]bool, 4),
		}
		decreases := 0
		now := time.Now()
		for _, d := range deltasMs {
			before := c.dl.shaperRateKbps
			c.processDelay(DL, d*1000, now)
			if c.dl.shaperRateKbps < before {
				decreases++
			}
			now = now.Add(50 * time.Millisecond) // in the refractory period
		}
		return decreases
	}

	// The queue empties after the first decrease: the delay goes down.
	falling := []float64{100, 100, 90, 80, 70}
	if got := run(false, falling); got != 4 {
		t.Errorf("without the option: %d decreases, want 4 (one for each ping with bufferbloat)", got)
	}
	if got := run(true, falling); got != 1 {
		t.Errorf("falling delay: %d decreases, want 1", got)
	}
	// The delay still increases: each new maximum decreases the rate again.
	rising := []float64{100, 100, 120, 110, 140}
	if got := run(true, rising); got != 3 {
		t.Errorf("rising delay: %d decreases, want 3", got)
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

// owdRun sends pings of one reflector at 300 ms for d. owd returns the
// upload and the download delay (ms) at the time t since the start. It
// returns the deltas of the last ping (ms).
func owdRun(c *Core, d time.Duration, owd func(t time.Duration) (ulMs, dlMs float64)) (dlDeltaMs, ulDeltaMs float64) {
	start := time.Now()
	for t := time.Duration(0); t <= d; t += 300 * time.Millisecond {
		ul, dl := owd(t)
		p := owdPing("1.1.1.1", start.Add(t), ul, dl)
		rd := c.updateBaseline(p.Reflector, float64(p.RTT.Microseconds()))
		dlUs, ulUs := c.owdDeltas(rd, p)
		dlDeltaMs, ulDeltaMs = dlUs/1000, ulUs/1000
	}
	return dlDeltaMs, ulDeltaMs
}

// A clock change of 10 ms in 30 s (NTP after the boot) moves 10 ms from the
// upload to the download delay. Without load, the deltas must stay 0.
func TestOWDDeltas_ClockSlewIsNotDelay(t *testing.T) {
	for _, sign := range []float64{1, -1} {
		c, _ := newTestCore(t)
		dl, ul := owdRun(c, 40*time.Second, func(t time.Duration) (float64, float64) {
			drift := sign * 10 * math.Min(t.Seconds()/30, 1)
			return 10 - drift, 10 + drift
		})
		if dl > 0.5 || ul > 0.5 {
			t.Errorf("clock change %+.0f ms: delta dl %.1f ms, ul %.1f ms, want approximately 0", sign*10, dl, ul)
		}
	}
}

// After the boot, ntpd slews the clock faster than the kernel frequency
// limit: here 150 ms in 30 s. Without load, the deltas must stay small.
func TestOWDDeltas_FastClockSlewIsNotDelay(t *testing.T) {
	for _, sign := range []float64{1, -1} {
		c, _ := newTestCore(t)
		start := time.Now()
		var maxDl, maxUl, dl, ul float64
		for t := time.Duration(0); t <= 40*time.Second; t += 300 * time.Millisecond {
			drift := sign * 150 * math.Min(t.Seconds()/30, 1)
			p := owdPing("1.1.1.1", start.Add(t), 10-drift, 10+drift)
			rd := c.updateBaseline(p.Reflector, float64(p.RTT.Microseconds()))
			dlUs, ulUs := c.owdDeltas(rd, p)
			dl, ul = dlUs/1000, ulUs/1000
			maxDl, maxUl = math.Max(maxDl, dl), math.Max(maxUl, ul)
		}
		if maxDl > 10 || maxUl > 10 || dl > 0.5 || ul > 0.5 {
			t.Errorf("slew %+.0f ms: maximum delta dl %.1f ms, ul %.1f ms, last dl %.1f ms, ul %.1f ms", sign*150, maxDl, maxUl, dl, ul)
		}
	}
}

// A clock step of 650 ms (NTP after the boot of a router without a
// battery clock) must not go into the delta, also during a download queue.
func TestOWDDeltas_ClockStepIsNotDelay(t *testing.T) {
	for _, queue := range []float64{0, 40} {
		for _, sign := range []float64{1, -1} {
			c, _ := newTestCore(t)
			dl, ul := owdRun(c, 12*time.Second, func(t time.Duration) (float64, float64) {
				step, q := 0.0, 0.0
				if t >= 5*time.Second {
					q = queue
				}
				if t >= 10*time.Second {
					step = sign * 650
				}
				return 10 - step, 10 + q + step
			})
			if dl > queue+1 || ul > 1 {
				t.Errorf("step %+.0f ms, queue %.0f ms: delta dl %.1f ms, ul %.1f ms, want dl approximately the queue, ul 0", sign*650, queue, dl, ul)
			}
		}
	}
}

// A download queue with upload jitter is not a clock change: the download
// delta must be the same as without the jitter. (The download baseline
// increases slowly in the two runs.)
func TestOWDDeltas_QueueWithJitterStays(t *testing.T) {
	run := func(jitterMs float64) float64 {
		c, _ := newTestCore(t)
		dl, _ := owdRun(c, 60*time.Second, func(t time.Duration) (float64, float64) {
			jitter := float64(int(t/(300*time.Millisecond))%5) * jitterMs
			queue := 0.0
			if t >= 5*time.Second {
				queue = 40
			}
			return 10 + jitter, 10 + queue
		})
		return dl
	}
	if with, without := run(0.5), run(0); with < without-0.5 {
		t.Errorf("download delta %.1f ms with upload jitter, %.1f ms without", with, without)
	}
}

// A path change is faster than a clock slew. A shorter path (10 ms less in
// each direction) during a download queue of 40 ms must not move the
// upload decrease to the download baseline: the download delta decreases
// only by the path change of the download (10 ms).
func TestOWDDeltas_PathChangeIsNotClockChange(t *testing.T) {
	run := func(changeMs float64) float64 {
		c, _ := newTestCore(t)
		dl, _ := owdRun(c, 21*time.Second, func(t time.Duration) (float64, float64) {
			base, queue := 20.0, 0.0
			if t >= 5*time.Second {
				queue = 40
			}
			if t >= 20*time.Second {
				base -= changeMs
			}
			return base, base + queue
		})
		return dl
	}
	if with, without := run(10), run(0); with < without-10.5 {
		t.Errorf("download delta %.1f ms after the path change, %.1f ms without it", with, without)
	}
}

// A shorter route decreases the two delays: no clock change, the two
// baselines follow.
func TestOWDDeltas_ShorterRouteInBothDirections(t *testing.T) {
	c, _ := newTestCore(t)
	rd := &reflectorDelay{}
	ts := time.Now()
	c.owdDeltas(rd, owdPing("1.1.1.1", ts, 20, 20))
	c.owdDeltas(rd, owdPing("1.1.1.1", ts.Add(time.Second), 15, 15))
	if rd.dlBaselineUs > 16000 || rd.ulBaselineUs > 16000 {
		t.Errorf("baselines dl %.0f us, ul %.0f us, want approximately 15000", rd.dlBaselineUs, rd.ulBaselineUs)
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

func TestSetPaused_StopsRateChangesAndUsesBaseRate(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	act := &fakeActuator{}
	c := NewCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, testLogger(t))
	now := time.Now()
	c.Start(now)
	c.dl.shaperRateKbps = float64(cfg.Links[0].Download.MaxRateKbps)

	c.SetPaused(DL, true)
	base := cfg.Links[0].Download.BaseRateKbps
	if got := act.rates[DL][len(act.rates[DL])-1]; got != base {
		t.Errorf("pause must set the base rate %d, got %d", base, got)
	}
	if !c.Paused(DL) || c.Paused(UL) {
		t.Error("only DL must be paused")
	}

	// Pings with a high delay must not change the rate of a paused direction.
	n := len(act.rates[DL])
	c.updateBaseline("1.1.1.1", 10000)
	for i := range 20 {
		c.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 500 * time.Millisecond, Timestamp: now.Add(time.Duration(i) * time.Second)})
	}
	c.setRates(1, 1)
	if len(act.rates[DL]) != n || c.dl.shaperRateKbps != float64(base) {
		t.Errorf("a paused direction changed its rate: %v", act.rates[DL])
	}
	if c.ul.shaperRateKbps == float64(cfg.Links[0].Upload.BaseRateKbps) {
		t.Error("the upload direction must keep running")
	}

	c.SetPaused(DL, false)
	if c.Paused(DL) {
		t.Error("resume must clear the pause")
	}
	if got := act.rates[DL][len(act.rates[DL])-1]; got != base {
		t.Errorf("resume must start at the base rate %d, got %d", base, got)
	}
}

func TestSetPaused_RepeatSetsBaseRateAgain(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	act := &fakeActuator{}
	c := NewCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, testLogger(t))
	c.Start(time.Now())
	c.SetPaused(DL, true)
	n := len(act.rates[DL])
	c.SetPaused(DL, true)
	if len(act.rates[DL]) != n+1 || !c.Paused(DL) {
		t.Errorf("a second pause must send the base rate again: %v", act.rates[DL])
	}
	// A resume also sets the base rate each time. Thus, a resume after a
	// failed rate change is a retry.
	c.SetPaused(DL, false)
	m := len(act.rates[DL])
	c.ul.shaperRateKbps = float64(cfg.Links[0].Upload.MaxRateKbps)
	c.SetPaused(DL, false)
	if len(act.rates[DL]) != m+1 || c.Paused(DL) {
		t.Errorf("a second resume must send the base rate again: %v", act.rates[DL])
	}
}

// In the calibration mode, Core does not go to IDLE: the session needs
// the pings also without load.
func TestCalibratingPreventsIdle(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001
	now := time.Now()
	c.SetCalibrating(true, now)
	for i := range 3 {
		c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: now.Add(time.Duration(i) * time.Second)})
	}
	if c.state != StateRunning {
		t.Errorf("calibrating: got %s, want RUNNING", c.state)
	}
	// After the session, the normal idle detection applies again.
	c.SetCalibrating(false, now.Add(3*time.Second))
	for i := 4; i < 6; i++ {
		c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: now.Add(time.Duration(i) * time.Second)})
	}
	if c.state != StateIdle {
		t.Errorf("after calibrating: got %s, want IDLE", c.state)
	}
}

// The calibration mode starts the pingers of an idle link at once.
func TestCalibratingLeavesIdle(t *testing.T) {
	c, act := newTestCore(t)
	c.state = StateIdle
	starts := act.starts
	c.SetCalibrating(true, time.Now())
	if c.state != StateRunning || act.starts != starts+1 || !c.Calibrating() || !c.Snapshot().Calibrating {
		t.Errorf("state %s, pinger starts %d, status %+v", c.state, act.starts-starts, c.Snapshot())
	}
	// In RUNNING, the mode does not start the pingers again.
	c.SetCalibrating(true, time.Now())
	if act.starts != starts+1 {
		t.Errorf("pinger starts %d, want 1", act.starts-starts)
	}
}

// A fixed rate pauses the rate control; SetPaused ends it with the base
// rate.
func TestSetFixedRate(t *testing.T) {
	c, act := newTestCore(t)
	c.link.Download.Adjust = true
	c.SetFixedRate(DL, unshapedKbps)
	if got := act.rates[DL]; len(got) == 0 || got[len(got)-1] != unshapedKbps || !c.Paused(DL) {
		t.Fatalf("rates %v, paused %v", got, c.Paused(DL))
	}
	// The rate control does not change the fixed rate.
	c.setRates(1000, 1000)
	if got := act.rates[DL]; got[len(got)-1] != unshapedKbps {
		t.Errorf("a paused direction changed: %v", got)
	}
	c.SetPaused(DL, false)
	if got := act.rates[DL]; got[len(got)-1] != c.link.Download.BaseRateKbps || c.Paused(DL) {
		t.Errorf("after resume: rates %v, paused %v", got, c.Paused(DL))
	}
}

// Many timeouts in a calibration block give STALL. The next block starts
// the pingers again at once, for its idle measurements.
func TestCalibratingLeavesStall(t *testing.T) {
	c, act := newTestCore(t)
	c.SetCalibrating(true, time.Now())
	c.state = StateStall
	starts, invalidates := act.starts, act.invalidates
	c.SetCalibrating(true, time.Now())
	if c.state != StateRunning || act.starts != starts+1 || act.invalidates != invalidates+1 {
		t.Errorf("state %s, pinger starts %d, shaper invalidations %d", c.state, act.starts-starts, act.invalidates-invalidates)
	}
}

// The unshaped mode keeps the delay measurement: it leaves IDLE at once,
// and a link with low traffic does not go to IDLE. After the mode, the
// normal idle detection applies again.
func TestUnshapedPreventsIdle(t *testing.T) {
	c, act := newTestCore(t)
	c.cfg.EnableSleepFunction = true
	c.cfg.ConnectionActiveThrKbps = 2000
	c.cfg.SustainedIdleSleepThrS = 0.001
	c.state = StateIdle
	starts := act.starts
	now := time.Now()
	c.SetUnshaped(true, now)
	if c.state != StateRunning || act.starts != starts+1 || !c.Unshaped() || !c.Snapshot().Unshaped {
		t.Fatalf("state %s, pinger starts %d, status %+v", c.state, act.starts-starts, c.Snapshot())
	}
	for i := range 3 {
		c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: now.Add(time.Duration(i) * time.Second)})
	}
	if c.state != StateRunning {
		t.Errorf("unshaped with low traffic: got %s, want RUNNING", c.state)
	}
	c.SetUnshaped(false, now.Add(3*time.Second))
	for i := 4; i < 6; i++ {
		c.HandleRate(RateStats{DlRateKbps: 100, UlRateKbps: 100, Timestamp: now.Add(time.Duration(i) * time.Second)})
	}
	if c.state != StateIdle || c.Snapshot().Unshaped {
		t.Errorf("after the unshaped mode: got %s, status %+v", c.state, c.Snapshot())
	}
}

// Without shaping, a saturated upload has a low load against 10 Gbit/s:
// its queue must not go into the learned cross delay, also not after
// "shape".
func TestUnshapedDoesNotLearnCrossDelay(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.LearnCrossDelay = true
	c.link.Download.Adjust, c.link.Upload.Adjust = true, true
	c.state = StateRunning
	ts := time.Now()
	c.HandlePing(owdPing("1.1.1.1", ts, 10, 10))
	c.SetUnshaped(true, ts)
	c.SetFixedRate(DL, unshapedKbps)
	c.SetFixedRate(UL, unshapedKbps)
	ping := func(n int, ulMs float64) {
		for range n {
			ts = ts.Add(50 * time.Millisecond)
			c.HandlePing(owdPing("1.1.1.1", ts, 10+ulMs, 10))
		}
	}
	// A saturated upload of 10 Mbit/s with download activity.
	c.HandleRate(RateStats{Timestamp: ts, DlRateKbps: 50000, UlRateKbps: 10000})
	ping(200, 60)
	if c.ul.crossDelayUs != 0 {
		t.Fatalf("unshaped: learned %.0f us of cross delay", c.ul.crossDelayUs)
	}
	// After "shape", the low load starts again.
	c.SetPaused(DL, false)
	c.SetPaused(UL, false)
	c.SetUnshaped(false, ts)
	if !c.ul.lowLoadSince.IsZero() {
		t.Errorf("low load since %v after shape, want zero", c.ul.lowLoadSince)
	}
	ping(10, 60)
	if c.ul.crossDelayUs != 0 {
		t.Errorf("after shape: learned %.0f us at once", c.ul.crossDelayUs)
	}
}

// The status counts the ping replies that Core uses: not the timeouts,
// and not the replies outside RUNNING.
func TestSnapshotCountsReplies(t *testing.T) {
	c, _ := newTestCore(t)
	c.state = StateRunning
	ts := time.Now()
	for i := range 3 {
		c.HandlePing(owdPing("1.1.1.1", ts.Add(time.Duration(i)*time.Second), 10, 10))
	}
	c.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: ts.Add(4 * time.Second), Timeout: true})
	if got := c.Snapshot().Replies; got != 3 {
		t.Errorf("replies %d, want 3", got)
	}
	c.state = StateIdle
	c.HandlePing(owdPing("1.1.1.1", ts.Add(5*time.Second), 10, 10))
	if got := c.Snapshot().Replies; got != 3 {
		t.Errorf("replies in IDLE: %d, want 3", got)
	}
}
