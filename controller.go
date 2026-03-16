// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"math"
	"sync"
	"time"
)

// State represents the controller's operating state.
type State int

const (
	StateRunning State = iota
	StateIdle
	StateStall
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "RUNNING"
	case StateIdle:
		return "IDLE"
	case StateStall:
		return "STALL"
	default:
		return "UNKNOWN"
	}
}

const mtuBytes = 1500

// dirState holds per-direction runtime state.
type dirState struct {
	shaperRateKbps    float64
	loadPercent       float64
	delayWindow       []bool // true = delay exceeded threshold
	delayWindowIdx    int
	bbDetected        bool
	lastBBTime        time.Time
	lastDecayTime     time.Time
	avgDeltaUs        float64 // average delta across active reflectors
}

// Controller implements the main cake-autorate control loop.
type Controller struct {
	cfg           *Config
	shaper        *Shaper
	pingerMgr     *PingerManager
	monitor       *Monitor
	logger        *Logger

	state         State
	dl            dirState
	ul            dirState
	lastPingTime  time.Time
	idleSince     time.Time

	mu            sync.Mutex

	// Channels
	pingCh        chan PingResult
	rateCh        chan RateStats

	// Cancellation for sub-goroutines (pingers)
	pingerCancel  context.CancelFunc
}

// NewController creates a new Controller.
func NewController(cfg *Config, logger *Logger) *Controller {
	shaper := NewShaper(logger)
	pingerMgr := NewPingerManager(cfg, logger)
	monitor := NewMonitor(cfg.Download.Interface, cfg.Upload.Interface, cfg.MonitorIntervalMs, logger)

	c := &Controller{
		cfg:       cfg,
		shaper:    shaper,
		pingerMgr: pingerMgr,
		monitor:   monitor,
		logger:    logger,
		state:     StateRunning,
		pingCh:    make(chan PingResult, 100),
		rateCh:    make(chan RateStats, 10),
	}

	c.dl = dirState{
		shaperRateKbps: float64(cfg.Download.BaseRateKbps),
		delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
	}
	c.ul = dirState{
		shaperRateKbps: float64(cfg.Upload.BaseRateKbps),
		delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
	}

	return c
}

// Run starts the controller. Blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	if c.cfg.StartupWaitS > 0 {
		c.logger.Infof("waiting %.1fs before starting", c.cfg.StartupWaitS)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(c.cfg.StartupWaitS * float64(time.Second))):
		}
	}

	// Set initial shaper rates
	c.applyRate(DL)
	c.applyRate(UL)

	// Start monitor
	go c.monitor.Run(ctx, c.rateCh)

	// Start pingers
	c.startPingers(ctx)

	// Health check ticker
	healthTicker := time.NewTicker(time.Duration(c.cfg.ReflectorResponseDeadlineS * float64(time.Second)))
	defer healthTicker.Stop()

	c.logger.Infof("controller started in %s state", c.state)
	c.logger.Infof("dl: %s %d/%d/%d kbps, ul: %s %d/%d/%d kbps",
		c.cfg.Download.Interface, c.cfg.Download.MinRateKbps, c.cfg.Download.BaseRateKbps, c.cfg.Download.MaxRateKbps,
		c.cfg.Upload.Interface, c.cfg.Upload.MinRateKbps, c.cfg.Upload.BaseRateKbps, c.cfg.Upload.MaxRateKbps)

	for {
		select {
		case <-ctx.Done():
			c.stopPingers()
			c.logger.Infof("controller stopped")
			return ctx.Err()

		case stats := <-c.rateCh:
			c.handleRateStats(ctx, stats)

		case result := <-c.pingCh:
			c.handlePingResult(result)

		case <-healthTicker.C:
			c.pingerMgr.ReplaceUnhealthy()
		}
	}
}

func (c *Controller) startPingers(ctx context.Context) {
	pingerCtx, cancel := context.WithCancel(ctx)
	c.pingerCancel = cancel
	go c.pingerMgr.Run(pingerCtx, c.pingCh)
}

func (c *Controller) stopPingers() {
	if c.pingerCancel != nil {
		c.pingerCancel()
		c.pingerCancel = nil
	}
}

func (c *Controller) handleRateStats(ctx context.Context, stats RateStats) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update load percentages
	if c.dl.shaperRateKbps > 0 {
		c.dl.loadPercent = stats.DlRateKbps / c.dl.shaperRateKbps
	}
	if c.ul.shaperRateKbps > 0 {
		c.ul.loadPercent = stats.UlRateKbps / c.ul.shaperRateKbps
	}

	switch c.state {
	case StateRunning:
		// Check for transition to idle
		if c.cfg.EnableSleepFunction &&
			stats.DlRateKbps < float64(c.cfg.ConnectionActiveThrKbps) &&
			stats.UlRateKbps < float64(c.cfg.ConnectionActiveThrKbps) {
			if c.idleSince.IsZero() {
				c.idleSince = stats.Timestamp
			} else if stats.Timestamp.Sub(c.idleSince).Seconds() > c.cfg.SustainedIdleSleepThrS {
				c.transitionTo(StateIdle)
				c.stopPingers()
				if c.cfg.MinShaperRatesEnforcement {
					c.dl.shaperRateKbps = float64(c.cfg.Download.MinRateKbps)
					c.ul.shaperRateKbps = float64(c.cfg.Upload.MinRateKbps)
					c.applyRate(DL)
					c.applyRate(UL)
				}
			}
		} else {
			c.idleSince = time.Time{}
		}

	case StateIdle:
		// Check for transition back to running
		if stats.DlRateKbps >= float64(c.cfg.ConnectionActiveThrKbps) ||
			stats.UlRateKbps >= float64(c.cfg.ConnectionActiveThrKbps) {
			c.transitionTo(StateRunning)
			c.idleSince = time.Time{}
			c.dl.shaperRateKbps = float64(c.cfg.Download.BaseRateKbps)
			c.ul.shaperRateKbps = float64(c.cfg.Upload.BaseRateKbps)
			c.applyRate(DL)
			c.applyRate(UL)
			c.startPingers(ctx)
		}

	case StateStall:
		// Resume if we see significant traffic
		if stats.DlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) ||
			stats.UlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) {
			c.transitionTo(StateRunning)
			c.startPingers(ctx)
		}
	}
}

func (c *Controller) handlePingResult(result PingResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != StateRunning {
		return
	}

	c.lastPingTime = result.Timestamp

	if result.Timeout {
		c.handleTimeout(result)
		return
	}

	rttUs := float64(result.RTT.Microseconds())

	// Update baseline RTT
	c.pingerMgr.UpdateBaseline(result.Reflector, rttUs)

	state := c.pingerMgr.GetState(result.Reflector)
	if state == nil {
		return
	}

	state.mu.Lock()
	baselineUs := state.BaselineRTT
	state.mu.Unlock()

	// OWD delta (how much delay increased from baseline)
	// Using RTT/2 as OWD approximation (same as fping mode in original)
	deltaUs := (rttUs - baselineUs) / 2.0
	if deltaUs < 0 {
		deltaUs = 0
	}

	// Update delta EWMA only when load is below threshold
	// (high load naturally increases RTT, shouldn't affect EWMA)
	if c.dl.loadPercent < c.cfg.HighLoadThr && c.ul.loadPercent < c.cfg.HighLoadThr {
		c.pingerMgr.UpdateDeltaEWMA(result.Reflector, deltaUs)
	}

	// Process both directions (with RTT, both get the same delta)
	c.processDelay(DL, deltaUs)
	c.processDelay(UL, deltaUs)
}

func (c *Controller) handleTimeout(result PingResult) {
	// Check for stall condition
	if !c.lastPingTime.IsZero() &&
		time.Since(c.lastPingTime).Seconds() > c.cfg.GlobalPingResponseTimeoutS {
		c.transitionTo(StateStall)
		c.stopPingers()
		if c.cfg.MinShaperRatesEnforcement {
			c.dl.shaperRateKbps = float64(c.cfg.Download.MinRateKbps)
			c.ul.shaperRateKbps = float64(c.cfg.Upload.MinRateKbps)
			c.applyRate(DL)
			c.applyRate(UL)
		}
	}
}

func (c *Controller) processDelay(dir Direction, deltaUs float64) {
	ds := c.getDirState(dir)
	dc := c.cfg.DirConfig(dir)

	// Calculate compensated delay threshold
	compensationUs := WirePacketCompensationUs(mtuBytes, int(ds.shaperRateKbps))
	thresholdUs := dc.OWDDeltaDelayThrMs*1000.0 + compensationUs

	// Record in sliding window
	exceeded := deltaUs > thresholdUs
	ds.delayWindow[ds.delayWindowIdx] = exceeded
	ds.delayWindowIdx = (ds.delayWindowIdx + 1) % len(ds.delayWindow)

	// Count bufferbloat detections in window
	bbCount := 0
	for _, v := range ds.delayWindow {
		if v {
			bbCount++
		}
	}

	now := time.Now()
	bbDetected := bbCount >= c.cfg.BufferbloatDetectionThr

	if bbDetected {
		ds.bbDetected = true
		ds.lastBBTime = now
		c.adjustRateBufferbloat(dir, deltaUs, thresholdUs)
	} else if ds.loadPercent >= c.cfg.HighLoadThr {
		// High load, no bufferbloat — can increase rate
		refractoryDur := time.Duration(c.cfg.BufferbloatRefractoryPeriodMs) * time.Millisecond
		if !ds.bbDetected || now.Sub(ds.lastBBTime) > refractoryDur {
			ds.bbDetected = false
			c.adjustRateHighLoad(dir, deltaUs, thresholdUs)
		}
	} else {
		// Low load — decay toward base rate
		refractoryDur := time.Duration(c.cfg.DecayRefractoryPeriodMs) * time.Millisecond
		if now.Sub(ds.lastDecayTime) > refractoryDur {
			ds.lastDecayTime = now
			ds.bbDetected = false
			c.adjustRateDecay(dir)
		}
	}
}

// adjustRateBufferbloat reduces the shaper rate when bufferbloat is detected.
func (c *Controller) adjustRateBufferbloat(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.cfg.DirConfig(dir)

	// Calculate how far delta exceeds threshold relative to max threshold
	maxThrUs := dc.AvgOWDDeltaMaxAdjDownThrMs * 1000.0
	if maxThrUs <= thresholdUs {
		maxThrUs = thresholdUs + 1000 // prevent division by zero
	}

	factor := (deltaUs - thresholdUs) / (maxThrUs - thresholdUs)
	factor = clamp(factor, 0, 1)

	minAdj := c.cfg.ShaperRateMinAdjustDownBufferbloat
	maxAdj := c.cfg.ShaperRateMaxAdjustDownBufferbloat

	adjustment := minAdj - factor*(minAdj-maxAdj)
	newRate := ds.shaperRateKbps * adjustment

	newRate = clamp(newRate, float64(dc.MinRateKbps), float64(dc.MaxRateKbps))

	if newRate != ds.shaperRateKbps {
		c.logger.Debugf("[%s] bufferbloat: rate %.0f -> %.0f kbps (delta=%.0fus thr=%.0fus factor=%.2f)",
			dir, ds.shaperRateKbps, newRate, deltaUs, thresholdUs, factor)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

// adjustRateHighLoad increases the shaper rate during high load without bufferbloat.
func (c *Controller) adjustRateHighLoad(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.cfg.DirConfig(dir)

	// Factor based on how much headroom remains before hitting delay threshold
	maxUpThrUs := dc.AvgOWDDeltaMaxAdjUpThrMs * 1000.0
	if maxUpThrUs <= 0 {
		maxUpThrUs = 1
	}
	factor := 1.0 - (deltaUs / maxUpThrUs)
	factor = clamp(factor, 0, 1)

	minAdj := c.cfg.ShaperRateMinAdjustUpLoadHigh
	maxAdj := c.cfg.ShaperRateMaxAdjustUpLoadHigh

	adjustment := minAdj + factor*(maxAdj-minAdj)
	newRate := ds.shaperRateKbps * adjustment

	newRate = clamp(newRate, float64(dc.MinRateKbps), float64(dc.MaxRateKbps))

	if newRate != ds.shaperRateKbps {
		c.logger.Debugf("[%s] high load: rate %.0f -> %.0f kbps (factor=%.2f)",
			dir, ds.shaperRateKbps, newRate, factor)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

// adjustRateDecay moves the shaper rate toward the base rate during low load.
func (c *Controller) adjustRateDecay(dir Direction) {
	ds := c.getDirState(dir)
	dc := c.cfg.DirConfig(dir)

	baseRate := float64(dc.BaseRateKbps)
	var newRate float64

	if ds.shaperRateKbps > baseRate {
		newRate = ds.shaperRateKbps * c.cfg.ShaperRateAdjustDownLoadLow
		newRate = math.Max(newRate, baseRate)
	} else if ds.shaperRateKbps < baseRate {
		newRate = ds.shaperRateKbps * c.cfg.ShaperRateAdjustUpLoadLow
		newRate = math.Min(newRate, baseRate)
	} else {
		return
	}

	newRate = clamp(newRate, float64(dc.MinRateKbps), float64(dc.MaxRateKbps))

	if newRate != ds.shaperRateKbps {
		c.logger.Debugf("[%s] decay: rate %.0f -> %.0f kbps (base=%.0f)",
			dir, ds.shaperRateKbps, newRate, baseRate)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

func (c *Controller) applyRate(dir Direction) {
	dc := c.cfg.DirConfig(dir)
	ds := c.getDirState(dir)

	if !dc.Adjust {
		return
	}

	rateKbps := int(math.Round(ds.shaperRateKbps))
	if err := c.shaper.SetRate(dc.Interface, rateKbps); err != nil {
		c.logger.Errorf("[%s] setting rate to %d kbps: %v", dir, rateKbps, err)
	}
}

func (c *Controller) getDirState(dir Direction) *dirState {
	if dir == DL {
		return &c.dl
	}
	return &c.ul
}

func (c *Controller) transitionTo(newState State) {
	c.logger.Infof("state: %s -> %s", c.state, newState)
	c.state = newState
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
