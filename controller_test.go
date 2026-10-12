package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
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

// This test needs CAP_NET_RAW. Without it, the test is skipped. To run it
// without root:
//
//	unshare -rn sh -c 'ip link set lo up && go test -run TestPingers_RestartWithLivePingers -v'
func TestPingers_RestartWithLivePingers(t *testing.T) {
	conn, err := listenICMP(context.Background(), "", "")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
			t.Skip("CAP_NET_RAW is necessary")
		}
		t.Fatal(err)
	}
	conn.Close()

	c := testPingerController(t)
	c.cfg.PingIntervalMs = 20
	c.cfg.PingerCount = 1
	c.link.Reflectors = []string{"127.0.0.1"}
	c.pingerMgr = NewPingerManager(c.link, c.cfg, c.logger)
	c.pingCh = make(chan PingResult, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.runCtx = ctx

	waitResult := func() {
		t.Helper()
		select {
		case <-c.pingCh:
		case <-time.After(2 * time.Second):
			t.Fatal("no ping result in 2 s")
		}
	}

	c.StartPingers()
	waitResult()
	for range 3 {
		c.restartPingers()
		waitResult()
	}
	c.StopPingers()

	if c.pingerMgr.GetState("127.0.0.1").Active {
		t.Error("the reflector is active after StopPingers returned")
	}
	for len(c.pingCh) > 0 {
		<-c.pingCh
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(c.pingCh); n > 0 {
		t.Errorf("got %d ping results after StopPingers returned, want 0", n)
	}
}
