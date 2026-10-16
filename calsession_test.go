package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock advances by step at each call of Now. Thus, a session with a
// fake clock is fast.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *fakeClock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// fakeCalTarget records the calls. At the start of each block, it writes
// the events of the whole block to the recorder: pings with one-way
// delays of 4 reflectors and the achieved rates of a link with 50 Mbit/s
// download and 20 Mbit/s upload capacity.
type fakeCalTarget struct {
	mu        sync.Mutex
	clock     *fakeClock
	times     calTimes
	cfg       *Config
	calls     []string
	blocks    int
	startErr  error
	finishErr error
	onFinish  func()
}

func newFakeCalTarget(clock *fakeClock, times calTimes) *fakeCalTarget {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links = cfg.Links[:1]
	cfg.Links[0].Name = "wan"
	return &fakeCalTarget{clock: clock, times: times, cfg: cfg}
}

func (f *fakeCalTarget) call(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeCalTarget) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCalTarget) CalConfig() *Config { return f.cfg }

func (f *fakeCalTarget) CalStart() error {
	f.call("start")
	return f.startErr
}

func (f *fakeCalTarget) CalBlock(rec *Recorder, unshaped bool) error {
	f.call(fmt.Sprintf("block unshaped=%v", unshaped))
	f.mu.Lock()
	b := calSessionBlocks[f.blocks]
	f.blocks++
	f.mu.Unlock()
	start := f.clock.peek()
	loadFrom, loadTo := start.Add(f.times.Idle), start.Add(f.times.Idle+f.times.Load)
	end := loadTo.Add(f.times.Tail)
	for i, t := 0, start; t.Before(end); i, t = i+1, t.Add(250*time.Millisecond) {
		r := fmt.Sprintf("10.0.0.%d", i%4+1)
		jitter := time.Duration(i%3) * time.Millisecond
		rec.Ping("wan", PingResult{Reflector: r, RTT: 20 * time.Millisecond, Timestamp: t,
			OWD: true, DlOWD: 10*time.Millisecond + jitter, UlOWD: 10*time.Millisecond + jitter})
	}
	for i, t := 0, start; t.Before(end); i, t = i+1, t.Add(time.Second) {
		var dl, ul float64
		if !t.Before(loadFrom) && t.Before(loadTo) {
			v := float64(i % 5)
			switch {
			case b.policy != "unshaped":
				dl, ul = 30000, 10000
			case b.mode == "dl":
				dl, ul = 48000+v*1000, 1000
			case b.mode == "ul":
				dl, ul = 500, 19000+v*500
			case b.mode == "both":
				dl, ul = 40000, 8000+v*500
			}
		}
		rec.Rate("wan", RateStats{DlRateKbps: dl, UlRateKbps: ul, Timestamp: t})
	}
	return nil
}

func (f *fakeCalTarget) CalStopRecording() error {
	f.call("stop recording")
	return nil
}

func (f *fakeCalTarget) CalFinish() error {
	f.call("finish")
	if f.onFinish != nil {
		f.onFinish()
	}
	return f.finishErr
}

// newTestCalibrator returns a calibrator with a fake clock and a fake
// target "wan".
func newTestCalibrator(t *testing.T, times calTimes) (*Calibrator, *fakeCalTarget, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 50 * time.Millisecond}
	target := newFakeCalTarget(clock, times)
	k := NewCalibrator(context.Background(), t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
	k.now, k.tick = clock.Now, time.Microsecond
	// The fake clock is fast: the test polls less often than the
	// session checks the heartbeat.
	k.heartbeat = time.Hour
	return k, target, clock
}

// waitCal polls the status until the session ends. It returns the loads
// that the status showed in each block and phase.
func waitCal(t *testing.T, k *Calibrator) (CalStatus, []string) {
	t.Helper()
	var seen []string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st := k.Status()
		if st.State != "running" {
			return st, seen
		}
		s := fmt.Sprintf("%d %s %s %q", st.Block, st.Mode, st.Phase, st.Load)
		if len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
		}
		time.Sleep(100 * time.Microsecond)
	}
	t.Fatal("the session did not end")
	return CalStatus{}, nil
}

func TestCalibrationSession(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, _ := newTestCalibrator(t, times)
	// The phases from the hook: a poll can miss a short phase.
	var mu sync.Mutex
	var seen []string
	k.onPhase = func(st CalStatus) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%d %s %s %q", st.Block, st.Mode, st.Phase, st.Load))
		mu.Unlock()
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	st, _ := waitCal(t, k)
	if st.State != "done" || st.Result == nil {
		t.Fatalf("state %s, error %q", st.State, st.Error)
	}

	// The calls: start, then each block with its recording, then finish.
	want := []string{"start"}
	for _, b := range calSessionBlocks {
		want = append(want, fmt.Sprintf("block unshaped=%v", b.policy == "unshaped"), "stop recording")
	}
	want = append(want, "finish")
	if got := target.Calls(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("calls:\n got %v\nwant %v", got, want)
	}

	// The load field asks for the load of the block only in the load
	// phase.
	for _, s := range seen {
		f := strings.Fields(s)
		load := strings.Trim(f[3], `"`)
		if (f[2] == "load") != (load != "") || (load != "" && load != f[1]) {
			t.Errorf("status %s: wrong load", s)
		}
	}
	for _, want := range []string{`1 dl load "dl"`, `3 ul load "ul"`, `5 both load "both"`, `2 dl idle ""`} {
		if !strings.Contains(strings.Join(seen, "\n"), want) {
			t.Errorf("status never showed %s: %v", want, seen)
		}
	}

	// The proposal comes from the analysis of the recordings.
	r := st.Result
	if r.Link != "wan" || r.Download.MaxRateKbps < 50000 || r.Download.MaxRateKbps > 53000 {
		t.Errorf("download: %+v", r.Download)
	}
	if r.Upload.MaxRateKbps < 20000 || r.Upload.MaxRateKbps > 21500 {
		t.Errorf("upload: %+v", r.Upload)
	}
	// With download load, the upload capacity is lower: the base rate.
	if r.Upload.BaseRateKbps > 9000 {
		t.Errorf("upload base with download load: %+v", r.Upload)
	}
	if r.PingMethod != "icmp-timestamp" || len(r.Reflectors) != 4 || r.Measured["dl_capacity_kbps"].N == 0 {
		t.Errorf("ping method %q, reflectors %v, measured %v", r.PingMethod, r.Reflectors, r.Measured)
	}

	// The files: the session directory and the result file.
	if len(seen) != 3*len(calSessionBlocks) {
		t.Errorf("phases: %v", seen)
	}
	data, err := os.ReadFile(filepath.Join(k.dir, "calibration", "sessions", "wan", "blocks.tsv"))
	if err != nil || strings.Count(string(data), "\n") != 1+len(calSessionBlocks) {
		t.Errorf("blocks.tsv: %v\n%s", err, data)
	}
	var file CalOutput
	data, err = os.ReadFile(filepath.Join(k.dir, "calibration", "results", "wan.json"))
	if err != nil || json.Unmarshal(data, &file) != nil || file.Download.MaxRateKbps != r.Download.MaxRateKbps {
		t.Errorf("result file: %v\n%s", err, data)
	}
}

func TestCalibrationStopsWithoutStatusRequests(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, _ := newTestCalibrator(t, times)
	k.heartbeat = 5 * time.Second
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	// No status request: wait for the end without a heartbeat.
	k.mu.Lock()
	done := k.done
	k.mu.Unlock()
	<-done
	st := k.Status()
	if st.State != "failed" || !strings.Contains(st.Error, "no status request") {
		t.Errorf("state %s, error %q", st.State, st.Error)
	}
	if calls := target.Calls(); calls[len(calls)-1] != "finish" {
		t.Errorf("the rate control must be restored: %v", calls)
	}
}

func TestCalibrationStop(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, clock := newTestCalibrator(t, times)
	clock.step = 0 // the session waits in the first phase
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	if err := k.Start("wan", times); err == nil || !strings.Contains(err.Error(), "runs") {
		t.Errorf("a second start: %v", err)
	}
	if err := k.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := k.Status(); st.State != "stopped" || st.Load != "" {
		t.Errorf("status after stop: %+v", st)
	}
	if calls := target.Calls(); calls[len(calls)-1] != "finish" {
		t.Errorf("the rate control must be restored: %v", calls)
	}
	if err := k.Stop(); err == nil {
		t.Error("a stop without a session must give an error")
	}
	// A new session can start after the stop.
	clock.mu.Lock()
	clock.step = 50 * time.Millisecond
	clock.mu.Unlock()
	target.blocks = 0
	if err := k.Start("wan", times); err != nil {
		t.Errorf("start after a stop: %v", err)
	}
	if st, _ := waitCal(t, k); st.State != "done" {
		t.Errorf("second session: %s %q", st.State, st.Error)
	}
}

func TestCalibrationStartErrors(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, _ := newTestCalibrator(t, times)
	if err := k.Start("nolink", times); err == nil || !strings.Contains(err.Error(), "unknown link") {
		t.Errorf("unknown link: %v", err)
	}
	target.startErr = errors.New("needs adjust")
	if err := k.Start("wan", times); err == nil || err.Error() != "needs adjust" {
		t.Errorf("target error: %v", err)
	}
	if st := k.Status(); st.State != "none" {
		t.Errorf("state after failed starts: %+v", st)
	}
}

type fakeCalControl struct {
	link  string
	times calTimes
	err   error
	stops int
}

func (f *fakeCalControl) Start(link string, times calTimes) error {
	f.link, f.times = link, times
	return f.err
}

func (f *fakeCalControl) Stop() error {
	f.stops++
	return f.err
}

func (f *fakeCalControl) Status() CalStatus {
	return CalStatus{State: "running", Link: "wan", Block: 2, Load: "dl"}
}

func TestHandleCalibrateCommand(t *testing.T) {
	cal := &fakeCalControl{}
	h := &ControlHandler{Calib: cal}
	for _, tc := range []struct {
		line, want string
		load       time.Duration
	}{
		{"calibrate start wan", "ok", calDefaultLoad},
		{"calibrate start wan 20", "ok", 20 * time.Second},
		{"calibrate start wan 120", "ok", 120 * time.Second},
		{"calibrate start wan 19", "error: load time", 0},
		{"calibrate start wan 121", "error: load time", 0},
		{"calibrate start wan x", "error: load time", 0},
		{"calibrate start", "error: usage", 0},
		{"calibrate", "error: usage", 0},
		{"calibrate go", "error: unknown calibrate command", 0},
		{"calibrate status x", "error: usage", 0},
		{"calibrate stop", "ok", 0},
		{"calibrate status", `{"state":"running","link":"wan","block":2,"load":"dl"`, 0},
	} {
		cal.times = calTimes{}
		got := handleControlCommand(tc.line, h)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want prefix %q", tc.line, got, tc.want)
		}
		if tc.load != 0 && cal.times != defaultCalTimes(tc.load) {
			t.Errorf("%q: times %+v", tc.line, cal.times)
		}
	}
	cal.err = errors.New("a calibration\\nruns")
	if got := handleControlCommand("calibrate start wan", h); got != `error: a calibration\nruns` {
		t.Errorf("error answer: %q", got)
	}
	if got := handleControlCommand("calibrate status", &ControlHandler{}); got != "error: no calibration" {
		t.Errorf("without a calibrator: %q", got)
	}
}

// A failed restore of the rate control is a failed session, also after a
// stop: the shaper can still have no limit.
func TestCalibrationRestoreFailure(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	k, target, clock := newTestCalibrator(t, times)
	clock.step = 0
	target.finishErr = errors.New("link does not answer")
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	if err := k.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := k.Status(); st.State != "failed" || !strings.Contains(st.Error, "restore the rate control: link does not answer") {
		t.Errorf("status: %+v", st)
	}

	// After another error, the status has both errors.
	clock.mu.Lock()
	clock.step = 50 * time.Millisecond
	clock.mu.Unlock()
	k.heartbeat = 5 * time.Second
	target.blocks = 0
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	if st := k.Status(); st.State != "failed" || !strings.Contains(st.Error, "restore the rate control") || !strings.Contains(st.Error, "no status request") {
		t.Errorf("status: %+v", st)
	}
}

// At the stop of the daemon, Wait returns after the session restored the
// rate control.
func TestCalibrationWaitAtShutdown(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	target := newFakeCalTarget(clock, times)
	ctx, cancel := context.WithCancel(context.Background())
	k := NewCalibrator(ctx, t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
	k.now, k.tick, k.heartbeat = clock.Now, time.Millisecond, time.Hour
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	cancel()
	k.Wait()
	if calls := target.Calls(); calls[len(calls)-1] != "finish" {
		t.Errorf("calls: %v", calls)
	}
	if st := k.Status(); st.State != "stopped" {
		t.Errorf("state: %s", st.State)
	}
	var none *Calibrator
	none.Wait() // no calibrator: returns at once
}

// After the start of the shutdown, a new session does not start: Wait
// would not wait for it.
func TestCalibrationNoStartAfterShutdown(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	target := newFakeCalTarget(clock, times)
	ctx, cancel := context.WithCancel(context.Background())
	k := NewCalibrator(ctx, t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
	cancel()
	if err := k.Start("wan", times); err == nil || !strings.Contains(err.Error(), "stops") {
		t.Errorf("start after the shutdown: %v", err)
	}
	if calls := target.Calls(); len(calls) != 0 {
		t.Errorf("the link must not change: %v", calls)
	}
}

// The links "wan" and "wan.json" have separate files, in both orders.
func TestCalibrationFilesOfSimilarLinks(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	for _, order := range [][]string{{"wan", "wan.json"}, {"wan.json", "wan"}} {
		clock := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 50 * time.Millisecond}
		targets := map[string]CalTarget{}
		for _, name := range order {
			targets[name] = newFakeCalTarget(clock, times)
		}
		k := NewCalibrator(context.Background(), t.TempDir(), targets, NewDiscardLogger())
		k.now, k.tick, k.heartbeat = clock.Now, time.Microsecond, time.Hour
		for _, name := range order {
			if err := k.Start(name, times); err != nil {
				t.Fatal(err)
			}
			if st, _ := waitCal(t, k); st.State != "done" {
				t.Fatalf("%v: %s: %s %q", order, name, st.State, st.Error)
			}
		}
		for _, name := range order {
			if _, err := os.Stat(filepath.Join(k.dir, "calibration", "results", name+".json")); err != nil {
				t.Errorf("%v: result of %s: %v", order, name, err)
			}
			if _, err := os.Stat(filepath.Join(k.dir, "calibration", "sessions", name, "blocks.tsv")); err != nil {
				t.Errorf("%v: session of %s: %v", order, name, err)
			}
		}
	}
}

// slowCalTarget needs 10 s (on the fake clock) to set up a block.
type slowCalTarget struct{ *fakeCalTarget }

func (s slowCalTarget) CalBlock(rec *Recorder, unshaped bool) error {
	err := s.fakeCalTarget.CalBlock(rec, unshaped)
	s.clock.mu.Lock()
	s.clock.t = s.clock.t.Add(10 * time.Second)
	s.clock.mu.Unlock()
	return err
}

// The idle time of a block starts after its setup.
func TestCalibrationIdleAfterSetup(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 50 * time.Millisecond}
	target := slowCalTarget{newFakeCalTarget(clock, times)}
	k := NewCalibrator(context.Background(), t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
	k.now, k.tick, k.heartbeat = clock.Now, time.Microsecond, time.Hour
	// The time left at the start of the last block: the whole block, also
	// after the setup time of the blocks before it.
	var lastLeft float64
	k.onPhase = func(st CalStatus) {
		if st.Block == len(calSessionBlocks) && st.Phase == "idle" {
			lastLeft = k.Status().LeftS
		}
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	waitCal(t, k)
	if block := (times.Idle + times.Load + times.Tail).Seconds(); lastLeft < block-1 || lastLeft > block+1 {
		t.Errorf("time left at the start of the last block: %.1f s, want %.0f s", lastLeft, block)
	}
	blocks, err := readBlocks(filepath.Join(k.dir, "calibration", "sessions", "wan"))
	if err != nil || len(blocks) != len(calSessionBlocks) {
		t.Fatalf("blocks: %v %v", blocks, err)
	}
	for _, b := range blocks {
		if idle := b.loadStart.Sub(b.start); idle < times.Idle {
			t.Errorf("block %d: idle time %v, want %v", b.num, idle, times.Idle)
		}
	}
}

// The link names "." and ".." are valid, but the session would use the
// parent directory: the calibration refuses them.
func TestCalibrationRefusesDotNames(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	targets := map[string]CalTarget{}
	fakes := map[string]*fakeCalTarget{}
	for _, name := range []string{".", ".."} {
		fakes[name] = newFakeCalTarget(clock, times)
		targets[name] = fakes[name]
	}
	k := NewCalibrator(context.Background(), t.TempDir(), targets, NewDiscardLogger())
	for name, f := range fakes {
		if err := k.Start(name, times); err == nil || !strings.Contains(err.Error(), "another link name") {
			t.Errorf("%q: %v", name, err)
		}
		if calls := f.Calls(); len(calls) != 0 {
			t.Errorf("%q: the link must not change: %v", name, calls)
		}
	}
}

// The rate control comes back before the analysis: the last block has no
// shaper limit. A stop during the analysis gives no result.
func TestCalibrationRestoresBeforeAnalysis(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 50 * time.Millisecond}
	target := newFakeCalTarget(clock, times)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k := NewCalibrator(ctx, t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
	k.now, k.tick, k.heartbeat = clock.Now, time.Microsecond, time.Hour
	result := filepath.Join(k.dir, "calibration", "results", "wan.json")
	target.onFinish = func() {
		if _, err := os.Stat(result); err == nil {
			t.Error("the restore must come before the result")
		}
		// A stop at this time: during the analysis.
		cancel()
	}
	if err := k.Start("wan", times); err != nil {
		t.Fatal(err)
	}
	k.Wait()
	if st := k.Status(); st.State != "stopped" || st.Result != nil {
		t.Errorf("status: %+v", st)
	}
	if _, err := os.Stat(result); err == nil {
		t.Error("a stopped session must not write a result")
	}
	n := 0
	for _, c := range target.Calls() {
		if c == "finish" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("restores: %d, want 1", n)
	}
}

// The calibration directory must be a directory of this user that other
// users cannot write to: else another user can place links in it.
func TestCalibrationDirectoryMustBePrivate(t *testing.T) {
	times := defaultCalTimes(30 * time.Second)
	for _, tc := range []struct {
		name  string
		setup func(dir string)
	}{
		{"link", func(dir string) { os.Symlink(t.TempDir(), filepath.Join(dir, "calibration")) }},
		{"writable", func(dir string) {
			p := filepath.Join(dir, "calibration")
			os.Mkdir(p, 0o777)
			os.Chmod(p, 0o777)
		}},
		{"writable sessions", func(dir string) {
			os.Mkdir(filepath.Join(dir, "calibration"), 0o700)
			p := filepath.Join(dir, "calibration", "sessions")
			os.Mkdir(p, 0o777)
			os.Chmod(p, 0o777)
		}},
	} {
		clock := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 50 * time.Millisecond}
		target := newFakeCalTarget(clock, times)
		k := NewCalibrator(context.Background(), t.TempDir(), map[string]CalTarget{"wan": target}, NewDiscardLogger())
		k.now, k.tick, k.heartbeat = clock.Now, time.Microsecond, time.Hour
		tc.setup(k.dir)
		if err := k.Start("wan", times); err != nil {
			t.Fatal(err)
		}
		k.Wait()
		if st := k.Status(); st.State != "failed" || !strings.Contains(st.Error, "other users cannot write") {
			t.Errorf("%s: status %+v", tc.name, st)
		}
	}
}
