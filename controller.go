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
	delayWindowCount  int    // running count of true entries (avoids O(n) scan)
	bbDetected        bool
	lastBBTime        time.Time
	lastDecayTime     time.Time
}

// LinkController implements the cake-autorate control loop for a single WAN link.
type LinkController struct {
	name          string
	link          *LinkConfig
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
	consecutiveTimeouts int

	mu            sync.Mutex

	// Channels
	pingCh        chan PingResult
	rateCh        chan RateStats

	// Cancellation for sub-goroutines (pingers)
	pingerCancel  context.CancelFunc
}

// NewLinkController creates a new LinkController for a single WAN link.
func NewLinkController(link *LinkConfig, cfg *Config, shaper *Shaper, logger *Logger) *LinkController {
	pingerMgr := NewPingerManager(link, cfg, logger)
	monitor := NewMonitor(link.Download.Interface, link.Upload.Interface, cfg.MonitorIntervalMs, logger)

	c := &LinkController{
		name:         link.Name,
		link:         link,
		cfg:          cfg,
		shaper:       shaper,
		pingerMgr:    pingerMgr,
		monitor:      monitor,
		logger:       logger,
		state:        StateRunning,
		lastPingTime: time.Now(), // assume connectivity until proven otherwise
		pingCh:       make(chan PingResult, 100),
		rateCh:       make(chan RateStats, 10),
	}

	c.dl = dirState{
		shaperRateKbps: float64(link.Download.BaseRateKbps),
		delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
	}
	c.ul = dirState{
		shaperRateKbps: float64(link.Upload.BaseRateKbps),
		delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
	}

	return c
}

// Run starts the controller. Blocks until ctx is cancelled.
func (c *LinkController) Run(ctx context.Context) error {
	if c.cfg.StartupWaitS > 0 {
		c.logger.Infof("[%s] waiting %.1fs before starting", c.name, c.cfg.StartupWaitS)
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

	c.logger.Infof("[%s] controller started in %s state", c.name, c.state)
	c.logger.Infof("[%s] dl: %s %d/%d/%d kbps, ul: %s %d/%d/%d kbps",
		c.name,
		c.link.Download.Interface, c.link.Download.MinRateKbps, c.link.Download.BaseRateKbps, c.link.Download.MaxRateKbps,
		c.link.Upload.Interface, c.link.Upload.MinRateKbps, c.link.Upload.BaseRateKbps, c.link.Upload.MaxRateKbps)

	for {
		select {
		case <-ctx.Done():
			c.stopPingers()
			c.logger.Infof("[%s] controller stopped", c.name)
			return ctx.Err()

		case stats := <-c.rateCh:
			c.handleRateStats(ctx, stats)

		case result := <-c.pingCh:
			c.handlePingResult(result)

		case <-healthTicker.C:
			if c.pingerMgr.ReplaceUnhealthy() {
				// Restart pingers to pick up the new active set
				c.stopPingers()
				c.startPingers(ctx)
			}
		}
	}
}

func (c *LinkController) startPingers(ctx context.Context) {
	pingerCtx, cancel := context.WithCancel(ctx)
	c.pingerCancel = cancel
	go c.pingerMgr.Run(pingerCtx, c.pingCh)
}

func (c *LinkController) stopPingers() {
	if c.pingerCancel != nil {
		c.pingerCancel()
		c.pingerCancel = nil
	}
}

func (c *LinkController) handleRateStats(ctx context.Context, stats RateStats) {
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
					c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps)
					c.ul.shaperRateKbps = float64(c.link.Upload.MinRateKbps)
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
			c.consecutiveTimeouts = 0
			c.lastPingTime = time.Now()
			c.dl.shaperRateKbps = float64(c.link.Download.BaseRateKbps)
			c.ul.shaperRateKbps = float64(c.link.Upload.BaseRateKbps)
			c.applyRate(DL)
			c.applyRate(UL)
			c.startPingers(ctx)
		}

	case StateStall:
		// Resume if we see significant traffic
		if stats.DlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) ||
			stats.UlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) {
			c.transitionTo(StateRunning)
			c.consecutiveTimeouts = 0
			c.lastPingTime = time.Now()
			// Invalidate shaper cache — the qdisc may have been
			// recreated during the stall (link flap, hotplug).
			c.shaper.InvalidateCache(c.link.Download.Interface)
			c.shaper.InvalidateCache(c.link.Upload.Interface)
			c.dl.shaperRateKbps = float64(c.link.Download.BaseRateKbps)
			c.ul.shaperRateKbps = float64(c.link.Upload.BaseRateKbps)
			c.applyRate(DL)
			c.applyRate(UL)
			c.startPingers(ctx)
		}
	}
}

func (c *LinkController) handlePingResult(result PingResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != StateRunning {
		return
	}

	if result.Timeout {
		c.handleTimeout(result)
		return
	}

	// Only update lastPingTime on successful replies — timeouts are emitted
	// by the sweeper and would mask stall detection if counted here.
	c.lastPingTime = result.Timestamp
	c.consecutiveTimeouts = 0

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

func (c *LinkController) handleTimeout(result PingResult) {
	c.consecutiveTimeouts++

	// Require both: enough consecutive timeouts AND elapsed time since last
	// successful ping exceeds the global timeout threshold.
	if c.consecutiveTimeouts >= c.cfg.StallDetectionThr &&
		!c.lastPingTime.IsZero() &&
		time.Since(c.lastPingTime).Seconds() > c.cfg.GlobalPingResponseTimeoutS {
		c.transitionTo(StateStall)
		c.stopPingers()
		if c.cfg.MinShaperRatesEnforcement {
			c.dl.shaperRateKbps = float64(c.link.Download.MinRateKbps)
			c.ul.shaperRateKbps = float64(c.link.Upload.MinRateKbps)
			c.applyRate(DL)
			c.applyRate(UL)
		}
	}
}

func (c *LinkController) processDelay(dir Direction, deltaUs float64) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

	// Calculate compensated delay threshold
	compensationUs := WirePacketCompensationUs(mtuBytes, int(ds.shaperRateKbps))
	thresholdUs := dc.OWDDeltaDelayThrMs*1000.0 + compensationUs

	// Record in sliding window with O(1) running count
	exceeded := deltaUs > thresholdUs
	old := ds.delayWindow[ds.delayWindowIdx]
	ds.delayWindow[ds.delayWindowIdx] = exceeded
	ds.delayWindowIdx = (ds.delayWindowIdx + 1) % len(ds.delayWindow)

	// Update running count: subtract evicted, add new
	if old && !exceeded {
		ds.delayWindowCount--
	} else if !old && exceeded {
		ds.delayWindowCount++
	}
	bbCount := ds.delayWindowCount

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
func (c *LinkController) adjustRateBufferbloat(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

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
		c.logger.Debugf("[%s] [%s] bufferbloat: rate %.0f -> %.0f kbps (delta=%.0fus thr=%.0fus factor=%.2f)",
			c.name, dir, ds.shaperRateKbps, newRate, deltaUs, thresholdUs, factor)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

// adjustRateHighLoad increases the shaper rate during high load without bufferbloat.
func (c *LinkController) adjustRateHighLoad(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

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
		c.logger.Debugf("[%s] [%s] high load: rate %.0f -> %.0f kbps (factor=%.2f)",
			c.name, dir, ds.shaperRateKbps, newRate, factor)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

// adjustRateDecay moves the shaper rate toward the base rate during low load.
func (c *LinkController) adjustRateDecay(dir Direction) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

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
		c.logger.Debugf("[%s] [%s] decay: rate %.0f -> %.0f kbps (base=%.0f)",
			c.name, dir, ds.shaperRateKbps, newRate, baseRate)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

func (c *LinkController) applyRate(dir Direction) {
	dc := c.link.DirConfig(dir)
	ds := c.getDirState(dir)

	if !dc.Adjust {
		return
	}

	rateKbps := int(math.Round(ds.shaperRateKbps))
	if err := c.shaper.SetRate(dc.Interface, rateKbps); err != nil {
		c.logger.Errorf("[%s] [%s] setting rate to %d kbps: %v", c.name, dir, rateKbps, err)
	}
}

func (c *LinkController) getDirState(dir Direction) *dirState {
	if dir == DL {
		return &c.dl
	}
	return &c.ul
}

func (c *LinkController) transitionTo(newState State) {
	c.logger.Infof("[%s] state: %s -> %s", c.name, c.state, newState)
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
