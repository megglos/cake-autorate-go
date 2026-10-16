// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// readRecording returns the lines of a recording as JSON objects.
func readRecording(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = gz
	}
	var events []map[string]any
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		ev := make(map[string]any)
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %d: %v", len(events)+1, err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestRecorder_WritesHeaderAndEvents(t *testing.T) {
	for _, name := range []string{"rec.jsonl", "rec.jsonl.gz"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			rec, err := newRecorderAt(path, 0, testConfig(), start, testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			rec.Start("test", start)
			rec.Ping("test", PingResult{Reflector: "1.1.1.1", RTT: 12345 * time.Microsecond, Timestamp: start.Add(time.Second)})
			rec.Ping("test", PingResult{Reflector: "8.8.8.8", Timestamp: start.Add(2 * time.Second), Timeout: true})
			rec.Rate("test", RateStats{DlRateKbps: 0, UlRateKbps: 1.5, Timestamp: start.Add(3 * time.Second)})
			rec.ReflectorReset("test", "9.9.9.9", start.Add(4*time.Second))
			rec.Shaper("test", UL, 20000, start.Add(5*time.Second))
			rec.State("test", StateIdle, start.Add(6*time.Second))
			if err := rec.Close(); err != nil {
				t.Fatal(err)
			}

			events := readRecording(t, path)
			if len(events) != 8 {
				t.Fatalf("expected 8 lines, got %d", len(events))
			}
			h := events[0]
			if h["type"] != recHeader || h["format_version"] != float64(recordFormatVersion) || h["config"] == nil {
				t.Errorf("unexpected header: %v", h)
			}
			want := []string{
				`map[link:test t:0 type:start]`,
				`map[link:test reflector:1.1.1.1 rtt_us:12345 t:1e+06 type:ping]`,
				`map[link:test reflector:8.8.8.8 rtt_us:0 t:2e+06 timeout:true type:ping]`,
				`map[dl_kbps:0 link:test t:3e+06 type:rate ul_kbps:1.5]`,
				`map[link:test reflector:9.9.9.9 t:4e+06 type:reflector_reset]`,
				`map[dir:ul link:test rate_kbps:20000 t:5e+06 type:shaper]`,
				`map[link:test state:IDLE t:6e+06 type:state]`,
			}
			for i, w := range want {
				if got := fmt.Sprint(events[i+1]); got != w {
					t.Errorf("line %d:\n got %s\nwant %s", i+2, got, w)
				}
			}
		})
	}
}

func TestRecorder_NilIsNoOp(t *testing.T) {
	var rec *Recorder
	now := time.Now()
	rec.Start("x", now)
	rec.Ping("x", PingResult{Timestamp: now})
	rec.Rate("x", RateStats{Timestamp: now})
	rec.ReflectorReset("x", "r", now)
	rec.Shaper("x", DL, 1, now)
	rec.State("x", StateRunning, now)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorder_StopsAtSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	const limit = 4096
	start := time.Now()
	rec, err := newRecorderAt(path, limit, testConfig(), start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	// Write approximately 1 MB. Flush after each block, as the flush loop does.
	for i := 0; i < 10000; i++ {
		rec.Ping("test", PingResult{Reflector: "1.1.1.1", RTT: time.Millisecond, Timestamp: start})
		if i%100 == 0 {
			rec.mu.Lock()
			rec.flushLocked()
			rec.mu.Unlock()
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The recorder stops at the first flush after the limit. One block of
	// 100 events is less than 64 KiB.
	if info.Size() > limit+64*1024 {
		t.Errorf("file size %d is much larger than the limit %d", info.Size(), limit)
	}
}

// recDecision is a decision of Core in a recording or a replay.
type recDecision struct {
	t     int64
	what  string
	value string
}

// replayActuator collects the shaper decisions of a replay.
type replayActuator struct {
	t         *int64
	decisions *[]recDecision
}

func (a replayActuator) SetRate(dir Direction, rateKbps int) {
	*a.decisions = append(*a.decisions, recDecision{*a.t, "shaper " + string(dir), fmt.Sprint(rateKbps)})
}
func (replayActuator) StartPingers()     {}
func (replayActuator) StopPingers()      {}
func (replayActuator) InvalidateShaper() {}

// replayRecording sends the inputs of a recording to a new Core. It returns
// the decisions in the recording and the decisions of the replay.
func replayRecording(t *testing.T, path string) (recorded, replayed []recDecision) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	if !sc.Scan() {
		t.Fatal("empty recording")
	}
	var header recHeaderEvent
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339Nano, header.Start)
	if err != nil {
		t.Fatal(err)
	}
	cfg := header.Config

	var now int64
	cores := make(map[string]*Core)
	for i := range cfg.Links {
		cores[cfg.Links[i].Name] = NewCore(&cfg.Links[i], cfg, defaultMTU, defaultMTU,
			replayActuator{&now, &replayed}, testLogger(t))
	}

	for sc.Scan() {
		var ev struct {
			T         int64     `json:"t"`
			Type      string    `json:"type"`
			Link      string    `json:"link"`
			Reflector string    `json:"reflector"`
			RTTUs     int64     `json:"rtt_us"`
			Timeout   bool      `json:"timeout"`
			DlOWDUs   *int64    `json:"dl_owd_us"`
			UlOWDUs   *int64    `json:"ul_owd_us"`
			DlKbps    float64   `json:"dl_kbps"`
			UlKbps    float64   `json:"ul_kbps"`
			Dir       Direction `json:"dir"`
			RateKbps  int       `json:"rate_kbps"`
			State     string    `json:"state"`
			Paused    bool      `json:"paused"`
			On        bool      `json:"on"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		core := cores[ev.Link]
		if core == nil {
			t.Fatalf("event for unknown link %q", ev.Link)
		}
		ts := start.Add(time.Duration(ev.T) * time.Microsecond)
		now = ev.T
		before := core.State()
		switch ev.Type {
		case recStart:
			core.Start(ts)
		case recPing:
			p := PingResult{
				Reflector: ev.Reflector,
				RTT:       time.Duration(ev.RTTUs) * time.Microsecond,
				Timestamp: ts,
				Timeout:   ev.Timeout,
			}
			if ev.DlOWDUs != nil && ev.UlOWDUs != nil {
				p.OWD = true
				p.DlOWD = time.Duration(*ev.DlOWDUs) * time.Microsecond
				p.UlOWD = time.Duration(*ev.UlOWDUs) * time.Microsecond
			}
			core.HandlePing(p)
		case recRate:
			core.HandleRate(RateStats{DlRateKbps: ev.DlKbps, UlRateKbps: ev.UlKbps, Timestamp: ts})
		case recReflectorReset:
			core.ResetReflector(ev.Reflector)
		case recPause:
			core.SetPaused(ev.Dir, ev.Paused)
		case recCalibrating:
			core.SetCalibrating(ev.On, ts)
		case recFixedRate:
			core.SetFixedRate(ev.Dir, ev.RateKbps)
		case recUnshaped:
			core.SetUnshaped(ev.On, ts)
		case recShaper:
			recorded = append(recorded, recDecision{ev.T, "shaper " + string(ev.Dir), fmt.Sprint(ev.RateKbps)})
		case recState:
			recorded = append(recorded, recDecision{ev.T, "state", ev.State})
		default:
			t.Fatalf("unknown event type %q", ev.Type)
		}
		if after := core.State(); after != before {
			replayed = append(replayed, recDecision{ev.T, "state", after.String()})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recorded, replayed
}

// A replay of a recording must give the same decisions as the recorded run.
// If this test fails, the recording does not contain all inputs of Core.
func TestRecording_ReplayGivesSameDecisions(t *testing.T) {
	for _, run := range []struct {
		name   string
		policy simPolicy
	}{
		{"5g-variable", policyAutorate},
		{"starlink-like", policyAutorate},
		{"capacity-drop", policyAutorate},
		{"5g-variable", policyAutorateOWD},
		{"bidir-saturated", policyAutorateOWD},
	} {
		t.Run(run.name+"/"+string(run.policy), func(t *testing.T) {
			sc := scenarioByName(t, run.name)
			sc.duration = 60 * time.Second
			sc.recordPath = filepath.Join(t.TempDir(), "rec.jsonl")
			runSim(sc, run.policy)

			recorded, replayed := replayRecording(t, sc.recordPath)
			if len(recorded) == 0 {
				t.Fatal("the recording has no decisions")
			}
			if len(recorded) != len(replayed) {
				t.Fatalf("recorded %d decisions, replay gave %d", len(recorded), len(replayed))
			}
			for i := range recorded {
				if recorded[i] != replayed[i] {
					t.Fatalf("decision %d: recorded %+v, replay gave %+v", i, recorded[i], replayed[i])
				}
			}
			t.Logf("%d decisions are the same", len(recorded))
		})
	}
}

// A replay with stall and resume tests the state events and the start event.
func TestRecording_ReplayStallAndResume(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	cfg.StallDetectionThr = 2
	cfg.GlobalPingResponseTimeoutS = 1
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	logger := testLogger(t)
	rec, err := newRecorderAt(path, 0, cfg, start, logger)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, logger)

	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	rc.Start(at(0))
	rc.HandlePing(PingResult{Reflector: "1.1.1.1", RTT: 20 * time.Millisecond, Timestamp: at(100)})
	rc.ResetReflector("1.1.1.1", at(200))
	rc.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: at(1500), Timeout: true})
	rc.HandlePing(PingResult{Reflector: "1.1.1.1", Timestamp: at(1600), Timeout: true})
	rc.HandleRate(RateStats{DlRateKbps: 500, Timestamp: at(2000)})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	recorded, replayed := replayRecording(t, path)
	var states []string
	for _, d := range recorded {
		if d.what == "state" {
			states = append(states, d.value)
		}
	}
	if fmt.Sprint(states) != "[STALL RUNNING]" {
		t.Errorf("recorded states %v, want [STALL RUNNING]", states)
	}
	if fmt.Sprint(recorded) != fmt.Sprint(replayed) {
		t.Errorf("recorded %v\nreplay gave %v", recorded, replayed)
	}
}

func TestRecordingCore_RecordsPause(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Now()
	rec, err := newRecorderAt(path, 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, testLogger(t))
	rc.Start(start)
	rc.SetPaused(DL, true, start.Add(time.Second))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"pause"`) || !strings.Contains(string(data), `"paused":true`) {
		t.Errorf("recording has no pause event:\n%s", data)
	}
}

// A pause, a resume and a repeated resume (the retry after a failed rate
// change) give the same shaper decisions in a replay.
func TestRecording_ReplayPauseAndRetry(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Now()
	rec, err := newRecorderAt(path, 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, testLogger(t))
	rc.Start(start)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	rc.HandleRate(RateStats{Timestamp: at(1), DlRateKbps: 50000, UlRateKbps: 20000})
	rc.SetPaused(DL, true, at(2))
	rc.SetPaused(DL, false, at(3))
	rc.SetPaused(DL, false, at(4)) // retry
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, replayed := replayRecording(t, path)
	var shaper []recDecision
	for _, d := range recorded {
		if strings.HasPrefix(d.what, "shaper") {
			shaper = append(shaper, d)
		}
	}
	if len(shaper) == 0 || !slices.Equal(shaper, replayed) {
		t.Errorf("recorded %v, replay gave %v", shaper, replayed)
	}
}

// The recorder of a calibration block gets the same events as the main
// recorder, from the start of the block until it is removed.
func TestRecordingCore_CalRecorder(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	dir := t.TempDir()
	start := time.Now()
	main, err := newRecorderAt(filepath.Join(dir, "main.jsonl"), 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, main, testLogger(t))
	rc.Start(start)
	rc.HandleRate(RateStats{DlRateKbps: 1, Timestamp: start.Add(time.Second)})
	cal, err := newRecorderAt(filepath.Join(dir, "cal.jsonl"), 0, cfg, start.Add(2*time.Second), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc.SetCalRecorder(cal, start.Add(2*time.Second))
	rc.HandleRate(RateStats{DlRateKbps: 2, Timestamp: start.Add(3 * time.Second)})
	rc.SetFixedRate(DL, unshapedKbps, start.Add(4*time.Second))
	rc.SetCalRecorder(nil, start.Add(5*time.Second))
	rc.HandleRate(RateStats{DlRateKbps: 3, Timestamp: start.Add(6 * time.Second)})
	for _, r := range []*Recorder{main, cal} {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	types := func(name string) string {
		var out []string
		for _, ev := range readRecording(t, filepath.Join(dir, name)) {
			s := fmt.Sprint(ev["type"])
			if s == "rate" {
				s += fmt.Sprint(ev["dl_kbps"])
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	}
	if got, want := types("cal.jsonl"), "header start rate2 fixed_rate shaper"; got != want {
		t.Errorf("calibration recording: got %q, want %q", got, want)
	}
	if got := types("main.jsonl"); !strings.Contains(got, "rate1 rate2 fixed_rate shaper rate3") {
		t.Errorf("main recording: %q", got)
	}
}

// A recording with a calibration gives the same decisions in a replay: the
// start of the calibration mode from IDLE (base rates, RUNNING) and an
// unshaped block with a fixed rate.
func TestRecording_ReplayCalibration(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	cfg.EnableSleepFunction = true
	cfg.SustainedIdleSleepThrS = 1
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Now()
	rec, err := newRecorderAt(path, 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, testLogger(t))
	rc.Start(start)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	// No load: the link goes to IDLE.
	for s := 1; s <= 3; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	if rc.Core().State() != StateIdle {
		t.Fatalf("state %s, want IDLE", rc.Core().State())
	}
	rc.SetCalibrating(true, at(4))
	rc.SetFixedRate(DL, unshapedKbps, at(5))
	rc.SetFixedRate(UL, unshapedKbps, at(5))
	for s := 6; s <= 8; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	rc.SetPaused(DL, false, at(9))
	rc.SetPaused(UL, false, at(9))
	rc.SetCalibrating(false, at(10))
	for s := 11; s <= 13; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, replayed := replayRecording(t, path)
	if len(recorded) == 0 || !slices.Equal(recorded, replayed) {
		t.Errorf("recorded:\n%v\nreplay:\n%v", recorded, replayed)
	}
	var fixed bool
	for _, d := range recorded {
		fixed = fixed || d.value == fmt.Sprint(unshapedKbps)
	}
	if !fixed {
		t.Errorf("no fixed rate in the recording: %v", recorded)
	}
}

// A recording with the unshaped mode gives the same decisions in a replay:
// the wake-up from IDLE, no IDLE with low traffic, the fixed rates and
// the shaping again.
func TestRecording_ReplayUnshaped(t *testing.T) {
	cfg := testConfig()
	cfg.Links[0].Download.Adjust = true
	cfg.Links[0].Upload.Adjust = true
	cfg.EnableSleepFunction = true
	cfg.SustainedIdleSleepThrS = 1
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	start := time.Now()
	rec, err := newRecorderAt(path, 0, cfg, start, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, &fakeActuator{}, rec, testLogger(t))
	rc.Start(start)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	for s := 1; s <= 3; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	if rc.Core().State() != StateIdle {
		t.Fatalf("state %s, want IDLE", rc.Core().State())
	}
	rc.SetUnshaped(true, at(4))
	rc.SetFixedRate(DL, unshapedKbps, at(4))
	rc.SetFixedRate(UL, unshapedKbps, at(4))
	for s := 5; s <= 8; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	if rc.Core().State() != StateRunning {
		t.Fatalf("unshaped: state %s, want RUNNING", rc.Core().State())
	}
	rc.SetPaused(DL, false, at(9))
	rc.SetPaused(UL, false, at(9))
	rc.SetUnshaped(false, at(9))
	for s := 10; s <= 13; s++ {
		rc.HandleRate(RateStats{Timestamp: at(s)})
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, replayed := replayRecording(t, path)
	if len(recorded) == 0 || !slices.Equal(recorded, replayed) {
		t.Errorf("recorded:\n%v\nreplay:\n%v", recorded, replayed)
	}
}
