package main

import (
	"context"
	"testing"
)

// --- readInterfaceMTU tests ---

func TestReadInterfaceMTU_LoopbackInterface(t *testing.T) {
	logger := testLogger(t)
	mtu := readInterfaceMTU("lo", logger)
	// lo MTU is typically 65536 on Linux
	if mtu <= 0 {
		t.Errorf("expected positive MTU for lo, got %d", mtu)
	}
}

func TestReadInterfaceMTU_NonexistentInterface(t *testing.T) {
	logger := testLogger(t)
	mtu := readInterfaceMTU("nonexistent_iface_xyz", logger)
	if mtu != defaultMTU {
		t.Errorf("expected fallback to %d for nonexistent iface, got %d", defaultMTU, mtu)
	}
}

// --- pinger restart tests ---

// testPingerController returns a controller whose run context is already
// cancelled. Thus, the pingers that it starts stop at once and send no
// packets.
func testPingerController(t *testing.T) *LinkController {
	t.Helper()
	cfg := testConfig()
	cfg.PingMethod = PingMethodTimestamp
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return &LinkController{
		name:      cfg.Links[0].Name,
		link:      &cfg.Links[0],
		cfg:       cfg,
		pingerMgr: NewPingerManager(&cfg.Links[0], cfg, testLogger(t)),
		logger:    testLogger(t),
		pingCh:    make(chan PingResult, 100),
		runCtx:    ctx,
	}
}

func TestStartPingers_StopsRunningSet(t *testing.T) {
	c := testPingerController(t)
	running, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.pingerCancel = cancel

	c.StartPingers()

	if running.Err() == nil {
		t.Error("the pingers that ran before StartPingers did not stop")
	}
	if c.pingerCancel == nil {
		t.Error("StartPingers did not keep the cancel function of the new pingers")
	}
}

func TestRestartPingers_DoesNotStartStoppedPingers(t *testing.T) {
	c := testPingerController(t)

	// The Core stopped the pingers (IDLE or STALL). A reflector replacement
	// must not start them: the Core starts them again when it goes back to
	// RUNNING, and the pingers started here would then never stop.
	c.restartPingers()

	if c.pingerCancel != nil {
		t.Error("restartPingers started the pingers that the Core stopped")
	}
}

func TestRestartPingers_RestartsRunningPingers(t *testing.T) {
	c := testPingerController(t)
	running, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.pingerCancel = cancel

	c.restartPingers()

	if running.Err() == nil {
		t.Error("restartPingers did not stop the old pingers")
	}
	if c.pingerCancel == nil {
		t.Error("restartPingers did not start the new pingers")
	}
}
