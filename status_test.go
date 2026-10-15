package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nopActuator struct{}

func (nopActuator) SetRate(Direction, int) {}
func (nopActuator) StartPingers()          {}
func (nopActuator) StopPingers()           {}
func (nopActuator) InvalidateShaper()      {}

func TestCoreSnapshot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	link := &cfg.Links[0]
	core := NewCore(link, cfg, 1500, 1500, nopActuator{}, NewDiscardLogger())
	now := time.Now()
	core.Start(now)
	core.HandleRate(RateStats{Timestamp: now, DlRateKbps: 10000, UlRateKbps: 5000})
	core.HandlePing(PingResult{Reflector: "r1", RTT: 20 * time.Millisecond, Timestamp: now})
	core.HandlePing(PingResult{Reflector: "r1", RTT: 120 * time.Millisecond, Timestamp: now.Add(time.Second)})

	st := core.Snapshot()
	if st.Name != link.Name || st.State != "RUNNING" {
		t.Errorf("unexpected name or state: %+v", st)
	}
	if st.Download.ShaperKbps != float64(link.Download.BaseRateKbps) || st.Download.AchievedKbps != 10000 {
		t.Errorf("unexpected download rates: %+v", st.Download)
	}
	if st.Download.DeltaMs < 40 || st.Download.BufferbloatCount != 1 {
		t.Errorf("the second ping must give a high delta: %+v", st.Download)
	}
	if st.Download.LoadPercent != 50 {
		t.Errorf("load percent: %v", st.Download.LoadPercent)
	}
	if b := core.ReflectorBaselinesUs()["r1"]; b < 20000 || b > 21000 {
		t.Errorf("baseline: %v", b)
	}
}

func TestSnapshotShowsPause(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	core := NewCore(&cfg.Links[0], cfg, 1500, 1500, nopActuator{}, NewDiscardLogger())
	core.Start(time.Now())
	core.SetPaused(UL, true)
	st := core.Snapshot()
	if st.Download.Paused || !st.Upload.Paused {
		t.Errorf("only the upload must show a pause: %+v %+v", st.Download, st.Upload)
	}
	core.SetPaused(UL, false)
	if core.Snapshot().Upload.Paused {
		t.Error("the upload must not show a pause after a resume")
	}
}

func TestStatusBoardFile(t *testing.T) {
	now := time.Now()
	b := NewStatusBoard("v1", now, []string{"a", "b"})
	b.Set(LinkStatus{Name: "b", State: "IDLE"})
	f := b.File(now)
	if f.Schema != statusSchema || f.Version != "v1" || len(f.Links) != 1 || f.Links[0].Name != "b" {
		t.Errorf("unexpected file: %+v", f)
	}
	b.Set(LinkStatus{Name: "a", State: "RUNNING"})
	if f := b.File(now); len(f.Links) != 2 || f.Links[0].Name != "a" {
		t.Errorf("links must keep the config order: %+v", f.Links)
	}
}

func TestRunStatusWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "status.json")
	b := NewStatusBoard("v1", time.Now(), []string{"a"})
	b.Set(LinkStatus{Name: "a", State: "RUNNING"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunStatusWriter(ctx, b, path, NewDiscardLogger()); close(done) }()
	// Stop the writer on each exit path, also when an assertion fails.
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)

	var data []byte
	for range 40 {
		var err error
		if data, err = os.ReadFile(path); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var f StatusFile
	if err := json.Unmarshal(data, &f); err != nil || len(f.Links) != 1 {
		t.Fatalf("status file not valid: %v %q", err, data)
	}
	// Stop the writer first: a write in progress has a temporary file.
	stop()
	if left, _ := filepath.Glob(path + ".*.tmp"); len(left) != 0 {
		t.Errorf("temporary files must not stay: %v", left)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("status file must be removed at stop")
	}
}

func TestRateChangesCountOnlyChanges(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	core := NewCore(&cfg.Links[0], cfg, 1500, 1500, nopActuator{}, NewDiscardLogger())
	core.Start(time.Now())
	core.applyRate(DL)
	core.applyRate(DL)
	if n := core.Snapshot().Download.RateChanges; n != 1 {
		t.Errorf("rate changes: %d, want 1", n)
	}
	core.dl.shaperRateKbps--
	core.applyRate(DL)
	if n := core.Snapshot().Download.RateChanges; n != 2 {
		t.Errorf("rate changes: %d, want 2", n)
	}
}
