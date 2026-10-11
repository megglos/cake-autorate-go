// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Licensed under GPL-2.0

package main

// This file contains a link simulator. It uses capacity traces to test the
// controller without a real connection.
//
// The simulator uses a fluid model with two queues in series for each
// direction. The first queue is the CAKE shaper, with the rate from the
// controller. The second queue is the ISP bottleneck, with the rate from a
// capacity trace. On upload, the shaper is before the bottleneck. On
// download, the shaper is after the bottleneck (ingress shaping on an IFB).
//
// In each direction, a bulk sender sends as much data as it can. It uses a
// window-based AIMD model, and its rate is cwnd / RTT. The sender decreases
// its window when CAKE drops packets (CoDel: the sojourn time stays above the
// target for one interval). It also decreases its window when the
// bottleneck buffer is full.
//
// Pings and latency-sensitive traffic are sparse flows. CAKE keeps these
// flows away from the bulk queue. Thus, their delay is the base RTT plus the
// bottleneck queue delay in the two directions. Autorate must keep this
// delay low and the throughput high.
//
// Units: rates are in kbit/s and times are in ms. Thus, rate*time is bits.

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

const (
	simStepMs        = 1.0
	simMSSBits       = 1500 * 8
	simCodelTargetMs = 5.0
	simCodelIntvlMs  = 100.0
	simUnshapedKbps  = 1e9
	simProbeEveryMs  = 10
	simBloatThrMs    = 30.0 // excess probe delay above this value is bloat time
)

// simDirection is the model of one traffic direction.
type simDirection struct {
	capacity    func(t float64) float64 // kbps at time t (ms)
	demand      func(t float64) bool    // true when a bulk transfer is active
	shaperFirst bool                    // UL: shaper, then bottleneck
	bufferMs    float64                 // bottleneck buffer size, in ms at the current capacity
	flows       int

	shaperKbps  float64
	qShaper     float64 // bits
	qBottleneck float64 // bits
	cwnd        float64 // bits, total for all flows
	slowStart   bool
	lastSignal  float64
	codelAbove  float64 // time when the CAKE sojourn went above the target, -1 if below

	shaperOut float64 // total bits out of CAKE (interface counter)
	delivered float64 // total bits out of the path
}

func (d *simDirection) bottleneckDelayMs(t float64) float64 {
	return d.qBottleneck / math.Max(d.capacity(t), 1)
}

func (d *simDirection) shaperDelayMs() float64 {
	return d.qShaper / math.Max(d.shaperKbps, 1)
}

// step moves the direction forward by dt ms. baseRTTMs is the path RTT
// without queues. reverseMs is the bottleneck delay of the reverse direction.
// The ACKs wait in that queue. CAKE keeps the ACKs away from the reverse
// bulk queue, but the bottleneck does not.
func (d *simDirection) step(t, dt, baseRTTMs, reverseMs float64) {
	capKbps := d.capacity(t)
	rtt := baseRTTMs + d.shaperDelayMs() + d.bottleneckDelayMs(t) + reverseMs

	if !d.demand(t) {
		d.cwnd = 0
	} else if d.cwnd == 0 {
		d.cwnd = float64(10*d.flows) * simMSSBits // initial window
		d.slowStart = true
		d.lastSignal = t
	}

	// Self-clocked sender: a maximum of cwnd bits in flight for each RTT.
	in := d.cwnd / rtt * dt

	q1, r1 := &d.qShaper, d.shaperKbps
	q2, r2 := &d.qBottleneck, capKbps
	if !d.shaperFirst {
		q1, r1, q2, r2 = q2, r2, q1, r1
	}
	*q1 += in
	out1 := math.Min(*q1, r1*dt)
	*q1 -= out1
	*q2 += out1
	out2 := math.Min(*q2, r2*dt)
	*q2 -= out2
	if d.shaperFirst {
		d.shaperOut += out1
	} else {
		d.shaperOut += out2
	}
	d.delivered += out2

	congested := false
	if buf := d.bufferMs * capKbps; d.qBottleneck > buf {
		d.qBottleneck = buf
		congested = true
	}
	if d.shaperDelayMs() > simCodelTargetMs {
		if d.codelAbove < 0 {
			d.codelAbove = t
		} else if t-d.codelAbove > simCodelIntvlMs {
			congested = true
		}
	} else {
		d.codelAbove = -1
	}

	if d.cwnd == 0 {
		return
	}
	switch {
	case congested && t-d.lastSignal > rtt:
		// For each signal, one flow decreases its window (CUBIC beta = 0.7).
		d.lastSignal = t
		if d.slowStart {
			d.slowStart = false
			d.cwnd *= 0.5
		} else {
			d.cwnd *= 1 - 0.3/float64(d.flows)
		}
	case d.slowStart:
		d.cwnd += d.cwnd * dt / rtt
	default:
		// Reno: each flow adds one MSS for each RTT.
		d.cwnd += float64(d.flows) * simMSSBits * dt / rtt
	}
	d.cwnd = math.Max(d.cwnd, float64(2*d.flows)*simMSSBits)
}

// simScenario describes a link and its traffic.
type simScenario struct {
	name     string
	duration time.Duration
	seed     uint64

	dlCapacity, ulCapacity func(t float64) float64
	dlDemand, ulDemand     func(t float64) bool
	bufferMs               float64
	flows                  int

	// baseRTT returns the RTT without load to reflector i at time t (ms).
	baseRTT func(t float64, i int) float64
	// jitterMs is the mean jitter for each ping (exponential distribution).
	jitterMs float64
	// pingLoss is the probability that a ping gets no reply. Core then gets
	// a timeout after reflector_response_deadline_s.
	pingLoss float64
	// outage returns true while the link transmits nothing (optional). Then
	// the capacity is almost zero and no ping gets a reply.
	outage func(t float64) bool
	// ulCouplingMs is the upload delay that the radio adds when the download
	// is active. A shaper on the upload cannot decrease it (5G: the scheduler
	// gives fewer upload grants when the download is busy). On the 5G link,
	// the delay was the same for all download rates from 25 to 300 Mbit/s.
	// Thus, the delay increases to the full value at 20 % download use and
	// then stays constant.
	ulCouplingMs float64
	// ulCouplingJitterMs is the mean of an additional random upload delay
	// for each ping when the download is active (exponential distribution).
	// On the 5G link, the delay from download activity was 10 to 46 ms
	// (p10 to p95), with a median of 21 ms.
	ulCouplingJitterMs float64
	// ulGrantMs is the upload delay that the radio adds when the upload is
	// active. ulGrantJitterMs is the mean of an additional random delay for
	// each ping (exponential distribution). On 5G, the upload data waits
	// for a grant of the scheduler. On the 5G link, upload activity
	// increased the upload delay by 14 ms (p50) and 24 ms (p95), also at
	// 1/6 of the upload capacity. A shaper cannot remove it. Thus, the delay
	// increases to the full value at 5 % upload use and then stays
	// constant.
	ulGrantMs, ulGrantJitterMs float64

	// link contains the rate limits and thresholds. The simulator sets Adjust=true.
	link LinkConfig
	// tune changes the controller config (optional).
	tune func(cfg *Config)
	// recordPath is the file for a recording of the run (optional).
	recordPath string
}

// simPolicy selects what sets the shaper rate.
type simPolicy string

const (
	policyAutorate simPolicy = "autorate"
	// policyAutorateOWD is autorate with one-way delays (ICMP timestamps)
	// instead of RTT/2.
	policyAutorateOWD simPolicy = "autorate-owd"
	policyUnshaped    simPolicy = "unshaped"
	policyFixed       simPolicy = "fixed-base"
)

// simResult is the scorecard for one run.
type simResult struct {
	scenario string
	policy   simPolicy

	// Excess delay (ms) of a sparse flow. The simulator samples it every 10ms
	// while a bulk transfer is active.
	latP50, latP95, latP99 float64
	// p95 of the delay of each direction (ms), while the direction has
	// demand. It includes the random parts of the path delay (jitter, and
	// the coupling and grant noise) of the last ping: the latency target
	// measures them too. Without autorate (no pings), they are NaN.
	dlLatP95, ulLatP95 float64
	// bloatS is the total time with an excess delay of more than 30ms.
	bloatS float64
	// Delivered bits / capacity bits, while the direction has demand.
	dlUtil, ulUtil float64

	// Shaper rate over time (one sample every 100ms), for analysis.
	dlShaper, ulShaper []float64
	// Learned upload cross delay (us) over time (one sample every 100ms).
	ulCrossDelay []float64
	// Number of SetRate calls.
	rateChanges int
	// stallS is the total time in StateStall. stallEnd is the time (ms) of
	// the last change from StateStall to StateRunning, or -1.
	stallS   float64
	stallEnd float64
}

// simActuator applies the decisions of Core to the simulated shaper.
type simActuator struct {
	dl, ul  *simDirection
	pinging bool
	changes int
}

func (a *simActuator) SetRate(dir Direction, rateKbps int) {
	a.changes++
	if dir == DL {
		a.dl.shaperKbps = float64(rateKbps)
	} else {
		a.ul.shaperKbps = float64(rateKbps)
	}
}
func (a *simActuator) StartPingers()     { a.pinging = true }
func (a *simActuator) StopPingers()      { a.pinging = false }
func (a *simActuator) InvalidateShaper() {}

type simPendingPing struct {
	at     float64
	result PingResult
}

// runSim runs a scenario with a policy and returns the scorecard.
func runSim(sc simScenario, policy simPolicy) simResult {
	rng := rand.New(rand.NewPCG(sc.seed, 0))

	cfg := DefaultConfig()
	link := sc.link
	link.Name = sc.name
	link.Download.Adjust = true
	link.Upload.Adjust = true
	if len(link.Reflectors) == 0 {
		link.Reflectors = cfg.Reflectors
	}
	cfg.Links = []LinkConfig{link}
	if sc.tune != nil {
		sc.tune(cfg)
	}
	flows := sc.flows
	if flows == 0 {
		flows = 4
	}
	if sc.outage != nil {
		// During an outage, the link transmits almost nothing.
		dlCap, ulCap, down := sc.dlCapacity, sc.ulCapacity, sc.outage
		sc.dlCapacity = func(t float64) float64 {
			if down(t) {
				return 1
			}
			return dlCap(t)
		}
		sc.ulCapacity = func(t float64) float64 {
			if down(t) {
				return 1
			}
			return ulCap(t)
		}
	}
	deadlineMs := cfg.ReflectorResponseDeadlineS * 1000

	dl := &simDirection{capacity: sc.dlCapacity, demand: sc.dlDemand, bufferMs: sc.bufferMs, flows: flows, codelAbove: -1}
	ul := &simDirection{capacity: sc.ulCapacity, demand: sc.ulDemand, bufferMs: sc.bufferMs, flows: flows, codelAbove: -1, shaperFirst: true}
	act := &simActuator{dl: dl, ul: ul, pinging: true}

	switch policy {
	case policyUnshaped:
		dl.shaperKbps, ul.shaperKbps = simUnshapedKbps, simUnshapedKbps
	case policyFixed:
		dl.shaperKbps, ul.shaperKbps = float64(link.Download.BaseRateKbps), float64(link.Upload.BaseRateKbps)
	}

	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(t float64) time.Time { return epoch.Add(time.Duration(t * float64(time.Millisecond))) }

	owd := policy == policyAutorateOWD
	var core *RecordingCore
	if policy == policyAutorate || owd {
		logger := NewDiscardLogger()
		defer logger.Close()
		var rec *Recorder
		if sc.recordPath != "" {
			var err error
			if rec, err = newRecorderAt(sc.recordPath, 0, cfg, epoch, logger); err != nil {
				panic(err)
			}
			defer rec.Close()
		}
		core = NewRecordingCore(&cfg.Links[0], cfg, defaultMTU, defaultMTU, act, rec, logger)
		core.Start(at(0))
	}

	// Pingers: PingerCount reflectors, with start times spread across the interval.
	nPingers := min(cfg.PingerCount, len(link.Reflectors))
	pingIntvl := float64(cfg.PingIntervalMs)
	nextPing := make([]float64, nPingers)
	for i := range nextPing {
		nextPing[i] = pingIntvl * float64(i) / float64(nPingers)
	}
	var pending []simPendingPing

	monIntvl := float64(cfg.MonitorIntervalMs)
	nextMon := monIntvl
	var prevDlOut, prevUlOut float64

	var lat, dlLat, ulLat []float64
	// Random path delay of the last ping, for dlLat and ulLat.
	var dlNoiseMs, ulNoiseMs float64
	var res simResult
	res.stallEnd = -1
	wasStall := false
	res.scenario, res.policy = sc.name, policy
	var dlDelivered, dlCap, ulDelivered, ulCap float64
	var dlUse float64 // EWMA of download rate / capacity
	var ulUse float64 // EWMA of upload rate / capacity

	end := float64(sc.duration.Milliseconds())
	step := 0
	for t := 0.0; t < end; t += simStepMs {
		base0 := sc.baseRTT(t, 0)
		dlBefore, ulBefore := dl.delivered, ul.delivered
		couplingActive := math.Min(dlUse/0.2, 1)
		grantActive := math.Min(ulUse/0.05, 1)
		couplingMs := (sc.ulCouplingMs+sc.ulCouplingJitterMs)*couplingActive +
			(sc.ulGrantMs+sc.ulGrantJitterMs)*grantActive
		dlRev, ulRev := ul.bottleneckDelayMs(t)+couplingMs, dl.bottleneckDelayMs(t)
		dl.step(t, simStepMs, base0, dlRev)
		// The upload TCP flows also see the coupling delay on their
		// forward path.
		ul.step(t, simStepMs, base0+couplingMs, ulRev)
		dlUse += 0.01 * ((dl.delivered-dlBefore)/simStepMs/math.Max(dl.capacity(t), 1) - dlUse)
		ulUse += 0.01 * ((ul.delivered-ulBefore)/simStepMs/math.Max(ul.capacity(t), 1) - ulUse)
		if dl.demand(t) {
			dlDelivered += dl.delivered - dlBefore
			dlCap += dl.capacity(t) * simStepMs
		}
		if ul.demand(t) {
			ulDelivered += ul.delivered - ulBefore
			ulCap += ul.capacity(t) * simStepMs
		}

		dlQueueMs, ulQueueMs := dl.bottleneckDelayMs(t), ul.bottleneckDelayMs(t)+couplingMs
		queueMs := dlQueueMs + ulQueueMs

		if core != nil {
			// Send pings
			for i := range nextPing {
				if t < nextPing[i] {
					continue
				}
				nextPing[i] += pingIntvl
				if !act.pinging {
					continue
				}
				base := sc.baseRTT(t, i)
				jitter := sc.jitterMs * rng.ExpFloat64()
				// The random parts of the coupling delay and the grant delay
				// are different for each ping. Their means are in couplingMs.
				// Draw a random number only if the scenario has the delay.
				// Then the other scenarios keep the same random sequence.
				var ulNoise float64
				if sc.ulCouplingJitterMs > 0 {
					ulNoise = sc.ulCouplingJitterMs * (rng.ExpFloat64() - 1) * couplingActive
				}
				if sc.ulGrantJitterMs > 0 {
					ulNoise += sc.ulGrantJitterMs * (rng.ExpFloat64() - 1) * grantActive
				}
				rtt := base + queueMs + jitter + ulNoise
				dlNoiseMs, ulNoiseMs = jitter/2, jitter/2+ulNoise
				result := PingResult{
					Reflector: link.Reflectors[i],
					RTT:       time.Duration(rtt * float64(time.Millisecond)),
					Timestamp: at(t + rtt),
				}
				if owd {
					// Each reflector has a different clock offset. The
					// timestamps have a resolution of 1 ms.
					offset := 13.7*float64(i) - 40
					ul := base/2 + ulQueueMs + jitter/2 + ulNoise + offset
					dl := base/2 + dlQueueMs + jitter/2 - offset
					result.OWD = true
					result.UlOWD = time.Duration(math.Round(ul)) * time.Millisecond
					result.DlOWD = time.Duration(math.Round(dl)) * time.Millisecond
				}
				// Draw a random number only with ping loss. Then the other
				// scenarios keep the same random sequence (and baseline).
				lost := sc.pingLoss > 0 && rng.Float64() < sc.pingLoss
				if lost || (sc.outage != nil && sc.outage(t)) {
					// No reply: the pinger sends a timeout after the deadline.
					result = PingResult{Reflector: result.Reflector, Timestamp: at(t + deadlineMs), Timeout: true}
					pending = append(pending, simPendingPing{at: t + deadlineMs, result: result})
					continue
				}
				pending = append(pending, simPendingPing{at: t + rtt, result: result})
			}
			// Deliver the replies that are due, in the order of arrival
			sort.Slice(pending, func(a, b int) bool { return pending[a].at < pending[b].at })
			n := 0
			for n < len(pending) && pending[n].at <= t {
				core.HandlePing(pending[n].result)
				n++
			}
			pending = pending[n:]

			// Throughput monitor
			if t >= nextMon {
				core.HandleRate(RateStats{
					DlRateKbps: (dl.shaperOut - prevDlOut) / monIntvl,
					UlRateKbps: (ul.shaperOut - prevUlOut) / monIntvl,
					Timestamp:  at(t),
				})
				prevDlOut, prevUlOut = dl.shaperOut, ul.shaperOut
				nextMon += monIntvl
			}
		}

		if step%simProbeEveryMs == 0 && (dl.demand(t) || ul.demand(t)) {
			lat = append(lat, queueMs)
			if dl.demand(t) {
				dlLat = append(dlLat, math.Max(dlQueueMs+dlNoiseMs, 0))
			}
			if ul.demand(t) {
				ulLat = append(ulLat, math.Max(ulQueueMs+ulNoiseMs, 0))
			}
			if queueMs > simBloatThrMs {
				res.bloatS += simProbeEveryMs / 1000.0
			}
		}
		if core != nil {
			if core.Core().State() == StateStall {
				res.stallS += simStepMs / 1000
				wasStall = true
			} else if wasStall {
				res.stallEnd, wasStall = t, false
			}
		}
		if step%100 == 0 {
			res.dlShaper = append(res.dlShaper, math.Min(dl.shaperKbps, dl.capacity(t)*10))
			res.ulShaper = append(res.ulShaper, math.Min(ul.shaperKbps, ul.capacity(t)*10))
			if core != nil {
				res.ulCrossDelay = append(res.ulCrossDelay, core.Core().ul.crossDelayUs)
			}
		}
		step++
	}

	sort.Float64s(lat)
	res.latP50, res.latP95, res.latP99 = percentile(lat, 0.50), percentile(lat, 0.95), percentile(lat, 0.99)
	sort.Float64s(dlLat)
	sort.Float64s(ulLat)
	res.dlLatP95, res.ulLatP95 = percentile(dlLat, 0.95), percentile(ulLat, 0.95)
	if core == nil {
		res.dlLatP95, res.ulLatP95 = math.NaN(), math.NaN()
	}
	if dlCap > 0 {
		res.dlUtil = dlDelivered / dlCap
	}
	if ulCap > 0 {
		res.ulUtil = ulDelivered / ulCap
	}
	res.rateChanges = act.changes
	return res
}

// percentile returns the p-quantile of the sorted values.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(int(p*float64(len(sorted))), len(sorted)-1)]
}
