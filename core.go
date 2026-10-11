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

// State is the operating state of the controller.
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

// Actuator applies the decisions of Core. In production, it sets the shaper
// rate and starts or stops the pingers. Tests and the simulator record or
// model these decisions.
type Actuator interface {
	SetRate(dir Direction, rateKbps int)
	StartPingers()
	StopPingers()
	// InvalidateShaper removes the cached shaper state. The system can
	// create the qdisc again during a stall (link flap, hotplug).
	InvalidateShaper()
}

// dirState is the runtime state for one direction.
type dirState struct {
	shaperRateKbps   float64
	achievedKbps     float64 // last measured throughput
	loadPercent      float64
	mtuBytes         int
	delayWindow      []bool // true: the delay was more than the threshold
	delayWindowIdx   int
	delayWindowCount int // number of true entries (no O(n) scan)
	bbDetected       bool
	lastBBTime       time.Time
	lastDecayTime    time.Time
	crossDelayUs     float64   // learned delay from activity in the other direction
	lowLoadSince     time.Time // start of the low load for the cross delay, or zero
}

// reflectorDelay is the delay data for one reflector.
type reflectorDelay struct {
	baselineUs float64
	deltaEWMA  float64

	// Baselines of the one-way delays (ICMP timestamps). owdInit is false
	// until the first reply with one-way delays.
	owdInit      bool
	dlBaselineUs float64
	ulBaselineUs float64
}

// Core is the cake-autorate control algorithm for one WAN link.
//
// Core is deterministic. It does no I/O, starts no goroutines and does not
// read the wall clock. The time comes from the timestamps of the events.
// Thus, the same events always give the same decisions.
// Do not call Core from more than one goroutine at the same time.
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
	// lastPingOWD is true if the last ping reply had one-way delays.
	lastPingOWD bool
}

// NewCore creates a Core. dlMTU and ulMTU are the MTUs of the interfaces.
// Core uses them for the wire packet compensation.
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

// Start applies the initial shaper rates. Core starts with the assumption
// that the link operates. Thus, now is the time of the last good ping.
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

// ResetReflector removes the delay data for a reflector. Use it when a spare
// reflector becomes active, because its old baseline is not correct.
func (c *Core) ResetReflector(reflector string) {
	delete(c.reflectors, reflector)
}

// HandleRate processes a sample of the measured throughput.
func (c *Core) HandleRate(stats RateStats) {
	c.dl.achievedKbps = stats.DlRateKbps
	c.ul.achievedKbps = stats.UlRateKbps

	// Update load percentages
	if c.dl.shaperRateKbps > 0 {
		c.dl.loadPercent = stats.DlRateKbps / c.dl.shaperRateKbps
	}
	if c.ul.shaperRateKbps > 0 {
		c.ul.loadPercent = stats.UlRateKbps / c.ul.shaperRateKbps
	}
	if c.ul.achievedKbps >= crossDelayMaxLoad*c.ul.shaperRateKbps {
		c.ul.lowLoadSince = time.Time{}
	} else if c.ul.lowLoadSince.IsZero() {
		c.ul.lowLoadSince = stats.Timestamp
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
		// Go back to running when there is traffic
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

	// Update lastPingTime only for replies. The sweeper sends the timeouts.
	// If timeouts update lastPingTime, the stall detection does not operate.
	c.lastPingTime = result.Timestamp
	c.consecutiveTimeouts = 0

	rttUs := float64(result.RTT.Microseconds())

	// Update baseline RTT
	rd := c.updateBaseline(result.Reflector, rttUs)

	// The OWD delta is the delay increase above the baseline.
	// Without one-way delays, RTT/2 is an approximate OWD (the same as fping
	// mode in the original). Then a queue in one direction also increases
	// the delta of the other direction.
	deltaUs := math.Max((rttUs-rd.baselineUs)/2.0, 0)
	dlDeltaUs, ulDeltaUs := deltaUs, deltaUs
	c.lastPingOWD = result.OWD
	if result.OWD {
		dlDeltaUs, ulDeltaUs = c.owdDeltas(rd, result)
		if c.cfg.LearnCrossDelay {
			ulDeltaUs = c.removeCrossDelay(ulDeltaUs, result.Timestamp)
		}
	}

	// Update the delta EWMA only when the load is below the threshold.
	// High load increases the RTT, and this must not change the EWMA.
	if c.dl.loadPercent < c.cfg.HighLoadThr && c.ul.loadPercent < c.cfg.HighLoadThr {
		rd.deltaEWMA = c.cfg.AlphaDeltaEWMA*deltaUs + (1-c.cfg.AlphaDeltaEWMA)*rd.deltaEWMA
	}

	c.processDelay(DL, dlDeltaUs, result.Timestamp)
	c.processDelay(UL, ulDeltaUs, result.Timestamp)
}

// owdDeltas updates the one-way delay baselines of a reflector. It returns
// the download and the upload delta. Each baseline contains the clock
// offset between the hosts. Thus, the delta does not contain it.
func (c *Core) owdDeltas(rd *reflectorDelay, result PingResult) (dlDeltaUs, ulDeltaUs float64) {
	dlUs := float64(result.DlOWD.Microseconds())
	ulUs := float64(result.UlOWD.Microseconds())
	if !rd.owdInit {
		rd.owdInit = true
		rd.dlBaselineUs, rd.ulBaselineUs = dlUs, ulUs
		return 0, 0
	}
	rd.dlBaselineUs = c.ewmaBaseline(rd.dlBaselineUs, dlUs)
	rd.ulBaselineUs = c.ewmaBaseline(rd.ulBaselineUs, ulUs)
	return math.Max(dlUs-rd.dlBaselineUs, 0), math.Max(ulUs-rd.ulBaselineUs, 0)
}

const (
	// crossDelayAlpha is the EWMA alpha of the learned cross delay, for each
	// ping. With 6 reflectors at 300 ms, the time constant is approximately
	// 1 s.
	crossDelayAlpha = 0.05
	// crossDelayMaxLoad is the maximum upload load (achieved rate / shaper
	// rate) when Core learns the cross delay. At this load, the upload
	// shaper has no queue.
	crossDelayMaxLoad = 0.5
	// crossDelayMinLowLoad is the minimum time with low upload load before
	// Core learns the cross delay. After an upload capacity decrease, the
	// load is also low, but only until the first rate decrease (less than
	// 1 s). In that time, the queue of the upload shaper increases.
	crossDelayMinLowLoad = 2 * time.Second
)

// removeCrossDelay returns the upload delta without the delay that
// download activity causes. On 5G, the scheduler gives fewer upload grants
// when the download is busy. The upload shaper cannot remove this delay.
//
// Core learns this delay while the download is active and the upload load
// was low for crossDelayMinLowLoad or more. Then the delay cannot come from
// the upload shaper. The learned
// delay is not more than the upload delay threshold. Thus, a larger delay
// always shows bufferbloat. Core removes the learned delay only while the
// download is active.
//
// Only the upload uses it. After a capacity decrease, the download load
// can be low while its queue increases. Then the download would learn its
// own queue.
func (c *Core) removeCrossDelay(ulDeltaUs float64, now time.Time) float64 {
	if c.dl.achievedKbps < float64(c.cfg.ConnectionActiveThrKbps) {
		return ulDeltaUs
	}
	ul := &c.ul
	if !ul.lowLoadSince.IsZero() && now.Sub(ul.lowLoadSince) >= crossDelayMinLowLoad {
		ul.crossDelayUs += crossDelayAlpha * (ulDeltaUs - ul.crossDelayUs)
		ul.crossDelayUs = math.Min(ul.crossDelayUs, c.link.Upload.OWDDeltaDelayThrMs*1000)
	}
	return math.Max(ulDeltaUs-ul.crossDelayUs, 0)
}

// updateBaseline updates the baseline RTT of a reflector with an EWMA. It
// returns the delay data of the reflector.
func (c *Core) updateBaseline(reflector string, rttUs float64) *reflectorDelay {
	rd, ok := c.reflectors[reflector]
	if !ok {
		// First measurement: set the baseline
		rd = &reflectorDelay{baselineUs: rttUs}
		c.reflectors[reflector] = rd
		return rd
	}

	rd.baselineUs = c.ewmaBaseline(rd.baselineUs, rttUs)
	return rd
}

// ewmaBaseline returns the next value of a delay baseline after a sample.
func (c *Core) ewmaBaseline(baselineUs, sampleUs float64) float64 {
	if sampleUs < baselineUs {
		// Delay decreased: follow the new minimum quickly
		return c.cfg.AlphaBaselineDecrease*sampleUs + (1-c.cfg.AlphaBaselineDecrease)*baselineUs
	}
	// Delay increased: the baseline moves slowly
	return c.cfg.AlphaBaselineIncrease*sampleUs + (1-c.cfg.AlphaBaselineIncrease)*baselineUs
}

func (c *Core) handleTimeout(result PingResult) {
	c.consecutiveTimeouts++

	// Two conditions are necessary: enough consecutive timeouts, and more time
	// since the last good ping than the global timeout threshold.
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

	// Calculate the compensated delay threshold
	compensationUs := WirePacketCompensationUs(ds.mtuBytes, int(ds.shaperRateKbps))
	thresholdUs := dc.OWDDeltaDelayThrMs*1000.0 + compensationUs

	// Record the result in the sliding window. Keep the count in O(1).
	exceeded := deltaUs > thresholdUs
	old := ds.delayWindow[ds.delayWindowIdx]
	ds.delayWindow[ds.delayWindowIdx] = exceeded
	ds.delayWindowIdx = (ds.delayWindowIdx + 1) % len(ds.delayWindow)

	// Update the count: remove the old entry, add the new entry
	if old && !exceeded {
		ds.delayWindowCount--
	} else if !old && exceeded {
		ds.delayWindowCount++
	}
	bbCount := ds.delayWindowCount

	bbDetected := bbCount >= c.cfg.BufferbloatDetectionThr
	bbRefractoryDur := time.Duration(c.cfg.BufferbloatRefractoryPeriodMs) * time.Millisecond

	if bbDetected {
		// With RefractoryOnDecrease, decrease the rate only one time in
		// each refractory period. After a decrease, the detection window
		// keeps the old delayed pings for some time. Without the refractory
		// period, each new ping decreases the rate again before the
		// decrease can have an effect.
		if c.cfg.RefractoryOnDecrease && ds.bbDetected && now.Sub(ds.lastBBTime) <= bbRefractoryDur {
			return
		}
		ds.bbDetected = true
		ds.lastBBTime = now
		c.adjustRateBufferbloat(dir, deltaUs, thresholdUs)
	} else if ds.loadPercent >= c.cfg.HighLoadThr {
		// High load and no bufferbloat: increase the rate
		if !ds.bbDetected || now.Sub(ds.lastBBTime) > bbRefractoryDur {
			ds.bbDetected = false
			c.adjustRateHighLoad(dir, deltaUs, thresholdUs)
		}
	} else {
		// Low load: move the rate toward the base rate
		refractoryDur := time.Duration(c.cfg.DecayRefractoryPeriodMs) * time.Millisecond
		if now.Sub(ds.lastDecayTime) > refractoryDur {
			ds.lastDecayTime = now
			ds.bbDetected = false
			c.adjustRateDecay(dir)
		}
	}
}

// adjustRateBufferbloat decreases the shaper rate when Core finds bufferbloat.
func (c *Core) adjustRateBufferbloat(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

	// Calculate how much the delta is above the threshold, relative to the max threshold
	maxThrUs := dc.AvgOWDDeltaMaxAdjDownThrMs * 1000.0
	if maxThrUs <= thresholdUs {
		maxThrUs = thresholdUs + 1000 // prevent a division by zero
	}

	factor := (deltaUs - thresholdUs) / (maxThrUs - thresholdUs)
	factor = clamp(factor, 0, 1)

	minAdj := c.cfg.ShaperRateMinAdjustDownBufferbloat
	maxAdj := c.cfg.ShaperRateMaxAdjustDownBufferbloat

	adjustment := minAdj - factor*(minAdj-maxAdj)

	// Decrease from the achieved rate if it is lower than the shaper rate.
	// Then the queue in the bottleneck empties: the new rate is below the
	// rate that the link transmits now. A decrease from a shaper rate far
	// above the capacity has no effect, and the queue stays full.
	// If the achieved rate is below the minimum rate, it is not a good
	// measurement of the capacity (for example, the load stopped). Then use
	// the shaper rate.
	// Use the achieved rate only with one-way delays. With RTT/2, a queue
	// in the other direction also gives bufferbloat in this direction. Then
	// the achieved rate shows only the demand, and each decrease goes lower
	// until the rate is at the minimum.
	fromRate := ds.shaperRateKbps
	if c.lastPingOWD && ds.achievedKbps > float64(dc.MinRateKbps) && ds.achievedKbps < fromRate {
		fromRate = ds.achievedKbps
	}
	newRate := fromRate * adjustment

	newRate = clamp(newRate, float64(dc.MinRateKbps), float64(dc.MaxRateKbps))

	if newRate != ds.shaperRateKbps {
		c.logger.Debugf("[%s] [%s] bufferbloat: rate %.0f -> %.0f kbps (achieved=%.0f delta=%.0fus thr=%.0fus factor=%.2f)",
			c.name, dir, ds.shaperRateKbps, newRate, ds.achievedKbps, deltaUs, thresholdUs, factor)
		ds.shaperRateKbps = newRate
		c.applyRate(dir)
	}
}

// adjustRateHighLoad increases the shaper rate at high load without bufferbloat.
func (c *Core) adjustRateHighLoad(dir Direction, deltaUs, thresholdUs float64) {
	ds := c.getDirState(dir)
	dc := c.link.DirConfig(dir)

	// The factor shows the remaining headroom below the delay threshold
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

// adjustRateDecay moves the shaper rate toward the base rate at low load.
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

// setRates sets and applies the two shaper rates.
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
