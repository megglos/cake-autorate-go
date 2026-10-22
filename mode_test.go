package main

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The options of each mode. custom uses the config.
func TestLinkMode_Options(t *testing.T) {
	c, _ := newTestCore(t)
	c.cfg.PingMethod = PingMethodTimestamp
	c.cfg.RefractoryOnDecrease, c.cfg.IncreaseToDelayThr, c.cfg.AutoMaxRate = false, true, false
	for _, tc := range []struct {
		mode          LinkMode
		rod, inc, max bool
	}{
		{ModeLatency, false, false, false},
		{ModeBalanced, true, false, false},
		{ModeBandwidth, true, true, true},
		{ModeCustom, false, true, false},
	} {
		c.SetMode(tc.mode)
		if c.refractoryOnDecrease() != tc.rod || c.increaseToDelayThr() != tc.inc || c.autoMaxRate() != tc.max {
			t.Errorf("%s: refractory_on_decrease %v, increase_to_delay_thr %v, auto_max_rate %v; want %v %v %v",
				tc.mode, c.refractoryOnDecrease(), c.increaseToDelayThr(), c.autoMaxRate(), tc.rod, tc.inc, tc.max)
		}
	}
	// With icmp (RTT/2), balanced has no refractory_on_decrease; bandwidth
	// has it.
	c.cfg.PingMethod = PingMethodEcho
	c.SetMode(ModeBalanced)
	if c.refractoryOnDecrease() {
		t.Error("balanced with icmp: refractory_on_decrease must be off")
	}
	c.SetMode(ModeBandwidth)
	if !c.refractoryOnDecrease() {
		t.Error("bandwidth with icmp: refractory_on_decrease must be on")
	}
	// The ping method of the link applies.
	method := PingMethodTimestamp
	c.link.PingMethod = &method
	c.SetMode(ModeBalanced)
	if !c.refractoryOnDecrease() {
		t.Error("balanced with icmp-timestamp of the link: refractory_on_decrease must be on")
	}
	// The mode of the config in latency also turns the options off.
	c.cfg.RefractoryOnDecrease, c.cfg.AutoMaxRate = true, true
	c.SetMode(ModeLatency)
	if c.refractoryOnDecrease() || c.increaseToDelayThr() || c.autoMaxRate() {
		t.Error("latency must turn all options off")
	}
}

// A change from bandwidth to a mode without auto_max_rate removes the
// learned maximum and decreases the rate to max_rate_kbps.
func TestLinkMode_LeaveBandwidthRemovesLearnedMax(t *testing.T) {
	c, act := newTestCore(t)
	c.link.Download.Adjust = true
	c.SetMode(ModeBandwidth)
	cfgMax := float64(c.link.Download.MaxRateKbps)
	c.dl.shaperRateKbps = cfgMax
	runAtMax(c, time.Now(), 10*time.Second)
	if c.maxRateKbps(DL) <= cfgMax || c.dl.shaperRateKbps <= cfgMax {
		t.Fatalf("bandwidth: max %.0f, rate %.0f, want above %.0f", c.maxRateKbps(DL), c.dl.shaperRateKbps, cfgMax)
	}
	c.SetMode(ModeBalanced)
	if c.maxRateKbps(DL) != cfgMax || c.dl.shaperRateKbps != cfgMax || lastRate(act, DL) != int(cfgMax) {
		t.Errorf("balanced: max %.0f, rate %.0f, last rate %d, want %.0f", c.maxRateKbps(DL), c.dl.shaperRateKbps, lastRate(act, DL), cfgMax)
	}
}

// With balanced (refractory_on_decrease), one decrease in each refractory
// period; with latency, each delayed ping decreases the rate.
func TestLinkMode_RefractoryOnDecrease(t *testing.T) {
	decreases := func(m LinkMode) int {
		c, act := newTestCore(t)
		c.cfg.PingMethod = PingMethodTimestamp
		c.link.Download.Adjust = true
		c.SetMode(m)
		c.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)
		thrUs := c.link.Download.OWDDeltaDelayThrMs * 1000
		now := time.Now()
		for i := 0; i < 20; i++ {
			now = now.Add(50 * time.Millisecond)
			c.dl.achievedKbps = c.dl.shaperRateKbps
			c.dl.loadPercent = 1
			c.processDelay(DL, 3*thrUs, now)
		}
		n := 0
		r := act.rates[DL]
		for i := 1; i < len(r); i++ {
			if r[i] < r[i-1] {
				n++
			}
		}
		return n
	}
	lat, bal := decreases(ModeLatency), decreases(ModeBalanced)
	if bal >= lat || bal > 2 {
		t.Errorf("decreases in 1 s: latency %d, balanced %d; want fewer with balanced (at most 2)", lat, bal)
	}
}

func TestLoadConfig_Mode(t *testing.T) {
	load := func(t *testing.T, yaml string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := load(t, linkOverrideYAML)
	if cfg.Mode != string(ModeBalanced) || linkModeOf(cfg, &cfg.Links[0]) != ModeBalanced {
		t.Errorf("default mode %q, want balanced", cfg.Mode)
	}
	// A config from before the modes keeps its options.
	cfg = load(t, "refractory_on_decrease: true\n"+linkOverrideYAML)
	if cfg.Mode != string(ModeCustom) {
		t.Errorf("mode with refractory_on_decrease %q, want custom", cfg.Mode)
	}
	yaml := strings.Replace(linkOverrideYAML, "    learn_cross_delay: true\n", "    learn_cross_delay: true\n    mode: bandwidth\n", 1)
	cfg = load(t, "mode: latency\n"+yaml)
	if linkModeOf(cfg, &cfg.Links[0]) != ModeLatency || linkModeOf(cfg, &cfg.Links[1]) != ModeBandwidth {
		t.Errorf("modes %s %s, want latency bandwidth", linkModeOf(cfg, &cfg.Links[0]), linkModeOf(cfg, &cfg.Links[1]))
	}
	if cfg.ForLink(&cfg.Links[1]).Mode != string(ModeBandwidth) {
		t.Error("ForLink must apply the mode of the link")
	}
}

// In the mode custom, the options of the link override the top-level
// options. In the other modes, the mode sets them.
func TestLinkMode_CustomLinkOverrides(t *testing.T) {
	c, _ := newTestCore(t)
	on, off := true, false
	c.cfg.RefractoryOnDecrease, c.cfg.IncreaseToDelayThr, c.cfg.AutoMaxRate = true, false, false
	c.link.RefractoryOnDecrease, c.link.AutoMaxRate = &off, &on
	c.SetMode(ModeCustom)
	if c.refractoryOnDecrease() || c.increaseToDelayThr() || !c.autoMaxRate() {
		t.Errorf("custom: %v %v %v, want false false true", c.refractoryOnDecrease(), c.increaseToDelayThr(), c.autoMaxRate())
	}
	c.SetMode(ModeLatency)
	if c.autoMaxRate() {
		t.Error("latency: the link option must not apply")
	}
}

func TestLoadConfig_ModeOfLinkWithOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := strings.Replace(linkOverrideYAML, "    learn_cross_delay: true\n", "    learn_cross_delay: true\n    auto_max_rate: true\n", 1)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if linkModeOf(cfg, &cfg.Links[0]) != ModeBalanced || linkModeOf(cfg, &cfg.Links[1]) != ModeCustom {
		t.Errorf("modes %s %s, want balanced custom", linkModeOf(cfg, &cfg.Links[0]), linkModeOf(cfg, &cfg.Links[1]))
	}
	if !cfg.ForLink(&cfg.Links[1]).AutoMaxRate || cfg.AutoMaxRate {
		t.Error("auto_max_rate of the link must apply only to the link")
	}
}

func TestValidate_Mode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Mode = "fast"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("top-level mode: %v", err)
	}
	cfg.Mode = ""
	bad := "fast"
	cfg.Links[0].Mode = &bad
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("link mode: %v", err)
	}
}

func TestHandleControlCommand_Mode(t *testing.T) {
	a := &fakeLinkControl{}
	h := &ControlHandler{Names: []string{"a"}, Links: map[string]LinkControl{"a": a}}
	for _, tc := range []struct{ line, want string }{
		{"mode a bandwidth", "ok"},
		{"mode a latency 3600", "ok"},
		{"mode a custom 86400", "ok"},
		{"mode a", "error: usage: mode LINK"},
		{"mode a balanced 1 2", "error: usage: mode LINK"},
		{"mode a fast", `error: unknown mode "fast"`},
		{"mode a balanced 0", "error: SECONDS must be 1 to 86400"},
		{"mode a balanced 86401", "error: SECONDS must be 1 to 86400"},
		{"mode a balanced x", "error: SECONDS must be 1 to 86400"},
		{"mode x balanced", `error: unknown link "x"`},
	} {
		if got := handleControlCommand(tc.line, h); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.line, got, tc.want)
		}
	}
	want := []string{"bandwidth 0s", "latency 1h0m0s", "custom 24h0m0s"}
	if !slices.Equal(a.modes, want) {
		t.Errorf("calls %v, want %v", a.modes, want)
	}
}

// "mode LINK MODE SECONDS": the link goes back to the mode of the config
// after the time. The status shows the mode and the time left.
func TestModeTimeLimit(t *testing.T) {
	c, _ := testUnshapeController(t)
	c.cfg.Mode = string(ModeBalanced)
	c.core.Core().SetMode(ModeBalanced)
	if err := c.SetMode(ModeBandwidth, time.Hour); err != nil {
		t.Fatal(err)
	}
	c.publishStatus()
	if st := boardStatus(c); st.Mode != ModeBandwidth || st.ConfigMode != ModeBalanced || st.ModeLeftS < 3590 || st.ModeLeftS > 3600 {
		t.Errorf("status: mode %s, config mode %s, left %v s", st.Mode, st.ConfigMode, st.ModeLeftS)
	}
	runOnController(t, c, func(now time.Time) { c.checkModeTime(now.Add(59 * time.Minute)) })
	if m := c.core.Core().Mode(); m != ModeBandwidth {
		t.Fatalf("mode %s before the end of the time", m)
	}
	runOnController(t, c, func(now time.Time) { c.checkModeTime(now.Add(61 * time.Minute)) })
	if m := c.core.Core().Mode(); m != ModeBalanced {
		t.Errorf("mode %s after the time, want balanced", m)
	}
	c.publishStatus()
	if st := boardStatus(c); st.Mode != ModeBalanced || st.ModeLeftS != 0 {
		t.Errorf("status after the time: mode %s, left %v", st.Mode, st.ModeLeftS)
	}
	// Without a time, the mode stays.
	if err := c.SetMode(ModeLatency, 0); err != nil {
		t.Fatal(err)
	}
	runOnController(t, c, func(now time.Time) { c.checkModeTime(now.Add(48 * time.Hour)) })
	if m := c.core.Core().Mode(); m != ModeLatency {
		t.Errorf("mode %s, want latency without a time limit", m)
	}
}

func TestModeRefusedInCalibration(t *testing.T) {
	c, _ := testUnshapeController(t)
	runOnController(t, c, func(now time.Time) { c.core.SetCalibrating(true, now) })
	if err := c.SetMode(ModeBandwidth, 0); err == nil || !strings.Contains(err.Error(), "calibration") {
		t.Errorf("err %v, want a calibration error", err)
	}
}

// The run loop sets the mode of the config at the end of the time, with
// its own timer.
func TestRunLoopEndsModeTime(t *testing.T) {
	c := testPingerController(t)
	act := &fakeActuator{}
	c.cfg.Mode = string(ModeLatency)
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
	if err := c.SetMode(ModeBandwidth, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	mode := func() LinkMode {
		var m LinkMode
		if err := c.call(func(time.Time) error { m = c.core.Core().Mode(); return nil }); err != nil {
			t.Fatal(err)
		}
		return m
	}
	deadline := time.Now().Add(5 * time.Second)
	for mode() != ModeLatency {
		if time.Now().After(deadline) {
			t.Fatal("the mode did not go back after the time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A replay with mode changes gives the same decisions.
func TestRecording_ReplayMode(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Mode = string(ModeLatency)
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Now()
	rec, err := newRecorderAt(path, 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, testLogger(t))
	rc.Start(start)
	maxKbps := float64(cfg.Links[0].Download.MaxRateKbps)
	now := start
	step := func(n int, delayMs int64) {
		for i := 0; i < n; i++ {
			now = now.Add(50 * time.Millisecond)
			rc.HandleRate(RateStats{DlRateKbps: math.Min(rc.Core().ShaperRateKbps(DL), 3*maxKbps), Timestamp: now})
			owd := delayMs * 1000
			rc.HandlePing(PingResult{Reflector: cfg.Links[0].Reflectors[0], RTT: time.Duration(2*owd+20000) * time.Microsecond,
				OWD: true, DlOWD: time.Duration(owd+10000) * time.Microsecond, UlOWD: 10 * time.Millisecond, Timestamp: now})
		}
	}
	step(40, 0)
	rc.SetMode(ModeBandwidth, now)
	step(200, 0)
	step(20, 200)
	rc.SetMode(ModeBalanced, now)
	step(100, 0)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, replayed := replayRecording(t, path)
	if len(recorded) == 0 || !slices.Equal(recorded, replayed) {
		t.Errorf("recorded:\n%v\nreplay:\n%v", recorded, replayed)
	}
}

// The output of --defaults is a valid config with the mode balanced.
func TestPrintDefaults_ModeLoads(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	printDefaults()
	os.Stdout = stdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the defaults do not load: %v", err)
	}
	if cfg.Mode != string(ModeBalanced) {
		t.Errorf("mode %q, want balanced", cfg.Mode)
	}
}

// The end of the time of a mode during a calibration does not change the
// mode: it comes after the calibration.
func TestModeTimeEndsAfterCalibration(t *testing.T) {
	c, _ := testUnshapeController(t)
	c.cfg.Mode = string(ModeBalanced)
	c.core.Core().SetMode(ModeBalanced)
	if err := c.SetMode(ModeBandwidth, time.Minute); err != nil {
		t.Fatal(err)
	}
	runOnController(t, c, func(now time.Time) {
		c.core.SetCalibrating(true, now)
		c.checkModeTime(now.Add(2 * time.Minute))
	})
	if m := c.core.Core().Mode(); m != ModeBandwidth {
		t.Fatalf("mode %s during the calibration, want bandwidth", m)
	}
	runOnController(t, c, func(now time.Time) {
		c.core.SetCalibrating(false, now)
		c.checkModeTime(now.Add(2 * time.Minute))
	})
	if m := c.core.Core().Mode(); m != ModeBalanced {
		t.Errorf("mode %s after the calibration, want balanced", m)
	}
}

// testFailingModeController returns a controller in the mode bandwidth
// with a download rate above max_rate_kbps (a learned maximum).
func testFailingModeController(t *testing.T) (*LinkController, *failingActuator, int) {
	t.Helper()
	c := testPingerController(t)
	c.link.Download.Adjust = true
	c.cfg.Mode = string(ModeBalanced)
	act := &failingActuator{c: c}
	c.core = NewRecordingCore(c.link, c.cfg, defaultMTU, defaultMTU, act, nil, testLogger(t))
	runCommands(t, c)
	core := c.core.Core()
	runOnController(t, c, func(now time.Time) {
		c.core.SetMode(ModeBandwidth, now)
		core.dl.shaperRateKbps = float64(c.link.Download.MaxRateKbps)
		runAtMax(core, now, 10*time.Second)
	})
	if core.dl.shaperRateKbps <= float64(c.link.Download.MaxRateKbps) {
		t.Fatalf("rate %.0f, want above the maximum", core.dl.shaperRateKbps)
	}
	return c, act, c.link.Download.MaxRateKbps
}

// A failed rate change at a mode change: the same command again sends the
// rate again.
func TestModeRetryAfterRateError(t *testing.T) {
	c, act, maxKbps := testFailingModeController(t)
	act.fails = 1
	if err := c.SetMode(ModeBalanced, 0); err == nil {
		t.Fatal("want the rate error")
	}
	if err := c.SetMode(ModeBalanced, 0); err != nil {
		t.Fatal(err)
	}
	if got := lastRate(&act.fakeActuator, DL); got != maxKbps {
		t.Errorf("rate %d after the second command, want %d", got, maxKbps)
	}
}

// A failed rate change at the end of a timed mode: the controller tries
// again without a new command.
func TestModeTimeRetryAfterRateError(t *testing.T) {
	c, act, maxKbps := testFailingModeController(t)
	if err := c.SetMode(ModeBandwidth, time.Minute); err != nil {
		t.Fatal(err)
	}
	act.fails = 1
	runOnController(t, c, func(now time.Time) { c.checkModeTime(now.Add(2 * time.Minute)) })
	if c.modeUntil.IsZero() || c.modeTimer == nil {
		t.Fatal("the controller must try again after the rate error")
	}
	runOnController(t, c, func(now time.Time) { c.checkModeTime(now.Add(3 * time.Minute)) })
	if got := lastRate(&act.fakeActuator, DL); got != maxKbps || !c.modeUntil.IsZero() {
		t.Errorf("rate %d, time %v; want %d and no time", got, c.modeUntil, maxKbps)
	}
}

// A failed rate change at bandwidth -> balanced: a later change to latency
// sends the capped rate again.
func TestModeOtherModeAfterRateError(t *testing.T) {
	c, act, maxKbps := testFailingModeController(t)
	act.fails = 1
	if err := c.SetMode(ModeBalanced, 0); err == nil {
		t.Fatal("want the rate error")
	}
	if err := c.SetMode(ModeLatency, 0); err != nil {
		t.Fatal(err)
	}
	if got := lastRate(&act.fakeActuator, DL); got != maxKbps {
		t.Errorf("rate %d after latency, want %d", got, maxKbps)
	}
}

// The recordings of a calibration have the live mode of the link, and the
// config of the link stays the same.
func TestCalConfig_LiveMode(t *testing.T) {
	c, _ := testUnshapeController(t)
	c.cfg.Mode = string(ModeBalanced)
	c.core.Core().SetMode(ModeBalanced)
	if err := c.SetMode(ModeBandwidth, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.CalStart(); err != nil {
		t.Fatal(err)
	}
	cfg := c.CalConfig()
	if m := linkModeOf(cfg, &cfg.Links[0]); m != ModeBandwidth {
		t.Errorf("mode %s in the recording config, want bandwidth", m)
	}
	if c.link.Mode != nil || linkModeOf(c.cfg, c.link) != ModeBalanced {
		t.Errorf("the config of the link changed")
	}
}
