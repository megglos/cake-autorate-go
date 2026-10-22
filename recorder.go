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
	"strings"
	"sync"
	"time"
)

// The recording is a JSONL file. Each line is one event. The first line is a
// header with the config. The recording contains all inputs of Core (ping
// results, throughput samples, start, reflector resets, pause and resume). Thus, a replay
// through Core gives the same decisions. The recording also contains the
// decisions (shaper rates, state changes), to compare a replay with the real
// session.
//
// The field "t" is the time of the event in microseconds after the start of
// the recording. It uses the monotonic clock. Thus, a change of the wall
// clock (NTP) does not change "t".
//
// If the file name ends with ".gz", the recorder writes gzip.

// recordFormatVersion increases when the event format changes.
const recordFormatVersion = 1

// recordFlushInterval is the time between two writes of the buffer to the
// file. If the program stops without a shutdown, the recording loses the
// events of this time or less.
const recordFlushInterval = time.Second

// Event types.
const (
	recHeader         = "header"
	recStart          = "start"
	recPing           = "ping"
	recRate           = "rate"
	recReflectorReset = "reflector_reset"
	recShaper         = "shaper"
	recState          = "state"
	recPause          = "pause"
	recCalibrating    = "calibrating"
	recFixedRate      = "fixed_rate"
	recUnshaped       = "unshaped"
	recMode           = "mode"
)

type recHeaderEvent struct {
	Type           string  `json:"type"`
	FormatVersion  int     `json:"format_version"`
	ProgramVersion string  `json:"program_version"`
	Start          string  `json:"start"` // wall clock, RFC 3339
	Config         *Config `json:"config"`
}

type recStartEvent struct {
	T    int64  `json:"t"`
	Type string `json:"type"`
	Link string `json:"link"`
}

type recPingEvent struct {
	T         int64  `json:"t"`
	Type      string `json:"type"`
	Link      string `json:"link"`
	Reflector string `json:"reflector"`
	RTTUs     int64  `json:"rtt_us"`
	Timeout   bool   `json:"timeout,omitempty"`
	// One-way delays (ICMP timestamps only). They contain the clock offset.
	DlOWDUs *int64 `json:"dl_owd_us,omitempty"`
	UlOWDUs *int64 `json:"ul_owd_us,omitempty"`
	// The time in the reflector (ICMP timestamps only). The RTT contains
	// it, the one-way delays do not.
	ReflectorUs int64 `json:"reflector_us,omitempty"`
}

type recRateEvent struct {
	T      int64   `json:"t"`
	Type   string  `json:"type"`
	Link   string  `json:"link"`
	DlKbps float64 `json:"dl_kbps"`
	UlKbps float64 `json:"ul_kbps"`
}

type recReflectorResetEvent struct {
	T         int64  `json:"t"`
	Type      string `json:"type"`
	Link      string `json:"link"`
	Reflector string `json:"reflector"`
}

type recShaperEvent struct {
	T        int64     `json:"t"`
	Type     string    `json:"type"`
	Link     string    `json:"link"`
	Dir      Direction `json:"dir"`
	RateKbps int       `json:"rate_kbps"`
}

type recPauseEvent struct {
	T      int64     `json:"t"`
	Type   string    `json:"type"`
	Link   string    `json:"link"`
	Dir    Direction `json:"dir"`
	Paused bool      `json:"paused"`
}

// recCalibratingEvent is the start or the end of the calibration mode.
type recCalibratingEvent struct {
	T    int64  `json:"t"`
	Type string `json:"type"`
	Link string `json:"link"`
	On   bool   `json:"on"`
}

// recUnshapedEvent is the start or the end of the unshaped mode.
type recUnshapedEvent struct {
	T    int64  `json:"t"`
	Type string `json:"type"`
	Link string `json:"link"`
	On   bool   `json:"on"`
}

// recModeEvent is a change of the link mode.
type recModeEvent struct {
	T    int64    `json:"t"`
	Type string   `json:"type"`
	Link string   `json:"link"`
	Mode LinkMode `json:"mode"`
}

// recFixedRateEvent is a fixed shaper rate of a calibration block.
type recFixedRateEvent struct {
	T        int64     `json:"t"`
	Type     string    `json:"type"`
	Link     string    `json:"link"`
	Dir      Direction `json:"dir"`
	RateKbps int       `json:"rate_kbps"`
}

type recStateEvent struct {
	T     int64  `json:"t"`
	Type  string `json:"type"`
	Link  string `json:"link"`
	State string `json:"state"`
}

// Recorder writes events to a recording file. More than one goroutine can
// use a Recorder at the same time. A nil *Recorder is valid and records
// nothing.
type Recorder struct {
	mu       sync.Mutex
	start    time.Time
	file     *os.File
	counter  *countingWriter
	gz       *gzip.Writer
	buf      *bufio.Writer
	maxBytes int64
	stopped  bool
	// stopReason is the reason of a stop before Close, or empty.
	stopReason string
	logger     *Logger
	done       chan struct{}
	closed     sync.WaitGroup
}

// countingWriter counts the bytes that go to the file.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// NewRecorder creates the recording file at path and writes the header.
// maxBytes is the maximum size of the file. When the file gets to this size,
// the recorder stops. Use 0 for no limit.
func NewRecorder(path string, maxBytes int64, cfg *Config, logger *Logger) (*Recorder, error) {
	return newRecorderAt(path, maxBytes, cfg, time.Now(), logger)
}

// newRecorderAt is NewRecorder with a given start time. The simulator uses
// it, because its clock does not start at time.Now().
func newRecorderAt(path string, maxBytes int64, cfg *Config, start time.Time, logger *Logger) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating recording file: %w", err)
	}
	r := &Recorder{
		start:    start,
		file:     f,
		counter:  &countingWriter{w: f},
		maxBytes: maxBytes,
		logger:   logger,
		done:     make(chan struct{}),
	}
	var w io.Writer = r.counter
	if strings.HasSuffix(path, ".gz") {
		r.gz = gzip.NewWriter(r.counter)
		w = r.gz
	}
	r.buf = bufio.NewWriterSize(w, 64*1024)

	r.write(recHeaderEvent{
		Type:           recHeader,
		FormatVersion:  recordFormatVersion,
		ProgramVersion: version,
		Start:          r.start.Format(time.RFC3339Nano),
		Config:         cfg,
	})

	r.closed.Add(1)
	go r.flushLoop()
	return r, nil
}

func (r *Recorder) flushLoop() {
	defer r.closed.Done()
	ticker := time.NewTicker(recordFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.mu.Lock()
			r.flushLocked()
			r.mu.Unlock()
		}
	}
}

// flushLocked writes the buffer to the file. The caller must hold r.mu.
func (r *Recorder) flushLocked() {
	if r.stopped {
		return
	}
	err := r.buf.Flush()
	if err == nil && r.gz != nil {
		err = r.gz.Flush()
	}
	if err != nil {
		r.stopLocked(fmt.Sprintf("write error: %v", err))
		return
	}
	if r.maxBytes > 0 && r.counter.n >= r.maxBytes {
		r.stopLocked(fmt.Sprintf("the file is at the size limit (%d bytes)", r.maxBytes))
	}
}

// stopLocked stops the recording. The program continues. The caller must
// hold r.mu.
func (r *Recorder) stopLocked(reason string) {
	r.stopped = true
	r.stopReason = reason
	r.logger.Errorf("recording stopped: %s", reason)
}

// EarlyStop returns an error if the recorder stopped before Close (a write
// error or the size limit): then the recording is not complete.
func (r *Recorder) EarlyStop() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopReason == "" {
		return nil
	}
	return fmt.Errorf("recording stopped: %s", r.stopReason)
}

// Close writes all buffered events and closes the file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	close(r.done)
	r.closed.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if !r.stopped {
		if err := r.buf.Flush(); err != nil {
			errs = append(errs, err)
		}
	}
	// Always close the gzip stream, so that the file is a valid gzip file.
	if r.gz != nil {
		if err := r.gz.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.file.Close(); err != nil {
		errs = append(errs, err)
	}
	r.stopped = true
	if len(errs) > 0 {
		return fmt.Errorf("closing recording: %v", errs)
	}
	return nil
}

func (r *Recorder) write(ev any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		r.stopLocked(fmt.Sprintf("encoding event: %v", err))
		return
	}
	data = append(data, '\n')
	if _, err := r.buf.Write(data); err != nil {
		r.stopLocked(fmt.Sprintf("write error: %v", err))
	}
}

// since returns the microseconds from the start of the recording to ts.
func (r *Recorder) since(ts time.Time) int64 {
	return ts.Sub(r.start).Microseconds()
}

// Start records the start of Core for a link.
func (r *Recorder) Start(link string, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recStartEvent{T: r.since(ts), Type: recStart, Link: link})
}

// Ping records a ping result.
func (r *Recorder) Ping(link string, p PingResult) {
	if r == nil {
		return
	}
	ev := recPingEvent{
		T:         r.since(p.Timestamp),
		Type:      recPing,
		Link:      link,
		Reflector: p.Reflector,
		RTTUs:     p.RTT.Microseconds(),
		Timeout:   p.Timeout,
	}
	if p.OWD {
		dl, ul := p.DlOWD.Microseconds(), p.UlOWD.Microseconds()
		ev.DlOWDUs, ev.UlOWDUs = &dl, &ul
		ev.ReflectorUs = p.ReflectorTime.Microseconds()
	}
	r.write(ev)
}

// Rate records a throughput sample.
func (r *Recorder) Rate(link string, s RateStats) {
	if r == nil {
		return
	}
	r.write(recRateEvent{
		T:      r.since(s.Timestamp),
		Type:   recRate,
		Link:   link,
		DlKbps: s.DlRateKbps,
		UlKbps: s.UlRateKbps,
	})
}

// ReflectorReset records that Core removed the delay data of a reflector.
func (r *Recorder) ReflectorReset(link, reflector string, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recReflectorResetEvent{T: r.since(ts), Type: recReflectorReset, Link: link, Reflector: reflector})
}

// Shaper records a shaper rate that Core set.
func (r *Recorder) Shaper(link string, dir Direction, rateKbps int, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recShaperEvent{T: r.since(ts), Type: recShaper, Link: link, Dir: dir, RateKbps: rateKbps})
}

// Pause records that a user paused or resumed the rate control of a
// direction. It is an input of Core.
func (r *Recorder) Pause(link string, dir Direction, paused bool, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recPauseEvent{T: r.since(ts), Type: recPause, Link: link, Dir: dir, Paused: paused})
}

// Calibrating records the start or the end of the calibration mode. It is
// an input of Core.
func (r *Recorder) Calibrating(link string, on bool, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recCalibratingEvent{T: r.since(ts), Type: recCalibrating, Link: link, On: on})
}

// Unshaped records the start or the end of the unshaped mode. It is an
// input of Core.
func (r *Recorder) Unshaped(link string, on bool, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recUnshapedEvent{T: r.since(ts), Type: recUnshaped, Link: link, On: on})
}

// Mode records a change of the link mode. It is an input of Core.
func (r *Recorder) Mode(link string, m LinkMode, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recModeEvent{T: r.since(ts), Type: recMode, Link: link, Mode: m})
}

// FixedRate records a fixed shaper rate of a calibration block. It is an
// input of Core.
func (r *Recorder) FixedRate(link string, dir Direction, rateKbps int, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recFixedRateEvent{T: r.since(ts), Type: recFixedRate, Link: link, Dir: dir, RateKbps: rateKbps})
}

// State records a state change of Core.
func (r *Recorder) State(link string, s State, ts time.Time) {
	if r == nil {
		return
	}
	r.write(recStateEvent{T: r.since(ts), Type: recState, Link: link, State: s.String()})
}

// RecordingCore sends events to a Core and records the events and the
// decisions of the Core. The controller and the simulator use it. Thus, a
// recording from the simulator has the same content as a recording from a
// real link.
type RecordingCore struct {
	name string
	core *Core
	act  Actuator
	rec  *Recorder
	// cal is the recorder of the current block of a calibration session,
	// or nil. It gets the same events as rec.
	cal *Recorder

	// eventTime is the timestamp of the event that Core processes now. The
	// recorder uses it for the decisions of Core.
	eventTime time.Time
}

// NewRecordingCore creates a Core that sends its decisions to act. rec can be
// nil.
func NewRecordingCore(link *LinkConfig, cfg *Config, dlMTU, ulMTU int, act Actuator, rec *Recorder, logger *Logger) *RecordingCore {
	rc := &RecordingCore{name: link.Name, act: act, rec: rec}
	rc.core = NewCore(link, cfg, dlMTU, ulMTU, rc, logger)
	return rc
}

// Core returns the Core.
func (rc *RecordingCore) Core() *Core { return rc.core }

// record writes an event to the recorders. The methods of Recorder accept a
// nil recorder.
func (rc *RecordingCore) record(f func(r *Recorder)) {
	f(rc.rec)
	f(rc.cal)
}

// SetCalRecorder sets the recorder of a calibration block (nil: no
// recorder). The recording starts with a start event at ts.
func (rc *RecordingCore) SetCalRecorder(rec *Recorder, ts time.Time) {
	rc.cal = rec
	rc.cal.Start(rc.name, ts)
}

// SetCalibrating starts or ends the calibration mode of the Core.
func (rc *RecordingCore) SetCalibrating(on bool, ts time.Time) {
	rc.record(func(r *Recorder) { r.Calibrating(rc.name, on, ts) })
	rc.process(ts, func() { rc.core.SetCalibrating(on, ts) })
}

// SetUnshaped starts or ends the unshaped mode of the Core.
func (rc *RecordingCore) SetUnshaped(on bool, ts time.Time) {
	rc.record(func(r *Recorder) { r.Unshaped(rc.name, on, ts) })
	rc.process(ts, func() { rc.core.SetUnshaped(on, ts) })
}

// SetMode records a change of the link mode and sends it to the Core.
func (rc *RecordingCore) SetMode(m LinkMode, ts time.Time) {
	rc.record(func(r *Recorder) { r.Mode(rc.name, m, ts) })
	rc.process(ts, func() { rc.core.SetMode(m) })
}

// SetFixedRate sets a fixed shaper rate and pauses the rate control of a
// direction (a calibration block without a shaper limit).
func (rc *RecordingCore) SetFixedRate(dir Direction, rateKbps int, ts time.Time) {
	rc.record(func(r *Recorder) { r.FixedRate(rc.name, dir, rateKbps, ts) })
	rc.process(ts, func() { rc.core.SetFixedRate(dir, rateKbps) })
}

// Start records the start and starts the Core.
func (rc *RecordingCore) Start(ts time.Time) {
	rc.record(func(r *Recorder) { r.Start(rc.name, ts) })
	rc.process(ts, func() { rc.core.Start(ts) })
}

// HandleRate records a throughput sample and sends it to the Core.
func (rc *RecordingCore) HandleRate(s RateStats) {
	rc.record(func(r *Recorder) { r.Rate(rc.name, s) })
	rc.process(s.Timestamp, func() { rc.core.HandleRate(s) })
}

// HandlePing records a ping result and sends it to the Core.
func (rc *RecordingCore) HandlePing(p PingResult) {
	rc.record(func(r *Recorder) { r.Ping(rc.name, p) })
	rc.process(p.Timestamp, func() { rc.core.HandlePing(p) })
}

// ResetReflector records a reflector reset and sends it to the Core.
func (rc *RecordingCore) ResetReflector(reflector string, ts time.Time) {
	rc.record(func(r *Recorder) { r.ReflectorReset(rc.name, reflector, ts) })
	rc.process(ts, func() { rc.core.ResetReflector(reflector) })
}

// SetPaused records a pause or a resume and sends it to the Core.
func (rc *RecordingCore) SetPaused(dir Direction, paused bool, ts time.Time) {
	rc.record(func(r *Recorder) { r.Pause(rc.name, dir, paused, ts) })
	rc.process(ts, func() { rc.core.SetPaused(dir, paused) })
}

// process lets the Core process an event with timestamp ts. It records a
// change of the state.
func (rc *RecordingCore) process(ts time.Time, f func()) {
	rc.eventTime = ts
	before := rc.core.State()
	f()
	if after := rc.core.State(); after != before {
		rc.record(func(r *Recorder) { r.State(rc.name, after, ts) })
	}
}

// SetRate implements Actuator.
func (rc *RecordingCore) SetRate(dir Direction, rateKbps int) {
	rc.record(func(r *Recorder) { r.Shaper(rc.name, dir, rateKbps, rc.eventTime) })
	rc.act.SetRate(dir, rateKbps)
}

// StartPingers implements Actuator.
func (rc *RecordingCore) StartPingers() { rc.act.StartPingers() }

// StopPingers implements Actuator.
func (rc *RecordingCore) StopPingers() { rc.act.StopPingers() }

// InvalidateShaper implements Actuator.
func (rc *RecordingCore) InvalidateShaper() { rc.act.InvalidateShaper() }
