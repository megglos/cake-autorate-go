package main

import (
	"context"
	"testing"
	"time"
)

// testLogger returns a silent logger for tests.
func testLogger(t *testing.T) *Logger {
	t.Helper()
	l, err := NewLogger(false, "", 0)
	if err != nil {
		t.Fatalf("creating test logger: %v", err)
	}
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

	ts := time.Now()
	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		RTT:       10 * time.Millisecond,
		Timestamp: ts,
		Timeout:   false,
	})

	if !c.lastPingTime.IsZero() {
		t.Error("ping result should be ignored when not in StateRunning")
	}
}

func TestHandleTimeout_TriggersStallWhenNoRecentPings(t *testing.T) {
	c := newTestController(t)
	c.state = StateRunning
	c.cfg.GlobalPingResponseTimeoutS = 0.001 // 1ms for fast test

	// Set lastPingTime far in the past
	c.lastPingTime = time.Now().Add(-10 * time.Second)

	c.handlePingResult(PingResult{
		Reflector: "1.1.1.1",
		Timestamp: time.Now(),
		Timeout:   true,
	})

	if c.state != StateStall {
		t.Errorf("expected StateStall after timeout with old lastPingTime, got %s", c.state)
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
