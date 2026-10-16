// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LinkController runs the control loop for one WAN link. It owns the
// goroutines and the I/O (pingers, throughput monitor, shaper). It sends
// their events to a Core. The Core makes all control decisions.
type LinkController struct {
	name      string
	link      *LinkConfig
	cfg       *Config
	shaper    *Shaper
	pingerMgr *PingerManager
	monitor   *Monitor
	logger    *Logger
	core      *RecordingCore

	// board receives the status of the link. It can be nil.
	board *StatusBoard
	// rateErr is the last error of SetRate. Only the goroutine of Run uses it.
	rateErr error
	// calPaused are the pause states of the directions before a
	// calibration session. Only the goroutine of Run uses it.
	calPaused map[Direction]bool
	// unshapePaused are the pause states of the directions before the
	// command "unshape". Only the goroutine of Run uses it.
	unshapePaused map[Direction]bool

	// ctrlCh carries the commands (pause, resume, calibration) to the
	// goroutine of Run.
	ctrlCh chan *ctrlCmd

	// Channels
	pingCh chan PingResult
	rateCh chan RateStats

	// runCtx is the context of Run. The pingers start in this context.
	runCtx context.Context
	// Cancellation for sub-goroutines (pingers)
	pingerCancel context.CancelFunc
	// pingerDone is closed when all pingers of the running set have stopped.
	pingerDone chan struct{}
}

// NewLinkController creates a new LinkController for a single WAN link.
// recorder can be nil.
func NewLinkController(link *LinkConfig, cfg *Config, shaper *Shaper, recorder *Recorder, logger *Logger) *LinkController {
	c := &LinkController{
		name:      link.Name,
		link:      link,
		cfg:       cfg,
		shaper:    shaper,
		pingerMgr: NewPingerManager(link, cfg, logger),
		monitor:   NewMonitor(link.Download.Interface, link.Upload.Interface, cfg.MonitorIntervalMs, logger),
		logger:    logger,
		ctrlCh:    make(chan *ctrlCmd),
		pingCh:    make(chan PingResult, 100),
		rateCh:    make(chan RateStats, 10),
	}
	c.core = NewRecordingCore(link, cfg,
		readInterfaceMTU(link.Download.Interface, logger),
		readInterfaceMTU(link.Upload.Interface, logger),
		c, recorder, logger)
	return c
}

// Run starts the controller. Blocks until ctx is cancelled.
func (c *LinkController) Run(ctx context.Context) error {
	c.runCtx = ctx

	if c.cfg.StartupWaitS > 0 {
		c.logger.Infof("[%s] waiting %.1fs before starting", c.name, c.cfg.StartupWaitS)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(c.cfg.StartupWaitS * float64(time.Second))):
		}
	}

	// Set initial shaper rates
	c.core.Start(time.Now())

	// Start monitor
	go c.monitor.Run(ctx, c.rateCh)

	// Start pingers
	c.StartPingers()

	// Health check ticker
	healthTicker := time.NewTicker(time.Duration(c.cfg.ReflectorResponseDeadlineS * float64(time.Second)))
	defer healthTicker.Stop()

	// The status ticker has no channel when there is no status board.
	var statusC <-chan time.Time
	if c.board != nil {
		statusTicker := time.NewTicker(statusInterval)
		defer statusTicker.Stop()
		statusC = statusTicker.C
		c.publishStatus()
	}

	c.logger.Infof("[%s] controller started in %s state", c.name, c.core.Core().State())
	c.logger.Infof("[%s] dl: %s %d/%d/%d kbps, ul: %s %d/%d/%d kbps",
		c.name,
		c.link.Download.Interface, c.link.Download.MinRateKbps, c.link.Download.BaseRateKbps, c.link.Download.MaxRateKbps,
		c.link.Upload.Interface, c.link.Upload.MinRateKbps, c.link.Upload.BaseRateKbps, c.link.Upload.MaxRateKbps)

	for {
		select {
		case <-ctx.Done():
			c.stop(time.Now())
			return ctx.Err()

		case stats := <-c.rateCh:
			c.core.HandleRate(stats)

		case result := <-c.pingCh:
			c.core.HandlePing(result)

		case <-statusC:
			c.publishStatus()
		case cmd := <-c.ctrlCh:
			cmd.err = cmd.fn(time.Now())
			close(cmd.done)

		case <-healthTicker.C:
			if promoted := c.pingerMgr.ReplaceUnhealthy(); len(promoted) > 0 {
				now := time.Now()
				for _, r := range promoted {
					c.core.ResetReflector(r, now)
				}
				c.restartPingers()
			}
		}
	}
}

// publishStatus sends the status of the link to the status board. Only the
// goroutine of Run calls it, thus it can read the Core.
func (c *LinkController) publishStatus() {
	st := c.core.Core().Snapshot()
	baselines := c.core.Core().ReflectorBaselinesUs()
	for _, r := range c.pingerMgr.Snapshot() {
		st.Reflectors = append(st.Reflectors, ReflectorDelayStatus{ReflectorStatus: r, BaselineMs: roundMs(baselines[r.Address])})
	}
	c.board.Set(st)
}

// ctrlCmd is a function that the goroutine of Run calls.
type ctrlCmd struct {
	fn   func(now time.Time) error
	done chan struct{}
	// err is the result of fn, set before done closes.
	err error
}

// calDispatchTimeout is the time that a calibration command waits until
// the controller takes it. A rate change can take some seconds (netlink,
// then tc).
const calDispatchTimeout = 15 * time.Second

// callUntilDone runs fn on the goroutine of Run. It waits up to dispatch
// until the controller takes the command, and then until fn returns: the
// controller always runs a command that it took. The calibration uses it,
// so that the restore of the rate control cannot time out after the
// controller took a block command.
func (c *LinkController) callUntilDone(fn func(now time.Time) error, dispatch time.Duration) error {
	cmd := &ctrlCmd{fn: fn, done: make(chan struct{})}
	select {
	case c.ctrlCh <- cmd:
	case <-time.After(dispatch):
		return fmt.Errorf("link %q does not run", c.name)
	}
	<-cmd.done
	return cmd.err
}

// call runs fn on the goroutine of Run and returns its result. It returns
// an error if the controller does not run or does not answer in time.
func (c *LinkController) call(fn func(now time.Time) error) error {
	cmd := &ctrlCmd{fn: fn, done: make(chan struct{})}
	timeout := time.After(2 * time.Second)
	select {
	case c.ctrlCh <- cmd:
	case <-timeout:
		return fmt.Errorf("link %q does not run", c.name)
	}
	select {
	case <-cmd.done:
		return cmd.err
	case <-timeout:
		return fmt.Errorf("link %q does not answer", c.name)
	}
}

// SetPaused pauses or resumes the rate control of the directions. It returns
// after the controller has done the command.
func (c *LinkController) SetPaused(dirs []Direction, paused bool) error {
	return c.call(func(now time.Time) error {
		// A pause or a resume changes the shaper of a calibration block.
		if c.core.Core().Calibrating() {
			return fmt.Errorf("link %q: a calibration runs, stop it first", c.name)
		}
		if c.core.Core().Unshaped() {
			return fmt.Errorf("link %q: the shaping is off, turn it on first", c.name)
		}
		var err error
		for _, dir := range dirs {
			c.rateErr = nil
			c.core.SetPaused(dir, paused, now)
			if c.rateErr != nil {
				// The user can send the command again: it sets the base
				// rate again.
				err = c.rateErr
			}
		}
		return err
	})
}

// SetUnshaped turns the shaping of the link off (on is true) or on again.
// Without shaping, the shaper rate of each direction with rate control is
// unshapedKbps and the rate control stops; the pings and the status
// continue. When the shaping comes back, each direction gets its pause
// state from before: a paused direction stays paused at the base rate,
// the others start the rate control again from the base rate. The state
// is not kept: the program starts with shaping, and it shapes the link
// again before it stops.
func (c *LinkController) SetUnshaped(on bool) error {
	return c.call(func(now time.Time) error {
		if c.core.Core().Calibrating() {
			return fmt.Errorf("link %q: a calibration runs, stop it first", c.name)
		}
		if on == c.core.Core().Unshaped() {
			return nil
		}
		if !on {
			return c.shapeAgain(now)
		}
		if !c.link.Download.Adjust && !c.link.Upload.Adjust {
			return fmt.Errorf("link %q: no direction has rate control (adjust)", c.name)
		}
		core := c.core.Core()
		c.unshapePaused = map[Direction]bool{DL: core.Paused(DL), UL: core.Paused(UL)}
		// Set the mode before the rates: after a failed rate change, the
		// user can turn the shaping on again. The mode also wakes an idle
		// link: the delay measurement continues without load.
		c.rateErr = nil
		c.core.SetUnshaped(true, now)
		for _, dir := range Directions() {
			if c.link.DirConfig(dir).Adjust {
				c.core.SetFixedRate(dir, unshapedKbps, now)
			}
		}
		c.logger.Infof("[%s] shaping off", c.name)
		return c.rateErr
	})
}

// shapeAgain ends the unshaped state, if set: each direction gets its
// pause state from before. Only the goroutine of Run calls it.
func (c *LinkController) shapeAgain(now time.Time) error {
	if !c.core.Core().Unshaped() {
		return nil
	}
	c.rateErr = nil
	for _, dir := range Directions() {
		if c.link.DirConfig(dir).Adjust {
			c.core.SetPaused(dir, c.unshapePaused[dir], now)
		}
	}
	if c.rateErr != nil {
		return c.rateErr
	}
	c.core.SetUnshaped(false, now)
	c.logger.Infof("[%s] shaping on", c.name)
	return nil
}

// stop ends the work of the controller at the end of Run. The link must
// not stay without a shaper limit after the stop of the program: it shapes
// an unshaped link again.
func (c *LinkController) stop(now time.Time) {
	if err := c.shapeAgain(now); err != nil {
		c.logger.Errorf("[%s] turning the shaping on before the stop: %v", c.name, err)
	}
	c.StopPingers()
	c.logger.Infof("[%s] controller stopped", c.name)
}

// restartPingers starts the pingers again to use the new active set of
// reflectors. When the Core stopped the pingers (IDLE or STALL), it does not
// start them. The Core starts them with the new set when it goes back to
// RUNNING.
func (c *LinkController) restartPingers() {
	if c.pingerCancel == nil {
		return
	}
	c.StartPingers()
}

// StartPingers implements Actuator. It stops the pingers that run before it
// starts the new ones. Thus, only one set of pingers runs at a time.
func (c *LinkController) StartPingers() {
	c.StopPingers()
	pingerCtx, cancel := context.WithCancel(c.runCtx)
	done := make(chan struct{})
	c.pingerCancel, c.pingerDone = cancel, done
	go func() {
		defer close(done)
		c.pingerMgr.Run(pingerCtx, c.pingCh)
	}()
}

// StopPingers implements Actuator. It returns after all pingers have
// stopped. Thus, the pingers of an old set do not change the reflector
// states after a new set starts. The pingers do not block on a full result
// channel after the cancel, thus the wait is short.
func (c *LinkController) StopPingers() {
	if c.pingerCancel != nil {
		c.pingerCancel()
		c.pingerCancel = nil
	}
	if c.pingerDone != nil {
		<-c.pingerDone
		c.pingerDone = nil
	}
}

// SetRate implements Actuator.
func (c *LinkController) SetRate(dir Direction, rateKbps int) {
	if err := c.shaper.SetRate(c.link.DirConfig(dir).Interface, rateKbps); err != nil {
		c.logger.Errorf("[%s] [%s] setting rate to %d kbps: %v", c.name, dir, rateKbps, err)
		c.rateErr = fmt.Errorf("setting the rate of %s to %d kbps: %w", dir, rateKbps, err)
	}
}

// InvalidateShaper implements Actuator.
func (c *LinkController) InvalidateShaper() {
	c.shaper.InvalidateCache(c.link.Download.Interface)
	c.shaper.InvalidateCache(c.link.Upload.Interface)
}

// readInterfaceMTU reads the MTU for a network interface from sysfs.
// Falls back to defaultMTU (1500) if the read fails.
func readInterfaceMTU(iface string, logger *Logger) int {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/mtu", iface))
	if err != nil {
		logger.Infof("reading MTU for %s: %v, using default %d", iface, err, defaultMTU)
		return defaultMTU
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || val <= 0 {
		logger.Infof("parsing MTU for %s: %v, using default %d", iface, err, defaultMTU)
		return defaultMTU
	}
	return val
}

// CalConfig implements CalTarget: the config of the link with only this
// link.
func (c *LinkController) CalConfig() *Config {
	cfg := *c.cfg
	cfg.Links = []LinkConfig{*c.link}
	return &cfg
}

// CalStart implements CalTarget. Both directions need the rate control: the
// session changes the shaper rates.
func (c *LinkController) CalStart() error {
	return c.callUntilDone(func(now time.Time) error {
		if !c.link.Download.Adjust || !c.link.Upload.Adjust {
			return fmt.Errorf("link %q: the calibration needs the rate control (adjust) in both directions", c.name)
		}
		if c.core.Core().Unshaped() {
			return fmt.Errorf("link %q: the shaping is off, turn it on first", c.name)
		}
		core := c.core.Core()
		c.calPaused = map[Direction]bool{DL: core.Paused(DL), UL: core.Paused(UL)}
		c.core.SetCalibrating(true, now)
		return nil
	}, calDispatchTimeout)
}

// CalBlock implements CalTarget.
func (c *LinkController) CalBlock(rec *Recorder, unshaped bool) error {
	return c.callUntilDone(func(now time.Time) error {
		c.core.SetCalRecorder(rec, now)
		c.rateErr = nil
		// The pings must run in the idle time of the block, also after a
		// STALL in the block before.
		c.core.SetCalibrating(true, now)
		for _, dir := range Directions() {
			if unshaped {
				c.core.SetFixedRate(dir, unshapedKbps, now)
			} else {
				c.core.SetPaused(dir, false, now)
			}
		}
		return c.rateErr
	}, calDispatchTimeout)
}

// CalStopRecording implements CalTarget.
func (c *LinkController) CalStopRecording() error {
	return c.callUntilDone(func(now time.Time) error {
		c.core.SetCalRecorder(nil, now)
		return nil
	}, calDispatchTimeout)
}

// CalFinish implements CalTarget.
func (c *LinkController) CalFinish() error {
	return c.callUntilDone(func(now time.Time) error {
		c.core.SetCalRecorder(nil, now)
		c.core.SetCalibrating(false, now)
		c.rateErr = nil
		for _, dir := range Directions() {
			c.core.SetPaused(dir, c.calPaused[dir], now)
		}
		return c.rateErr
	}, calDispatchTimeout)
}
