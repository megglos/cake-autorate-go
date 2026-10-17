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
	shaperRateKbps    float64
	achievedKbps      float64 // last measured throughput
	loadPercent       float64
	mtuBytes          int
	delayWindow       []bool // true: the delay was more than the threshold
	delayWindowIdx    int
	delayWindowCount  int // number of true entries (no O(n) scan)
	bbDetected        bool
	lastBBDeltaUs     float64 // delta at the last decrease
	lastBBTime        time.Time
	lastDecayTime     time.Time
	crossDelayUs      float64   // learned delay from activity in the other direction
	lowLoadSince      time.Time // start of the low load for the cross delay, or zero
	lastDeltaUs       float64   // delay delta of the last ping reply
	lastHighDelay     bool      // the last delta was above the compensated threshold
	rateChanges       int       // number of rate changes since the start
	lastRequestedKbps int       // last rate that Core sent to the actuator
	paused            bool      // true: Core does not change the shaper rate
}

// reflectorDelay is the delay data for one reflector.
type reflectorDelay struct {
	baselineUs float64

	// Baselines of the one-way delays (ICMP timestamps). owdInit is false
	// until the first reply with one-way delays.
	owdInit      bool
	dlBaselineUs float64
	ulBaselineUs float64
	// owdTime is the time of the last reply with one-way delays.
	owdTime time.Time
	// dlLastUs and ulLastUs are the last deltas (before the limit to 0).
	dlLastUs float64
	ulLastUs float64
	// pathBaselineUs is the baseline of the RTT without the time in the
	// reflector. The one-way delays do not contain this time.
	pathBaselineUs float64
}

// maxClockSlewUsPerS is the maximum change of the clock offset that
// owdDeltas moves between the one-way delay baselines during a queue (µs
// for each second). It is the frequency limit of the Linux kernel. A path
// change is faster: most of it does not move between the baselines.
const maxClockSlewUsPerS = 500

// dayUs is one day (µs): ICMP timestamps are the time since midnight UTC.
const dayUs = 24 * 3600 * 1e6

// tsResolutionUs is the resolution of ICMP timestamps (µs).
const tsResolutionUs = 1000

// maxClockSlewGap is the maximum time between two replies for the slew
// limit. After a longer gap (Idle, Stall), a large allowance can move a
// path change and a queue between the baselines. The rule for a low load
// removes the rest of a slew after the gap.
const maxClockSlewGap = 2 * time.Second

// minClockStepUs is the minimum clock step that owdDeltas finds from one
// reply to the next. ntpd steps the clock for an offset of more than
// 128 ms (busybox ntpd of OpenWrt: more than 1 s), and slews it for a
// smaller offset. A smaller change can be a queue that moves from one
// direction to the other at the same RTT: the replies cannot show the
// difference.
const minClockStepUs = 128000

// maxOWDRTTDiffUs is the maximum difference between the sum of the two
// one-way delays and the RTT. A larger difference shows a clock step while
// the ping was in flight. The one-way delays have whole milliseconds, and
// the reflector can reply later than it received the request.
const maxOWDRTTDiffUs = 25000

// clockMaxLoad is the maximum load (achieved rate / shaper rate) of each
// direction for a clock step and for the move of the baselines with
// clockSplitGain. At this load, the shaper has no queue. The replies
// cannot show a queue that exists from the first reply on (also the RTT
// baseline has it): a low limit needs a shaper rate of 5 times the
// capacity for this case. NTP changes the clock at the boot, when the
// link is usually idle.
const clockMaxLoad = 0.2

// maxStepRTTChangeUs is the maximum change of the RTT from one reply to
// the next at a clock step (noise and whole milliseconds).
const maxStepRTTChangeUs = 5000

// maxClockQueueUs is the maximum difference of the RTT without the time in
// the reflector from its baseline for a clock step and for the move of the
// baselines with clockSplitGain. A larger difference shows a queue on the
// path: a low load does not exclude a queue, for example after a capacity
// decrease below the shaper rate.
const maxClockQueueUs = 10000

// clockSplitGain is the part of a delta that owdDeltas moves to the
// baseline for each reply while the RTT is near its baseline.
const clockSplitGain = 0.25

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
	// replies is the number of ping replies that Core used (in RUNNING),
	// for the status: a reader can find the samples with a new delay.
	replies int
	// calibrating is true during a calibration session. Then Core does not
	// go to IDLE: the session needs the pings also without load.
	calibrating bool
	// unshaped is true while a user turned the shaping off. Then Core does
	// not go to IDLE: the delay measurement continues.
	unshaped bool
	// rateSeen is true after the first throughput sample (HandleRate).
	// Before it, the load is not known.
	rateSeen bool
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

// SetPaused pauses or resumes the rate control of a direction. In both
// cases, the shaper goes to the base rate of the direction. This is a safe
// rate. While a direction is paused, Core still processes pings and
// throughput samples, but it does not change the shaper rate. After a
// resume, the normal control starts from the base rate.
//
// Each call sets the base rate, also when the direction is already in the
// requested state. Thus, the user can send the command again after a failed
// rate change, and Core keeps no state that the recording does not contain.
func (c *Core) SetPaused(dir Direction, paused bool) {
	ds := c.getDirState(dir)
	ds.paused = false
	ds.shaperRateKbps = float64(c.link.DirConfig(dir).BaseRateKbps)
	c.applyRate(dir)
	ds.paused = paused
}

// SetCalibrating starts or ends the calibration mode. In this mode, Core
// does not go to IDLE. On each call with on, it leaves IDLE and STALL at
// once (with the base rates and the pingers on), because a calibration
// block needs the pings also without load. A session calls it at the start
// of each block: many timeouts in a block without a shaper limit can give
// STALL.
func (c *Core) SetCalibrating(on bool, now time.Time) {
	c.calibrating = on
	c.idleSince = time.Time{}
	if on {
		c.wake(now)
	}
}

// SetUnshaped starts or ends the unshaped mode (command "unshape"). In
// this mode, Core does not go to IDLE, and the start of the mode leaves
// IDLE and STALL at once: the delay measurement continues without load.
// The shaper rates do not change here: the controller sets them.
func (c *Core) SetUnshaped(on bool, now time.Time) {
	c.unshaped = on
	c.idleSince = time.Time{}
	// The low upload load of the unshaped time is not a low load.
	c.ul.lowLoadSince = time.Time{}
	if on {
		c.wake(now)
	}
}

// Unshaped reports whether Core is in the unshaped mode.
func (c *Core) Unshaped() bool { return c.unshaped }

// wake leaves IDLE and STALL at once, with the base rates (of the
// directions that are not paused) and the pingers on.
func (c *Core) wake(now time.Time) {
	if c.state == StateRunning {
		return
	}
	if c.state == StateStall {
		c.act.InvalidateShaper()
	}
	c.transitionTo(StateRunning)
	c.consecutiveTimeouts = 0
	c.lastPingTime = now
	c.setRates(c.link.Download.BaseRateKbps, c.link.Upload.BaseRateKbps)
	c.act.StartPingers()
}

// Calibrating reports whether Core is in the calibration mode.
func (c *Core) Calibrating() bool { return c.calibrating }

// SetFixedRate sets the shaper rate of a direction and pauses its rate
// control. A calibration session uses it for the blocks without a shaper
// limit. SetPaused ends it (with the base rate).
func (c *Core) SetFixedRate(dir Direction, rateKbps int) {
	ds := c.getDirState(dir)
	ds.paused = false
	ds.shaperRateKbps = float64(rateKbps)
	c.applyRate(dir)
	ds.paused = true
}

// Paused reports whether the rate control of a direction is paused.
func (c *Core) Paused(dir Direction) bool { return c.getDirState(dir).paused }

// State returns the current operating state.
func (c *Core) State() State { return c.state }

// ShaperRateKbps returns the current shaper rate for a direction.
func (c *Core) ShaperRateKbps(dir Direction) float64 {
	return c.getDirState(dir).shaperRateKbps
}

// ReflectorBaselinesUs returns the baseline RTT of each reflector with a
// reply.
func (c *Core) ReflectorBaselinesUs() map[string]float64 {
	out := make(map[string]float64, len(c.reflectors))
	for r, rd := range c.reflectors {
		out[r] = rd.baselineUs
	}
	return out
}

// ResetReflector removes the delay data for a reflector. Use it when a spare
// reflector becomes active, because its old baseline is not correct.
func (c *Core) ResetReflector(reflector string) {
	delete(c.reflectors, reflector)
}

// HandleRate processes a sample of the measured throughput.
func (c *Core) HandleRate(stats RateStats) {
	c.rateSeen = true
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
		if c.cfg.EnableSleepFunction && !c.calibrating && !c.unshaped &&
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
	c.replies++

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

	c.processDelay(DL, dlDeltaUs, result.Timestamp)
	c.processDelay(UL, ulDeltaUs, result.Timestamp)
}

// owdDeltas updates the one-way delay baselines of a reflector. It returns
// the download and the upload delta. Each baseline contains the clock
// offset between the hosts. Thus, the delta does not contain it.
//
// A change of the clock offset (for example, NTP after the boot) moves the
// two one-way delays by the same value in opposite directions. The RTT
// does not change. owdDeltas removes a clock change from the deltas with
// three rules:
//
//   - The RTT is near its baseline: no queue in the two directions. Each
//     baseline moves by clockSplitGain of its delta (a fast slew of ntpd
//     after the boot).
//   - A delay is below its baseline, and the delay of the other direction
//     is above its baseline: a queue only increases a delay. The two
//     baselines move by the smaller difference, with a limit of
//     maxClockSlewUsPerS (a slow slew during a queue). A path change is
//     faster: most of it does not move.
//   - The two deltas change from one reply to the next by minClockStepUs
//     or more in opposite directions, and the RTT stays near its baseline
//     (a step). The two baselines move by the step at once. A queue stays
//     in its delta.
func (c *Core) owdDeltas(rd *reflectorDelay, result PingResult) (dlDeltaUs, ulDeltaUs float64) {
	dlUs := float64(result.DlOWD.Microseconds())
	ulUs := float64(result.UlOWD.Microseconds())
	// A clock step while the ping is in flight: the reply has the step only
	// in the download delay. The RTT (monotonic clock) does not have it.
	// Do not use this reply: the next reply shows the step in the two
	// delays. The RTT also contains the time in the reflector.
	// The times have a resolution of 1 ms and wrap at midnight: each
	// one-way delay can be a day too low or too high. Thus, the check uses
	// the difference modulo one day.
	// The time in the reflector is in 0 to RTT. A clock step of the
	// reflector between its receive and its transmit time gives another
	// value, and the sum does not show it.
	inReflectorUs := float64(result.ReflectorTime.Microseconds())
	rttUs := float64(result.RTT.Microseconds())
	diffUs := math.Mod(dlUs+ulUs+inReflectorUs-rttUs+dayUs/2, dayUs)
	if diffUs < 0 {
		diffUs += dayUs
	}
	diffUs -= dayUs / 2
	if math.Abs(diffUs) > maxOWDRTTDiffUs || inReflectorUs < -tsResolutionUs || inReflectorUs > rttUs+tsResolutionUs {
		return math.Max(rd.dlLastUs, 0), math.Max(rd.ulLastUs, 0)
	}
	pathUs := float64(result.RTT.Microseconds()) - inReflectorUs
	if !rd.owdInit {
		rd.owdInit = true
		rd.dlBaselineUs, rd.ulBaselineUs = dlUs, ulUs
		rd.pathBaselineUs = pathUs
		rd.owdTime = result.Timestamp
		return 0, 0
	}
	rd.pathBaselineUs = c.ewmaBaseline(rd.pathBaselineUs, pathUs)
	// shift > 0: the clock offset moves delay from the upload to the
	// download.
	shift := math.Min(math.Max(rd.ulBaselineUs-ulUs, 0), math.Max(dlUs-rd.dlBaselineUs, 0)) -
		math.Min(math.Max(rd.dlBaselineUs-dlUs, 0), math.Max(ulUs-rd.ulBaselineUs, 0))
	gap := min(max(result.Timestamp.Sub(rd.owdTime), 0), maxClockSlewGap)
	maxShift := maxClockSlewUsPerS * gap.Seconds()
	shift = math.Max(-maxShift, math.Min(shift, maxShift))
	// A clock step (NTP after the boot) changes the two deltas by the same
	// value in opposite directions, and the RTT does not change. A change
	// of a queue at the same time changes the RTT: then the part of each
	// direction is not known. Thus, a step moves only while the load is
	// low (no queue) and the RTT does not change. Else it stays in the
	// delta until the load is low: then the rule below removes it.
	// With the shaping off (unshape, the unshaped blocks of a calibration),
	// the load is against unshapedKbps: a low load does not show that there
	// is no queue. Before the first throughput sample, the load is not
	// known.
	lowLoad := c.rateSeen && c.dl.loadPercent < clockMaxLoad && c.ul.loadPercent < clockMaxLoad &&
		!c.unshaped && c.dl.shaperRateKbps < unshapedKbps && c.ul.shaperRateKbps < unshapedKbps
	// Also with a low load, the path can have a queue (for example after a
	// capacity decrease below the shaper rate). Then a queue that moves
	// from one direction to the other looks like a clock step. Thus, the
	// RTT without the time in the reflector must also be near its baseline.
	queueFree := math.Abs(pathUs-rd.pathBaselineUs) <= maxClockQueueUs
	dlChangeUs := dlUs - rd.dlBaselineUs - rd.dlLastUs
	ulChangeUs := ulUs - rd.ulBaselineUs - rd.ulLastUs
	if step := (dlChangeUs - ulChangeUs) / 2; lowLoad && queueFree && math.Abs(step) >= minClockStepUs &&
		math.Abs(dlChangeUs+ulChangeUs) <= maxStepRTTChangeUs {
		shift = step
	}
	rd.owdTime = result.Timestamp
	rd.dlBaselineUs += shift
	rd.ulBaselineUs -= shift
	// The load of the two directions is low: no queue can occur. If the RTT
	// without the time in the reflector is also near its baseline (the RTT
	// does not contain the clock offset), a delta is a clock change (for
	// example, a fast slew of ntpd after the boot). Move each baseline a part of the way to its
	// delay: the delays have noise. With a higher load, a path change and
	// a queue can look like a clock change.
	dlExcessUs, ulExcessUs := dlUs-rd.dlBaselineUs, ulUs-rd.ulBaselineUs
	if rttExcess := math.Abs(pathUs - rd.pathBaselineUs); lowLoad && queueFree &&
		rttExcess < math.Max(math.Abs(dlExcessUs), math.Abs(ulExcessUs))/2 {
		rd.dlBaselineUs += clockSplitGain * dlExcessUs
		rd.ulBaselineUs += clockSplitGain * ulExcessUs
	}
	rd.dlBaselineUs = c.ewmaBaseline(rd.dlBaselineUs, dlUs)
	rd.ulBaselineUs = c.ewmaBaseline(rd.ulBaselineUs, ulUs)
	rd.dlLastUs, rd.ulLastUs = dlUs-rd.dlBaselineUs, ulUs-rd.ulBaselineUs
	return math.Max(rd.dlLastUs, 0), math.Max(rd.ulLastUs, 0)
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
	// Without shaping, the load of the upload is against 10 Gbit/s: a
	// saturated upload has a low load, and its queue is not cross delay.
	if !c.unshaped && !ul.lowLoadSince.IsZero() && now.Sub(ul.lowLoadSince) >= crossDelayMinLowLoad {
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
	ds.lastDeltaUs = deltaUs
	exceeded := deltaUs > thresholdUs
	ds.lastHighDelay = exceeded
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
	if ds.paused {
		return
	}

	bbDetected := bbCount >= c.cfg.BufferbloatDetectionThr
	bbRefractoryDur := time.Duration(c.cfg.BufferbloatRefractoryPeriodMs) * time.Millisecond

	if bbDetected {
		// With RefractoryOnDecrease, decrease the rate only one time in
		// each refractory period. After a decrease, the detection window
		// keeps the old delayed pings for some time. Without the refractory
		// period, each new ping decreases the rate again before the
		// decrease can have an effect.
		// With RepeatDecreaseOnRise, decrease the rate again in the
		// refractory period only while the delay increases.
		inRefractory := ds.bbDetected && now.Sub(ds.lastBBTime) <= bbRefractoryDur
		if c.cfg.RefractoryOnDecrease && inRefractory {
			return
		}
		if c.cfg.RepeatDecreaseOnRise && inRefractory && deltaUs <= ds.lastBBDeltaUs {
			return
		}
		ds.bbDetected = true
		ds.lastBBTime = now
		ds.lastBBDeltaUs = deltaUs
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
	if !c.dl.paused {
		c.dl.shaperRateKbps = float64(dlKbps)
	}
	if !c.ul.paused {
		c.ul.shaperRateKbps = float64(ulKbps)
	}
	c.applyRate(DL)
	c.applyRate(UL)
}

func (c *Core) applyRate(dir Direction) {
	ds := c.getDirState(dir)
	if ds.paused || !c.link.DirConfig(dir).Adjust {
		return
	}
	rate := int(math.Round(ds.shaperRateKbps))
	if rate != ds.lastRequestedKbps {
		ds.lastRequestedKbps = rate
		ds.rateChanges++
	}
	// Always send the rate: after a stall, the system can have changed it.
	c.act.SetRate(dir, rate)
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
