package main

import (
	"context"
	"errors"
	"os"
	"strings"
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

// runCommands runs the commands of the controller like Run, until the test
// ends.
func runCommands(t *testing.T, c *LinkController) {
	t.Helper()
	c.ctrlCh = make(chan *ctrlCmd)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case cmd := <-c.ctrlCh:
				cmd.err = cmd.fn(time.Now())
				close(cmd.done)
			case <-stop:
				return
			}
		}
	}()
}

// During a calibration, a pause or a resume would change the shaper of the
// block: the controller refuses them.
func TestSetPausedRefusedDuringCalibration(t *testing.T) {
	c := testPingerController(t)
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, &fakeActuator{}, nil, testLogger(t))
	runCommands(t, c)
	c.core.SetCalibrating(true, time.Now())
	if err := c.SetPaused([]Direction{DL}, true); err == nil || !strings.Contains(err.Error(), "calibration runs") {
		t.Errorf("pause during a calibration: %v", err)
	}
	if c.core.Core().Paused(DL) {
		t.Error("the pause must not apply")
	}
	c.core.SetCalibrating(false, time.Now())
	if err := c.SetPaused([]Direction{DL}, true); err != nil || !c.core.Core().Paused(DL) {
		t.Errorf("pause after the calibration: %v", err)
	}
}

// A calibration command waits until the controller has run it, also when
// it takes longer than the timeout of the other commands.
func TestCallUntilDoneWaitsForSlowCommand(t *testing.T) {
	c := testPingerController(t)
	runCommands(t, c)
	err := c.callUntilDone(func(time.Time) error {
		time.Sleep(2500 * time.Millisecond)
		return errors.New("done")
	}, time.Second)
	if err == nil || err.Error() != "done" {
		t.Errorf("got %v, want the result of the command", err)
	}
	// Nobody takes the command: the dispatch times out.
	idle := testPingerController(t)
	idle.ctrlCh = make(chan *ctrlCmd)
	if err := idle.callUntilDone(func(time.Time) error { return nil }, 50*time.Millisecond); err == nil {
		t.Error("a command without a controller must time out")
	}
}

// A calibration block starts the pingers of a stalled link.
func TestCalBlockLeavesStall(t *testing.T) {
	c := testPingerController(t)
	act := &fakeActuator{}
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, act, nil, testLogger(t))
	runCommands(t, c)
	c.core.SetCalibrating(true, time.Now())
	c.core.Core().state = StateStall
	if err := c.CalBlock(nil, true); err != nil {
		t.Fatal(err)
	}
	if st := c.core.Core().State(); st != StateRunning {
		t.Errorf("state after the block start: %s", st)
	}
}

// testUnshapeController returns a controller with rate control in both
// directions, a fake actuator and a status board.
func testUnshapeController(t *testing.T) (*LinkController, *fakeActuator) {
	t.Helper()
	c := testPingerController(t)
	c.link.Download.Adjust, c.link.Upload.Adjust = true, true
	act := &fakeActuator{}
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, act, nil, testLogger(t))
	c.board = NewStatusBoard("test", time.Now(), []string{c.name})
	runCommands(t, c)
	return c, act
}

// boardStatus returns the status of the link on the status board.
func boardStatus(c *LinkController) LinkStatus {
	for _, st := range c.board.File(time.Now()).Links {
		if st.Name == c.name {
			return st
		}
	}
	return LinkStatus{}
}

// lastRate returns the last rate that the controller set for dir.
func lastRate(act *fakeActuator, dir Direction) int {
	r := act.rates[dir]
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1]
}

// Without shaping, both directions have no useful shaper limit and no rate
// control. The status shows it. Then the shaping comes back with the pause
// state from before.
func TestSetUnshaped(t *testing.T) {
	c, act := testUnshapeController(t)
	if err := c.SetPaused([]Direction{UL}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	core := c.core.Core()
	for _, dir := range Directions() {
		if lastRate(act, dir) != unshapedKbps || !core.Paused(dir) {
			t.Errorf("%s: rate %d, paused %v", dir, lastRate(act, dir), core.Paused(dir))
		}
	}
	c.publishStatus()
	if !boardStatus(c).Unshaped {
		t.Error("the status must show the unshaped link")
	}
	// A second "unshape" sets the rates again (after an error, the user
	// sends the command again). It keeps the pause states from before.
	n := len(act.rates[DL])
	if err := c.SetUnshaped(true, 0); err != nil || len(act.rates[DL]) != n+1 || lastRate(act, DL) != unshapedKbps {
		t.Errorf("second unshape: %v, %d rate changes", err, len(act.rates[DL])-n)
	}

	if err := c.SetUnshaped(false, 0); err != nil {
		t.Fatal(err)
	}
	if lastRate(act, DL) != c.link.Download.BaseRateKbps || core.Paused(DL) {
		t.Errorf("dl after shape: rate %d, paused %v", lastRate(act, DL), core.Paused(DL))
	}
	if lastRate(act, UL) != c.link.Upload.BaseRateKbps || !core.Paused(UL) {
		t.Errorf("ul after shape: rate %d, paused %v (must stay paused)", lastRate(act, UL), core.Paused(UL))
	}
	c.publishStatus()
	if boardStatus(c).Unshaped {
		t.Error("the status must show the shaped link")
	}
}

// While the shaping is off, a pause, a resume and a calibration would
// change the shaper: the controller refuses them. During a calibration,
// the controller refuses "unshape".
func TestUnshapedRefusesOtherCommands(t *testing.T) {
	c, _ := testUnshapeController(t)
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPaused([]Direction{DL}, false); err == nil || !strings.Contains(err.Error(), "shaping is off") {
		t.Errorf("resume without shaping: %v", err)
	}
	if err := c.CalStart(); err == nil || !strings.Contains(err.Error(), "shaping is off") {
		t.Errorf("calibration without shaping: %v", err)
	}
	if c.core.Core().Calibrating() {
		t.Error("the calibration must not start")
	}
	if err := c.SetUnshaped(false, 0); err != nil {
		t.Fatal(err)
	}
	c.core.SetCalibrating(true, time.Now())
	if err := c.SetUnshaped(true, 0); err == nil || !strings.Contains(err.Error(), "calibration runs") {
		t.Errorf("unshape during a calibration: %v", err)
	}
	if c.core.Core().Unshaped() {
		t.Error("the link must stay shaped")
	}
}

// A direction without rate control keeps its shaper rate.
func TestSetUnshapedOnlyAdjustedDirections(t *testing.T) {
	c, act := testUnshapeController(t)
	c.link.Upload.Adjust = false
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	if lastRate(act, DL) != unshapedKbps || len(act.rates[UL]) != 0 {
		t.Errorf("rates: %v", act.rates)
	}
	c.link.Download.Adjust = false
	c.core.SetUnshaped(false, time.Now())
	if err := c.SetUnshaped(true, 0); err == nil {
		t.Error("a link without rate control cannot be unshaped")
	}
}

// The program shapes an unshaped link again before it stops.
func TestStopShapesAgain(t *testing.T) {
	c, act := testUnshapeController(t)
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	c.stop(time.Now())
	if c.core.Core().Unshaped() || lastRate(act, DL) != c.link.Download.BaseRateKbps || lastRate(act, UL) != c.link.Upload.BaseRateKbps {
		t.Errorf("after the stop: unshaped %v, rates %v", c.core.Core().Unshaped(), act.rates)
	}
}

// "unshape" on an idle link starts the pings at once.
func TestSetUnshapedWakesIdleLink(t *testing.T) {
	c, act := testUnshapeController(t)
	c.core.Core().state = StateIdle
	starts := act.starts
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	if st := c.core.Core().State(); st != StateRunning || act.starts != starts+1 {
		t.Errorf("state %s, pinger starts %d", st, act.starts-starts)
	}
}

// "unshape LINK SECONDS" shapes the link again after the time. The status
// shows the time left.
func TestUnshapeTimeLimit(t *testing.T) {
	c, act := testUnshapeController(t)
	if err := c.SetUnshaped(true, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	c.publishStatus()
	if st := boardStatus(c); !st.Unshaped || st.UnshapedLeftS < 25 || st.UnshapedLeftS > 30 {
		t.Errorf("status: unshaped %v, left %v s", st.Unshaped, st.UnshapedLeftS)
	}
	runOnController(t, c, func(now time.Time) { c.checkUnshapeTime(now.Add(29 * time.Second)) })
	if !c.core.Core().Unshaped() {
		t.Fatal("shaped before the end of the time")
	}
	runOnController(t, c, func(now time.Time) { c.checkUnshapeTime(now.Add(31 * time.Second)) })
	if c.core.Core().Unshaped() || lastRate(act, DL) != c.link.Download.BaseRateKbps {
		t.Errorf("after the time: unshaped %v, rates %v", c.core.Core().Unshaped(), act.rates)
	}
	c.publishStatus()
	if st := boardStatus(c); st.Unshaped || st.UnshapedLeftS != 0 {
		t.Errorf("status after the time: %+v", st)
	}

	// Without a time, the link stays unshaped. A second "unshape" with a
	// time sets the time.
	if err := c.SetUnshaped(true, 0); err != nil {
		t.Fatal(err)
	}
	runOnController(t, c, func(now time.Time) { c.checkUnshapeTime(now.Add(24 * time.Hour)) })
	if !c.core.Core().Unshaped() {
		t.Fatal("unshape without a time must stay")
	}
	if err := c.SetUnshaped(true, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	runOnController(t, c, func(now time.Time) { c.checkUnshapeTime(now.Add(11 * time.Second)) })
	if c.core.Core().Unshaped() {
		t.Error("the second unshape must set the time")
	}
}

// runOnController runs fn on the goroutine of the controller (runCommands).
func runOnController(t *testing.T, c *LinkController, fn func(now time.Time)) {
	t.Helper()
	if err := c.call(func(now time.Time) error { fn(now); return nil }); err != nil {
		t.Fatal(err)
	}
}

// The run loop shapes the link again at the end of the time of "unshape
// LINK SECONDS", with its own timer: also without health and status ticks.
func TestRunLoopEndsUnshapeTime(t *testing.T) {
	c := testPingerController(t)
	c.link.Download.Adjust, c.link.Upload.Adjust = true, true
	act := &fakeActuator{}
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, act, nil, testLogger(t))
	c.ctrlCh = make(chan *ctrlCmd)
	c.rateCh = make(chan RateStats)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.loop(ctx, nil, nil) }()
	defer func() {
		cancel()
		<-done
	}()
	if err := c.SetUnshaped(true, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	unshaped := func() bool {
		var u bool
		if err := c.call(func(time.Time) error { u = c.core.Core().Unshaped(); return nil }); err != nil {
			t.Fatal(err)
		}
		return u
	}
	if !unshaped() {
		t.Fatal("the link must be unshaped")
	}
	deadline := time.Now().Add(5 * time.Second)
	for unshaped() {
		if time.Now().After(deadline) {
			t.Fatal("the shaping did not come back after the time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// failingActuator is a fakeActuator whose next fails rate changes fail
// (as SetRate of the controller with a tc error).
type failingActuator struct {
	fakeActuator
	c     *LinkController
	fails int
}

func (f *failingActuator) SetRate(dir Direction, rateKbps int) {
	if f.fails > 0 {
		f.fails--
		f.c.rateErr = errors.New("tc failed")
		return
	}
	f.fakeActuator.SetRate(dir, rateKbps)
}

// If the shaping does not come back at the end of the time of "unshape"
// (a rate error), the controller tries again without a new command.
func TestRunLoopRetriesShapeAfterError(t *testing.T) {
	old := unshapeRetry
	unshapeRetry = 50 * time.Millisecond
	t.Cleanup(func() { unshapeRetry = old })
	c := testPingerController(t)
	c.link.Download.Adjust, c.link.Upload.Adjust = true, true
	act := &failingActuator{c: c}
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, act, nil, testLogger(t))
	c.ctrlCh = make(chan *ctrlCmd)
	c.rateCh = make(chan RateStats)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.loop(ctx, nil, nil) }()
	defer func() {
		cancel()
		<-done
	}()
	if err := c.SetUnshaped(true, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The first try to shape the link again fails.
	if err := c.call(func(time.Time) error { act.fails = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	var unshaped bool
	var dl int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := c.call(func(time.Time) error {
			unshaped, dl = c.core.Core().Unshaped(), lastRate(&act.fakeActuator, DL)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !unshaped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shaping did not come back after the failed try")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dl != c.link.Download.BaseRateKbps {
		t.Errorf("download rate %d, want the base rate", dl)
	}
}
