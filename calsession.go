// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Online calibration: the daemon runs a calibration session for one link.
// Another program (for example the LuCI page in a browser) makes the load:
// it reads the status of the session and makes the load that the field
// "load" tells. The session writes a directory in the format of
// scripts/record-session.sh and then runs the offline analysis
// (calibrateSession) on it. Thus, the same rules apply.
//
// Each block has an idle time (no load: the jitter of the reflectors), a
// load time and a short tail. In the unshaped blocks, the shaper of the
// link has no useful limit: the load fills the link, and the achieved rate
// is the capacity. In the autorate blocks, the normal rate control runs:
// the analysis gets the upload delay from download activity and the upload
// grant delay.

// calSessionBlocks are the blocks of a session, in their order.
var calSessionBlocks = []struct{ mode, policy string }{
	{"dl", "unshaped"},
	{"dl", "autorate"},
	{"ul", "unshaped"},
	{"ul", "autorate"},
	{"both", "unshaped"},
}

// unshapedKbps is the shaper rate in the unshaped blocks and of an
// unshaped link (command "unshape"): 10 Gbit/s,
// more than the capacity of a WAN link.
const unshapedKbps = 10_000_000

// calHeartbeat is the maximum time without a status request. The program
// that makes the load reads the status each second. Without the requests,
// there is no load: the session stops, so that the link does not stay
// without a shaper limit.
const calHeartbeat = 15 * time.Second

// The limits of the load time of a block.
const (
	calDefaultLoad = 40 * time.Second
	calMinLoad     = 20 * time.Second
	calMaxLoad     = 120 * time.Second
)

// calRecordingMaxBytes is the size limit of the recording of a block.
const calRecordingMaxBytes = 16 << 20

// calTimes are the times of each block.
type calTimes struct {
	Idle, Load, Tail time.Duration
}

// defaultCalTimes returns the times of a block with the given load time.
func defaultCalTimes(load time.Duration) calTimes {
	return calTimes{Idle: 15 * time.Second, Load: load, Tail: 3 * time.Second}
}

// CalTarget is the part of a link controller that a calibration session
// uses. Each method runs on the goroutine of the controller.
type CalTarget interface {
	// CalConfig returns the config for the recordings: the settings of
	// the link and only this link.
	CalConfig() *Config
	// CalStart starts the calibration mode: the controller does not go
	// to IDLE. It returns an error if the link cannot be calibrated.
	CalStart() error
	// CalBlock starts a block: the recorder of the block, and the shaper
	// without a limit (unshaped) or the normal rate control.
	CalBlock(rec *Recorder, unshaped bool) error
	// CalStopRecording ends the recording of the block.
	CalStopRecording() error
	// CalFinish ends the calibration mode and restores the rate control
	// as before the session (a paused direction stays paused).
	CalFinish() error
}

// CalDirProposal is the proposal for one direction. A value of 0 was not
// measured.
type CalDirProposal struct {
	MinRateKbps  int     `json:"min_rate_kbps,omitempty"`
	BaseRateKbps int     `json:"base_rate_kbps,omitempty"`
	MaxRateKbps  int     `json:"max_rate_kbps,omitempty"`
	ThresholdMs  float64 `json:"owd_delta_delay_thr_ms"`
}

// CalOutput is the result of a session.
type CalOutput struct {
	Link     string         `json:"link"`
	Finished time.Time      `json:"finished"`
	Download CalDirProposal `json:"download"`
	Upload   CalDirProposal `json:"upload"`
	// LearnCrossDelay, PingMethod and Reflectors are the proposed values.
	// Reflectors is empty if the session does not propose a list.
	LearnCrossDelay         bool     `json:"learn_cross_delay"`
	PingMethod              string   `json:"ping_method"`
	Reflectors              []string `json:"reflectors"`
	ConnectionActiveThrKbps int      `json:"connection_active_thr_kbps"`
	// Why has the measured data for the values, as text.
	Why   map[string]string `json:"why"`
	Notes []string          `json:"notes"`
	// Measured has the distributions of the measurements.
	Measured map[string]calDist `json:"measured"`
}

// newCalOutput makes the output from the result of the analysis.
func newCalOutput(link string, finished time.Time, res *calResult, cfg *Config, probe *calProbe) *CalOutput {
	p := res.propose(cfg)
	var notes []string
	probed := maps.Clone(res.Probed)
	if probed == nil {
		probed = map[string]bool{}
	}
	if probe != nil && probe.Err == nil && !probe.enough() {
		probe.Err = fmt.Errorf("fewer than %d requests to a reflector in %v", calMinIdleSamples, probe.Duration.Round(time.Second))
	}
	switch {
	case probe != nil && probe.Err == nil:
		// The probe tested ICMP timestamps on more reflectors than the
		// session pings: its result selects the ping method and the
		// reflectors.
		delete(p.Why, "ping_method")
		delete(p.Why, "reflectors")
		var why string
		p.PingMethod, p.Reflectors, why = proposeFromProbe(probe.Reflectors, cfg.PingerCount, probe.Duration)
		if p.PingMethod == PingMethodTimestamp {
			p.Why["reflectors"] = why
		} else {
			p.Why["ping_method"] = why
		}
		// The thresholds come from the jitter of the reflectors of the
		// proposal.
		var jitter []float64
		if p.PingMethod == PingMethodTimestamp {
			jitter = probeJitter(probe.Reflectors, p.Reflectors)
		}
		res.setThresholds(&p, jitter, fmt.Sprintf("ICMP timestamp probe, the %d reflectors in the list", len(p.Reflectors)))
		for _, r := range probe.Reflectors {
			if r.Sent > 0 {
				probed[r.Addr] = true
			}
		}
		if p.PingMethod == PingMethodTimestamp && cfg.PingMethod != PingMethodTimestamp {
			notes = append(notes, "the session used ICMP echo: after the change to icmp-timestamp, calibrate again to measure the upload delay from download activity (learn_cross_delay)")
		}
	case len(res.Reflectors) < calMinReflectors:
		// The session pings only the active reflectors (pinger_count).
		// With fewer than calMinReflectors and no probe, the analysis
		// cannot select the ping method and the reflectors: keep the
		// values of the config.
		if probe != nil {
			notes = append(notes, fmt.Sprintf("the ICMP timestamp probe did not run: %v", probe.Err))
		}
		p.PingMethod, p.Reflectors = cfg.PingMethod, nil
		res.setThresholds(&p, nil, "")
		delete(p.Why, "ping_method")
		delete(p.Why, "reflectors")
		notes = append(notes, fmt.Sprintf("the session pinged only %d reflectors (pinger_count %d): ping_method and reflectors keep the values of the config", len(res.Reflectors), cfg.PingerCount))
	case probe != nil:
		notes = append(notes, fmt.Sprintf("the ICMP timestamp probe did not run: %v", probe.Err))
	}
	// The session has no data of the configured reflectors that it did
	// not ping (spares). Keep them after the measured ones. A reflector
	// with only timeouts was pinged: it does not come back.
	if len(p.Reflectors) > 0 {
		var kept []string
		for _, r := range cfg.Links[0].Reflectors {
			if !probed[r] && !slices.Contains(p.Reflectors, r) {
				kept = append(kept, r)
			}
		}
		if len(kept) > 0 {
			p.Reflectors = append(slices.Clone(p.Reflectors), kept...)
			notes = append(notes, fmt.Sprintf("the session did not ping the reflectors %v: they stay in the list", kept))
		}
	}
	out := &CalOutput{
		Link:                    link,
		Finished:                finished,
		Download:                CalDirProposal{MinRateKbps: p.DlMin, BaseRateKbps: p.DlBase, MaxRateKbps: p.DlMax, ThresholdMs: p.DlThrMs},
		Upload:                  CalDirProposal{MinRateKbps: p.UlMin, BaseRateKbps: p.UlBase, MaxRateKbps: p.UlMax, ThresholdMs: p.UlThrMs},
		LearnCrossDelay:         p.LearnCrossDelay,
		PingMethod:              p.PingMethod,
		Reflectors:              p.Reflectors,
		ConnectionActiveThrKbps: activeThrKbps(cfg, p),
		Why:                     p.Why,
		Notes:                   append(slices.Clone(res.Notes), notes...),
		Measured: map[string]calDist{
			"dl_capacity_kbps":      res.DlCapacity,
			"ul_capacity_kbps":      res.UlCapacity,
			"ul_capacity_both_kbps": res.UlCapacityBoth,
			"idle_jitter_ms":        res.IdleJitterMs,
			"cross_delay_ms":        res.CrossDelayMs,
			"grant_delay_ms":        res.GrantDelayMs,
		},
	}
	if out.Reflectors == nil {
		out.Reflectors = []string{}
	}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	return out
}

// CalStatus is the answer of "calibrate status".
type CalStatus struct {
	// State is none (no session since the start), running, done, failed
	// or stopped.
	State string `json:"state"`
	// Session identifies the session: each start gives a new value. A
	// program that makes the load can check that it is the same session.
	Session string `json:"session,omitempty"`
	Link    string `json:"link,omitempty"`
	Block   int    `json:"block,omitempty"`
	Blocks  int    `json:"blocks,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Policy  string `json:"policy,omitempty"`
	// Phase is idle, load or tail.
	Phase string `json:"phase,omitempty"`
	// Load is the load that the other program must make now: dl, ul,
	// both, or empty (no load).
	Load string `json:"load"`
	// PhaseLeftS and LeftS are the times left in the phase and in the
	// session, in seconds.
	PhaseLeftS float64    `json:"phase_left_s"`
	LeftS      float64    `json:"left_s"`
	LoadS      float64    `json:"load_s,omitempty"`
	Error      string     `json:"error,omitempty"`
	Result     *CalOutput `json:"result,omitempty"`
}

// calPinMargin is the time of the pin of a session after its planned end.
// The end of the session removes the pin earlier.
const calPinMargin = time.Minute

// Calibrator runs one calibration session at a time.
type Calibrator struct {
	ctx     context.Context
	dir     string
	targets map[string]CalTarget
	logger  *Logger
	// now is the clock, tick the time between two checks in a phase, and
	// heartbeat the maximum time without a status request. Tests change
	// them.
	now       func() time.Time
	tick      time.Duration
	heartbeat time.Duration
	// onPhase, if set, gets the status at the start of each phase (for
	// the tests).
	onPhase func(CalStatus)
	// recMaxBytes is the size limit of the recording of a block.
	recMaxBytes int64
	// probe, if set, runs the ICMP timestamp probe in the idle time of the
	// first block.
	probe probeFunc
	// pin, if set, sends the traffic to the load hosts through the link of
	// the session (see Pinner), until the end of the session.
	pin PinControl

	mu       sync.Mutex
	status   CalStatus
	phaseEnd time.Time
	end      time.Time
	lastPoll time.Time
	cancel   context.CancelFunc
	done     chan struct{}
	// sessionCtx is the context of the last session.
	sessionCtx context.Context
	// sessions is the number of started sessions (for the session
	// identifier).
	sessions int
}

// NewCalibrator creates a calibrator. The sessions end when ctx ends. dir
// is the directory for the session files and the result files.
func NewCalibrator(ctx context.Context, dir string, targets map[string]CalTarget, logger *Logger) *Calibrator {
	return &Calibrator{ctx: ctx, dir: dir, targets: targets, logger: logger,
		now: time.Now, tick: 100 * time.Millisecond, heartbeat: calHeartbeat, recMaxBytes: calRecordingMaxBytes,
		status: CalStatus{State: "none"}}
}

// Start starts a session for a link with the given times.
func (k *Calibrator) Start(link string, times calTimes) error {
	target, ok := k.targets[link]
	if !ok {
		return fmt.Errorf("unknown link %q", link)
	}
	// The link name is a directory name of the session: "." and ".." are
	// valid link names, but not valid directory names.
	if link == "." || link == ".." {
		return fmt.Errorf("link %q: the calibration needs another link name", link)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// After the start of the shutdown, Wait does not wait for a new
	// session: the controllers would stop before its restore.
	if k.ctx.Err() != nil {
		return errors.New("the service stops")
	}
	if k.status.State == "running" {
		return fmt.Errorf("a calibration of link %q runs", k.status.Link)
	}
	if err := target.CalStart(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(k.ctx)
	now := k.now()
	block := times.Idle + times.Load + times.Tail
	k.sessions++
	k.status = CalStatus{State: "running", Session: fmt.Sprintf("%d-%d", now.Unix(), k.sessions), Link: link,
		Blocks: len(calSessionBlocks), LoadS: times.Load.Seconds()}
	k.end = now.Add(time.Duration(len(calSessionBlocks)) * block)
	k.lastPoll = now
	k.cancel = cancel
	k.sessionCtx = ctx
	done := make(chan struct{})
	k.done = done
	k.logger.Infof("[%s] calibration started (load %v for each block)", link, times.Load)
	pinTime := k.end.Sub(now) + calPinMargin
	go func() {
		// Release the context of the session at its end.
		defer cancel()
		k.run(ctx, link, target, times, pinTime, done)
	}()
	return nil
}

// Stop stops the running session. With a session identifier, it stops the
// session only if it is that session. It waits until the session has
// restored the rate control.
func (k *Calibrator) Stop(session string) error {
	k.mu.Lock()
	if k.status.State != "running" {
		k.mu.Unlock()
		return errors.New("no calibration runs")
	}
	// A program that shows an older session must not stop a newer one.
	if session != "" && session != k.status.Session {
		k.mu.Unlock()
		return fmt.Errorf("the session %q does not run", session)
	}
	cancel, done := k.cancel, k.done
	k.mu.Unlock()
	cancel()
	<-done
	return nil
}

// Status returns the status of the session. Each call is a heartbeat of
// the program that makes the load.
func (k *Calibrator) Status() CalStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	k.lastPoll = now
	st := k.status
	if st.State == "running" {
		st.PhaseLeftS = max(0, k.phaseEnd.Sub(now).Seconds())
		st.LeftS = max(0, k.end.Sub(now).Seconds())
	}
	return st
}

// Wait waits until a running session has ended and has restored the rate
// control. A nil calibrator has no session.
func (k *Calibrator) Wait() {
	if k == nil {
		return
	}
	k.mu.Lock()
	done := k.done
	k.mu.Unlock()
	if done != nil {
		<-done
	}
}

// errCalStopped is the error of a stopped session.
var errCalStopped = errors.New("stopped")

// run runs the session and sets the final state.
func (k *Calibrator) run(ctx context.Context, link string, target CalTarget, times calTimes, pinTime time.Duration, done chan struct{}) {
	defer close(done)
	// The load of the session goes through the link. Without it, the
	// session runs: on a router with one WAN link, the load uses the link.
	var pinErr error
	if k.pin != nil {
		if pinErr = k.pin.Pin(link, pinTime); pinErr != nil {
			k.logger.Errorf("[%s] calibration: %v", link, pinErr)
		} else {
			defer func() {
				if err := k.pin.Unpin(link); err != nil {
					k.logger.Errorf("[%s] calibration: %v", link, err)
				}
			}()
		}
	}
	// The session restores the rate control after the last block, before
	// the analysis. After an error, restore it here.
	restored := false
	var ferr error
	restore := func() error {
		if !restored {
			restored = true
			ferr = target.CalFinish()
		}
		return ferr
	}
	out, err := k.session(ctx, link, target, times, restore, pinErr)
	// A failed restore is always a failed session: the shaper can still
	// have no limit.
	if ferr := restore(); ferr != nil {
		if err != nil && !errors.Is(err, errCalStopped) && !errors.Is(err, ferr) {
			err = fmt.Errorf("restore the rate control: %v (after: %v)", ferr, err)
		} else {
			err = fmt.Errorf("restore the rate control: %w", ferr)
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.status.Phase, k.status.Load = "", ""
	switch {
	case errors.Is(err, errCalStopped):
		k.status.State = "stopped"
		k.logger.Infof("[%s] calibration stopped", link)
	case err != nil:
		k.status.State, k.status.Error = "failed", err.Error()
		k.logger.Errorf("[%s] calibration failed: %v", link, err)
	default:
		k.status.State, k.status.Result = "done", out
		k.logger.Infof("[%s] calibration done", link)
	}
}

// privateDir creates the directory path with mode 0700, or checks an
// existing one: it must be a directory (not a link) of this user, and no
// other user can write to it. Thus, no other user can place a link in it
// (the control socket can be in a shared directory, for example /tmp).
func privateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a directory of this user that other users cannot write to", path)
	}
	return nil
}

// session runs the blocks, restores the rate control and runs the
// analysis.
// pinErr is the error of the pin of the link: the result gets a note.
func (k *Calibrator) session(ctx context.Context, link string, target CalTarget, times calTimes, restore func() error, pinErr error) (*CalOutput, error) {
	// The sessions and the results have separate directories: a link name
	// can end with ".json". The top directory is private: then nobody can
	// place links in it.
	top := filepath.Join(k.dir, "calibration")
	for _, d := range []string{top, filepath.Join(top, "sessions"), filepath.Join(top, "results")} {
		if err := privateDir(d); err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(top, "sessions", link)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info := fmt.Sprintf("online calibration of link %s\nblock: idle %ds, load %ds, download warm-up in the mode both 0s\n",
		link, int(times.Idle.Seconds()), int(times.Load.Seconds()))
	if err := os.WriteFile(filepath.Join(dir, "session.txt"), []byte(info), 0o600); err != nil {
		return nil, err
	}
	var probe *calProbe
	lines := "block\tround\tmode\tpolicy\tstart\tload_start\tload_end\tend\tcake_dl_kbps\tcake_ul_kbps\trecording\n"

	for i, b := range calSessionBlocks {
		name := fmt.Sprintf("%02d-%s-%s.jsonl", i+1, b.mode, b.policy)
		rec, err := newRecorderAt(filepath.Join(dir, name), k.recMaxBytes, target.CalConfig(), k.now(), k.logger)
		if err != nil {
			return nil, err
		}
		k.setBlock(i+1, b.mode, b.policy)
		err = target.CalBlock(rec, b.policy == "unshaped")
		// The idle time starts after the setup of the block: a slow rate
		// change must not shorten it. The end of the session moves with
		// the setup time.
		start := k.now()
		k.mu.Lock()
		k.end = start.Add(time.Duration(len(calSessionBlocks)-i) * (times.Idle + times.Load + times.Tail))
		k.mu.Unlock()
		var loadStart, loadEnd time.Time
		if err == nil && i == 0 && k.probe != nil {
			probe, err = k.idleWithProbe(ctx, target.CalConfig(), start.Add(times.Idle), times.Idle)
		} else if err == nil {
			err = k.phase(ctx, "idle", "", start.Add(times.Idle))
		}
		if err == nil {
			loadStart = k.now()
			err = k.phase(ctx, "load", b.mode, loadStart.Add(times.Load))
			loadEnd = k.now()
		}
		if err == nil {
			err = k.phase(ctx, "tail", "", loadEnd.Add(times.Tail))
		}
		if serr := target.CalStopRecording(); serr != nil && err == nil {
			err = serr
		}
		if cerr := rec.Close(); cerr != nil && err == nil {
			err = cerr
		}
		// A recording that stopped early does not cover the block.
		if serr := rec.EarlyStop(); serr != nil && err == nil {
			err = fmt.Errorf("block %d: %w", i+1, serr)
		}
		if err != nil {
			return nil, err
		}
		lines += fmt.Sprintf("%d\t1\t%s\t%s\t%d\t%d\t%d\t%d\t-\t-\t%s\n", i+1, b.mode, b.policy,
			start.Unix(), loadStart.Unix(), loadEnd.Unix(), k.now().Unix(), name)
		if err := os.WriteFile(filepath.Join(dir, "blocks.tsv"), []byte(lines), 0o600); err != nil {
			return nil, err
		}
	}

	// Restore the rate control before the analysis: the last block has
	// no shaper limit.
	if err := restore(); err != nil {
		return nil, err
	}
	res, cfg, err := calibrateSession(dir)
	if err != nil {
		return nil, fmt.Errorf("analysis: %w", err)
	}
	out := newCalOutput(link, k.now(), res, cfg, probe)
	if pinErr != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("the load was not pinned to the link: %v; on a router with more than one WAN link, the load can use another link", pinErr))
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	// A stop during the analysis: no result.
	if ctx.Err() != nil {
		return nil, errCalStopped
	}
	if err := os.WriteFile(filepath.Join(top, "results", link+".json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	return out, nil
}

// idleWithProbe runs the idle phase until end and the probe at the same
// time. The probe stops at the end of the phase, or after the time dur
// (the phase uses the clock of the calibrator, the probe the real time).
func (k *Calibrator) idleWithProbe(ctx context.Context, cfg *Config, end time.Time, dur time.Duration) (*calProbe, error) {
	pctx, cancel := context.WithTimeout(ctx, dur)
	type result struct {
		probes []tsProbe
		err    error
	}
	ch := make(chan result, 1)
	started := time.Now()
	go func() {
		probes, err := k.probe(pctx, cfg, k.logger)
		ch <- result{probes, err}
	}()
	err := k.phase(ctx, "idle", "", end)
	cancel()
	r := <-ch
	return &calProbe{Reflectors: r.probes, Duration: time.Since(started), Err: r.err}, err
}

func (k *Calibrator) setBlock(num int, mode, policy string) {
	k.mu.Lock()
	k.status.Block, k.status.Mode, k.status.Policy = num, mode, policy
	k.mu.Unlock()
}

// phase sets the phase and waits until its end. It returns an error if
// the session stops, or if nobody asked for the status for calHeartbeat.
func (k *Calibrator) phase(ctx context.Context, name, load string, end time.Time) error {
	k.mu.Lock()
	k.status.Phase, k.status.Load = name, load
	k.phaseEnd = end
	st := k.status
	k.mu.Unlock()
	if k.onPhase != nil {
		k.onPhase(st)
	}
	tick := time.NewTicker(k.tick)
	defer tick.Stop()
	for {
		k.mu.Lock()
		now, lastPoll := k.now(), k.lastPoll
		k.mu.Unlock()
		if !now.Before(end) {
			return nil
		}
		if now.Sub(lastPoll) > k.heartbeat {
			return fmt.Errorf("no status request for %v: the program that makes the load stopped", k.heartbeat)
		}
		select {
		case <-ctx.Done():
			return errCalStopped
		case <-tick.C:
		}
	}
}
