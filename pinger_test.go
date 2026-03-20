package main

import (
	"testing"
)

func testPingerManager(t *testing.T) *PingerManager {
	t.Helper()
	cfg := testConfig()
	logger := testLogger(t)
	return NewPingerManager(&cfg.Links[0], cfg, logger)
}

func TestNewPingerManager_InitializesStates(t *testing.T) {
	pm := testPingerManager(t)

	if len(pm.states) != 4 {
		t.Errorf("expected 4 reflector states, got %d", len(pm.states))
	}
	for addr, state := range pm.states {
		if state == nil {
			t.Errorf("nil state for reflector %s", addr)
			continue
		}
		if state.Active {
			t.Errorf("reflector %s should not be active before Run", addr)
		}
		if len(state.MissedWindow) != pm.cfg.ReflectorMisbehavingDetectionWindow {
			t.Errorf("wrong MissedWindow size for %s: got %d, want %d",
				addr, len(state.MissedWindow), pm.cfg.ReflectorMisbehavingDetectionWindow)
		}
	}
}

func TestActiveReflectors_RespectsCount(t *testing.T) {
	pm := testPingerManager(t)
	pm.cfg.PingerCount = 2

	active := pm.ActiveReflectors()
	if len(active) != 2 {
		t.Errorf("expected 2 active reflectors, got %d", len(active))
	}
}

func TestActiveReflectors_CapsAtAvailable(t *testing.T) {
	pm := testPingerManager(t)
	pm.cfg.PingerCount = 100 // more than available

	active := pm.ActiveReflectors()
	if len(active) != 4 {
		t.Errorf("expected 4 active reflectors (all available), got %d", len(active))
	}
}

func TestReplaceUnhealthy_NoReplacementWhenHealthy(t *testing.T) {
	pm := testPingerManager(t)
	pm.cfg.PingerCount = 2
	pm.cfg.ReflectorMisbehavingDetectionThr = 3

	replaced := pm.ReplaceUnhealthy()
	if replaced {
		t.Error("should not replace when all reflectors are healthy")
	}
}

func TestReplaceUnhealthy_ReplacesWhenThresholdExceeded(t *testing.T) {
	pm := testPingerManager(t)
	pm.cfg.PingerCount = 2
	pm.cfg.ReflectorMisbehavingDetectionThr = 2
	pm.cfg.ReflectorMisbehavingDetectionWindow = 4

	// Re-init states with correct window size
	for addr := range pm.states {
		pm.states[addr] = &ReflectorState{
			MissedWindow: make([]bool, 4),
		}
	}

	// Mark the first active reflector as misbehaving
	firstActive := pm.reflectors[0]
	state := pm.states[firstActive]
	state.MissedWindow[0] = true
	state.MissedWindow[1] = true // 2 misses >= threshold of 2

	replaced := pm.ReplaceUnhealthy()
	if !replaced {
		t.Fatal("expected replacement when threshold exceeded")
	}

	// The misbehaving reflector should now be at the end
	if pm.reflectors[len(pm.reflectors)-1] != firstActive {
		t.Error("misbehaving reflector should be moved to end")
	}

	// The misbehaving reflector should be marked inactive
	if state.Active {
		t.Error("replaced reflector should be marked inactive")
	}
}

func TestReplaceUnhealthy_ConsecutiveMisbehaving(t *testing.T) {
	// Regression test: when two consecutive active reflectors are both
	// misbehaving, the second one should not be skipped after the first
	// is replaced (forward iteration with slice mutation).
	cfg := testConfig()
	cfg.PingerCount = 2
	cfg.ReflectorMisbehavingDetectionThr = 2
	cfg.ReflectorMisbehavingDetectionWindow = 4
	logger := testLogger(t)
	pm := NewPingerManager(&cfg.Links[0], cfg, logger)

	for addr := range pm.states {
		pm.states[addr] = &ReflectorState{
			MissedWindow: make([]bool, 4),
		}
	}

	// Mark both active reflectors as misbehaving
	first := pm.reflectors[0]
	second := pm.reflectors[1]
	pm.states[first].MissedWindow[0] = true
	pm.states[first].MissedWindow[1] = true
	pm.states[second].MissedWindow[0] = true
	pm.states[second].MissedWindow[1] = true

	replaced := pm.ReplaceUnhealthy()
	if !replaced {
		t.Fatal("expected replacements")
	}

	// Both misbehaving reflectors should be moved to the end
	n := len(pm.reflectors)
	tail := pm.reflectors[n-2:]
	if !((tail[0] == first && tail[1] == second) || (tail[0] == second && tail[1] == first)) {
		t.Errorf("both misbehaving reflectors should be at end, got tail: %v (first=%s, second=%s)",
			tail, first, second)
	}

	// Both should be marked inactive
	if pm.states[first].Active {
		t.Errorf("first misbehaving reflector should be inactive")
	}
	if pm.states[second].Active {
		t.Errorf("second misbehaving reflector should be inactive")
	}
}

func TestReplaceUnhealthy_NoReplacementWhenNoSpares(t *testing.T) {
	cfg := testConfig()
	cfg.PingerCount = 4 // same as number of reflectors — no spares
	cfg.ReflectorMisbehavingDetectionThr = 1
	cfg.ReflectorMisbehavingDetectionWindow = 4
	logger := testLogger(t)

	pm := NewPingerManager(&cfg.Links[0], cfg, logger)

	// Mark first reflector as misbehaving
	first := pm.reflectors[0]
	state := pm.states[first]
	state.MissedWindow[0] = true

	replaced := pm.ReplaceUnhealthy()
	if replaced {
		t.Error("should not replace when no spare reflectors exist")
	}
}

func TestRecordHealth_SlidingWindow(t *testing.T) {
	pm := testPingerManager(t)
	reflector := pm.reflectors[0]

	// Record alternating hits and misses
	pm.recordHealth(reflector, true)
	pm.recordHealth(reflector, false)
	pm.recordHealth(reflector, true)

	state := pm.states[reflector]
	state.mu.Lock()
	missed := 0
	for _, m := range state.MissedWindow {
		if m {
			missed++
		}
	}
	idx := state.MissedIdx
	state.mu.Unlock()

	if missed != 2 {
		t.Errorf("expected 2 misses recorded, got %d", missed)
	}
	if idx != 3 {
		t.Errorf("expected MissedIdx=3 after 3 records, got %d", idx)
	}
}

func TestRecordHealth_Wraps(t *testing.T) {
	cfg := testConfig()
	cfg.ReflectorMisbehavingDetectionWindow = 3
	logger := testLogger(t)
	pm := NewPingerManager(&cfg.Links[0], cfg, logger)
	reflector := pm.reflectors[0]

	// Record more entries than window size
	pm.recordHealth(reflector, true)
	pm.recordHealth(reflector, true)
	pm.recordHealth(reflector, true)
	pm.recordHealth(reflector, false) // wraps, overwrites first entry

	state := pm.states[reflector]
	state.mu.Lock()
	missed := 0
	for _, m := range state.MissedWindow {
		if m {
			missed++
		}
	}
	state.mu.Unlock()

	if missed != 2 {
		t.Errorf("expected 2 misses after wrap, got %d", missed)
	}
}

func TestUpdateBaseline_InitializesOnFirst(t *testing.T) {
	pm := testPingerManager(t)
	reflector := pm.reflectors[0]

	pm.UpdateBaseline(reflector, 5000)

	state := pm.states[reflector]
	state.mu.Lock()
	baseline := state.BaselineRTT
	state.mu.Unlock()

	if baseline != 5000 {
		t.Errorf("expected baseline 5000, got %f", baseline)
	}
}

func TestUpdateBaseline_TracksDecrease(t *testing.T) {
	pm := testPingerManager(t)
	reflector := pm.reflectors[0]

	pm.UpdateBaseline(reflector, 10000) // init
	pm.UpdateBaseline(reflector, 5000)  // decrease

	state := pm.states[reflector]
	state.mu.Lock()
	baseline := state.BaselineRTT
	state.mu.Unlock()

	// With alpha_baseline_decrease=0.9, new baseline = 0.9*5000 + 0.1*10000 = 5500
	if baseline < 5000 || baseline > 10000 {
		t.Errorf("baseline after decrease should be between 5000 and 10000, got %f", baseline)
	}
}

func TestUpdateBaseline_TracksIncreaseSlowly(t *testing.T) {
	pm := testPingerManager(t)
	reflector := pm.reflectors[0]

	pm.UpdateBaseline(reflector, 5000)  // init
	pm.UpdateBaseline(reflector, 10000) // increase

	state := pm.states[reflector]
	state.mu.Lock()
	baseline := state.BaselineRTT
	state.mu.Unlock()

	// With alpha_baseline_increase=0.001, baseline barely moves
	if baseline < 5000 || baseline > 5100 {
		t.Errorf("baseline should move slowly upward, got %f", baseline)
	}
}

func TestUpdateDeltaEWMA(t *testing.T) {
	pm := testPingerManager(t)
	reflector := pm.reflectors[0]

	pm.UpdateDeltaEWMA(reflector, 1000)

	state := pm.states[reflector]
	state.mu.Lock()
	ewma := state.DeltaEWMA
	state.mu.Unlock()

	// alpha_delta_ewma=0.095 → ewma = 0.095*1000 + 0.905*0 = 95
	expected := pm.cfg.AlphaDeltaEWMA * 1000
	if ewma < expected-1 || ewma > expected+1 {
		t.Errorf("expected DeltaEWMA ~%.1f, got %.1f", expected, ewma)
	}
}

func TestGetState_UnknownReflector(t *testing.T) {
	pm := testPingerManager(t)
	state := pm.GetState("192.168.1.1") // not in reflector list
	if state != nil {
		t.Error("expected nil for unknown reflector")
	}
}
