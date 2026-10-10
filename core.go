// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"math"
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

const defaultMTU = 1500

// Actuator receives the side effects of control decisions. The production
// implementation drives the shaper and pingers; tests and the simulator
// record or model them instead.
type Actuator interface {
	SetRate(dir Direction, rateKbps int)
	StartPingers()
	StopPingers()
	// InvalidateShaper drops cached shaper state; the qdisc may have been
	// recreated while the link was stalled (link flap, hotplug).
	InvalidateShaper()
}

// dirState holds per-direction runtime state.
type dirState struct {
	shaperRateKbps   float64
	loadPercent      float64
	mtuBytes         int
	delayWindow      []bool // true = delay exceeded threshold
	delayWindowIdx   int
	delayWindowCount int // running count of true entries (avoids O(n) scan)
	bbDetected       bool
	lastBBTime       time.Time
	lastDecayTime    time.Time
}

// reflectorDelay holds per-reflector delay tracking.
type reflectorDelay struct {
	baselineUs float64
	deltaEWMA  float64
}

// Core is the cake-autorate control algorithm for a single WAN link.
//
// It is deterministic: it performs no I/O, starts no goroutines and never
// reads the wall clock. Time comes from the timestamps on incoming events, so
// the same event sequence always produces the same decisions. Callers must
// not invoke it concurrently.
type Core struct {
	name   string
	link   *LinkConfig
	cfg    *Config
	act    Actuator
	logger *Logger

	state               State
	dl                  dirState
	ul                  dirState
	reflectors          map[string]*reflectorDelay
	lastPingTime        time.Time
	idleSince           time.Time
	consecutiveTimeouts int
}

// NewCore creates a controller core. dlMTU and ulMTU are the interface MTUs
// used for wire packet compensation.
func NewCore(link *LinkConfig, cfg *Config, dlMTU, ulMTU int, act Actuator, logger *Logger) *Core {
	return &Core{
		name:       link.Name,
		link:       link,
		cfg:        cfg,
		act:        act,
		logger:     logger,
		state:      StateRunning,
		reflectors: make(map[string]*reflectorDelay),
		dl: dirState{
			shaperRateKbps: float64(link.Download.BaseRateKbps),
			mtuBytes:       dlMTU,
			delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
		},
		ul: dirState{
			shaperRateKbps: float64(link.Upload.BaseRateKbps),
			mtuBytes:       ulMTU,
			delayWindow:    make([]bool, cfg.BufferbloatDetectionWindow),
		},
	}
}

// Start applies the initial shaper rates. Connectivity is assumed until
// proven otherwise, so now counts as the last successful ping.
func (c *Core) Start(now time.Time) {
	c.lastPingTime = now
	c.applyRate(DL)
	c.applyRate(UL)
}

// State returns the current operating state.
func (c *Core) State() State { return c.state }

// ShaperRateKbps returns the current shaper rate for a direction.
func (c *Core) ShaperRateKbps(dir Direction) float64 {
	return c.getDirState(dir).shaperRateKbps
}

// ResetReflector discards delay tracking for a reflector, e.g. when it is
// promoted from the spare pool and its old baseline is stale.
func (c *Core) ResetReflector(reflector string) {
	delete(c.reflectors, reflector)
}

// HandleRate processes an achieved-throughput sample.
func (c *Core) HandleRate(stats RateStats) {
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
				c.act.StopPingers()
				if c.cfg.MinShaperRatesEnforcement {
					c.setRates(c.link.Download.MinRateKbps, c.link.Upload.MinRateKbps)
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
			c.lastPingTime = stats.Timestamp
			c.setRates(c.link.Download.BaseRateKbps, c.link.Upload.BaseRateKbps)
			c.act.StartPingers()
		}

	case StateStall:
		// Resume if we see significant traffic
		if stats.DlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) ||
			stats.UlRateKbps >= float64(c.cfg.ConnectionStallThrKbps) {
			c.transitionTo(StateRunning)
			c.consecutiveTimeouts = 0
			c.lastPingTime = stats.Timestamp
			c.act.InvalidateShaper()
			c.setRates(c.link.Download.BaseRateKbps, c.link.Upload.BaseRateKbps)
			c.act.StartPingers()
		}
	}
}

// HandlePing processes a ping reply or timeout.
func (c *Core) HandlePing(result PingResult) {
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
	rd := c.updateBaseline(result.Reflector, rttUs)

	// OWD delta (how much delay increased from baseline)
	// Using RTT/2 as OWD approximation (same as fping mode in original)
	deltaUs := (rttUs - rd.baselineUs) / 2.0
	if deltaUs < 0 {
		deltaUs = 0
	}

	// Update delta EWMA only when load is below threshold
	// (high load naturally increases RTT, shouldn't affect EWMA)
	if c.dl.loadPercent < c.cfg.HighLoadThr && c.ul.loadPercent < c.cfg.HighLoadThr {
		rd.deltaEWMA = c.cfg.AlphaDeltaEWMA*deltaUs + (1-c.cfg.AlphaDeltaEWMA)*rd.deltaEWMA
	}

	// Process both directions (with RTT, both get the same delta)
	c.processDelay(DL, deltaUs, result.Timestamp)
	c.processDelay(UL, deltaUs, result.Timestamp)
}

// updateBaseline updates the baseline RTT for a reflector using EWMA and
// returns its delay tracking state.
func (c *Core) updateBaseline(reflector string, rttUs float64) *reflectorDelay {
	rd, ok := c.reflectors[reflector]
	if !ok {
		// First measurement — initialize baseline
		rd = &reflectorDelay{baselineUs: rttUs}
		c.reflectors[reflector] = rd
		return rd
	}

	if rttUs < rd.baselineUs {
		// Delay decreased — track new minimum quickly
		rd.baselineUs = c.cfg.AlphaBaselineDecrease*rttUs +
			(1-c.cfg.AlphaBaselineDecrease)*rd.baselineUs
	} else {
		// Delay increased — baseline moves slowly
		rd.baselineUs = c.cfg.AlphaBaselineIncrease*rttUs +
			(1-c.cfg.AlphaBaselineIncrease)*rd.baselineUs
	}
	return rd
}

func (c *Core) handleTimeout(result PingResult) {
	c.consecutiveTimeouts++

	// Require both: enough consecutive timeouts AND elapsed time since last
	// successful ping exceeds the global timeout threshold.
	if c.consecutiveTimeouts >= c.cfg.StallDetectionThr &&
		!c.lastPingTime.IsZero() &&
		result.Timestamp.Sub(c.lastPingTime).Seconds() > c.cfg.GlobalPingResponseTimeoutS {
		c.transitionTo(StateStall)
		c.act.StopPingers()
		if c.cfg.MinShaperRatesEnforcement {
			c.setRates(c.link.Download.MinRateKbps, c.link.Upload.MinRateKbps)
		}
	}
}

func (c *Core) processDelay(dir Direction, deltaUs float64, now time.Time) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

	// Calculate compensated delay threshold
	compensationUs := WirePacketCompensationUs(ds.mtuBytes, int(ds.shaperRateKbps))
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
func (c *Core) adjustRateBufferbloat(dir Direction, deltaUs, thresholdUs float64) {
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
func (c *Core) adjustRateHighLoad(dir Direction, deltaUs, thresholdUs float64) {
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
func (c *Core) adjustRateDecay(dir Direction) {
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

// setRates sets and applies both shaper rates.
func (c *Core) setRates(dlKbps, ulKbps int) {
	c.dl.shaperRateKbps = float64(dlKbps)
	c.ul.shaperRateKbps = float64(ulKbps)
	c.applyRate(DL)
	c.applyRate(UL)
}

func (c *Core) applyRate(dir Direction) {
	if !c.link.DirConfig(dir).Adjust {
		return
	}
	c.act.SetRate(dir, int(math.Round(c.getDirState(dir).shaperRateKbps)))
}

func (c *Core) getDirState(dir Direction) *dirState {
	if dir == DL {
		return &c.dl
	}
	return &c.ul
}

func (c *Core) transitionTo(newState State) {
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
