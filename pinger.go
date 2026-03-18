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
	"math/rand"
	"sync"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

// PingResult holds the result of a single ping to a reflector.
type PingResult struct {
	Reflector string
	RTT       time.Duration
	Timestamp time.Time
	Timeout   bool
}

// ReflectorState tracks the health and baseline RTT for a reflector.
type ReflectorState struct {
	mu             sync.Mutex
	BaselineRTT    float64 // microseconds
	DeltaEWMA      float64 // microseconds
	MissedWindow   []bool  // sliding window of missed responses
	MissedIdx      int
	Active         bool
}

// PingerManager manages ICMP pingers to multiple reflectors.
type PingerManager struct {
	cfg        *Config
	link       *LinkConfig
	logger     *Logger
	reflectors []string
	states     map[string]*ReflectorState
	mu         sync.RWMutex
}

// NewPingerManager creates a new pinger manager for a specific link.
func NewPingerManager(link *LinkConfig, cfg *Config, logger *Logger) *PingerManager {
	// Shuffle reflectors for randomization
	reflectors := make([]string, len(link.Reflectors))
	copy(reflectors, link.Reflectors)
	rand.Shuffle(len(reflectors), func(i, j int) {
		reflectors[i], reflectors[j] = reflectors[j], reflectors[i]
	})

	states := make(map[string]*ReflectorState)
	for _, r := range reflectors {
		states[r] = &ReflectorState{
			MissedWindow: make([]bool, cfg.ReflectorMisbehavingDetectionWindow),
			Active:       false,
		}
	}

	return &PingerManager{
		cfg:        cfg,
		link:       link,
		logger:     logger,
		reflectors: reflectors,
		states:     states,
	}
}

// ActiveReflectors returns the currently active reflector addresses.
func (pm *PingerManager) ActiveReflectors() []string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	count := pm.cfg.PingerCount
	if count > len(pm.reflectors) {
		count = len(pm.reflectors)
	}
	return append([]string(nil), pm.reflectors[:count]...)
}

// GetState returns the state for a reflector.
func (pm *PingerManager) GetState(reflector string) *ReflectorState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.states[reflector]
}

// Run starts pinging active reflectors, sending results to the channel.
// It staggers pings across reflectors within each interval.
func (pm *PingerManager) Run(ctx context.Context, resultCh chan<- PingResult) {
	active := pm.ActiveReflectors()
	interval := time.Duration(pm.cfg.PingIntervalMs) * time.Millisecond
	stagger := interval / time.Duration(len(active))
	deadline := time.Duration(pm.cfg.ReflectorResponseDeadlineS * float64(time.Second))

	pm.logger.Infof("[%s] starting %d pingers with %v interval", pm.link.Name, len(active), interval)

	for i, reflector := range active {
		state := pm.GetState(reflector)
		state.mu.Lock()
		state.Active = true
		state.mu.Unlock()

		// Stagger the start of each pinger
		delay := stagger * time.Duration(i)
		go pm.runPinger(ctx, reflector, interval, deadline, delay, resultCh)
	}

	<-ctx.Done()
}

func (pm *PingerManager) runPinger(ctx context.Context, reflector string, interval, deadline, initialDelay time.Duration, resultCh chan<- PingResult) {
	if initialDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialDelay):
		}
	}

	// Create a single long-running pinger instead of a new one per ping.
	// This avoids per-ping DNS resolution, socket creation, and object allocation.
	pinger, err := probing.NewPinger(reflector)
	if err != nil {
		pm.logger.Errorf("creating pinger for %s: %v", reflector, err)
		return
	}
	pinger.Count = -1 // continuous
	pinger.Interval = interval
	pinger.Timeout = time.Duration(math.MaxInt64) // context controls lifetime
	pinger.SetPrivileged(true)

	if pm.link.PingInterfaceName != "" {
		pinger.InterfaceName = pm.link.PingInterfaceName
	}
	if pm.link.PingSourceAddr != "" {
		pinger.Source = pm.link.PingSourceAddr
	}

	// Track in-flight pings for timeout detection.
	// With deadline (1s) > interval (300ms), up to ~3 pings can be in flight.
	var mu sync.Mutex
	pending := make(map[int]time.Time) // seq -> send time

	pinger.OnSend = func(pkt *probing.Packet) {
		mu.Lock()
		pending[pkt.Seq] = time.Now()
		mu.Unlock()
	}

	pinger.OnRecv = func(pkt *probing.Packet) {
		mu.Lock()
		sendTime, ok := pending[pkt.Seq]
		if ok {
			delete(pending, pkt.Seq)
		}
		mu.Unlock()
		if !ok {
			return // unexpected reply, ignore
		}

		pm.recordHealth(reflector, false)

		select {
		case resultCh <- PingResult{
			Reflector: reflector,
			RTT:       pkt.Rtt,
			Timestamp: sendTime,
			Timeout:   false,
		}:
		case <-ctx.Done():
		}
	}

	// Sweep for timed-out pings
	go func() {
		sweepInterval := deadline / 2
		if sweepInterval < 100*time.Millisecond {
			sweepInterval = 100 * time.Millisecond
		}
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				mu.Lock()
				var expired []int
				for seq, sent := range pending {
					if now.Sub(sent) > deadline {
						expired = append(expired, seq)
					}
				}
				for _, seq := range expired {
					delete(pending, seq)
				}
				mu.Unlock()

				// Send timeout results outside the lock
				for range expired {
					pm.recordHealth(reflector, true)
					select {
					case resultCh <- PingResult{
						Reflector: reflector,
						Timestamp: now,
						Timeout:   true,
					}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	err = pinger.RunWithContext(ctx)
	if err != nil && ctx.Err() == nil {
		pm.logger.Debugf("pinger %s: %v", reflector, err)
	}
}

// recordHealth updates the missed/received sliding window for a reflector.
func (pm *PingerManager) recordHealth(reflector string, missed bool) {
	state := pm.GetState(reflector)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.MissedWindow[state.MissedIdx] = missed
	state.MissedIdx = (state.MissedIdx + 1) % len(state.MissedWindow)
	state.mu.Unlock()
}

// ReplaceUnhealthy checks reflector health and replaces misbehaving ones
// with spare reflectors from the pool. Returns true if any replacements
// were made (callers should restart pingers to pick up the new set).
func (pm *PingerManager) ReplaceUnhealthy() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	activeCount := pm.cfg.PingerCount
	if activeCount > len(pm.reflectors) {
		activeCount = len(pm.reflectors)
	}

	replaced := false
	for i := 0; i < activeCount; i++ {
		state := pm.states[pm.reflectors[i]]
		if state == nil {
			continue
		}
		state.mu.Lock()
		missed := 0
		for _, m := range state.MissedWindow {
			if m {
				missed++
			}
		}
		state.mu.Unlock()

		if missed >= pm.cfg.ReflectorMisbehavingDetectionThr && len(pm.reflectors) > activeCount {
			pm.logger.Infof("[%s] replacing misbehaving reflector %s (missed %d/%d)",
				pm.link.Name, pm.reflectors[i], missed, len(state.MissedWindow))

			// Mark old reflector inactive
			state.mu.Lock()
			state.Active = false
			state.mu.Unlock()

			// Move misbehaving reflector to end, shift spare up
			bad := pm.reflectors[i]
			pm.reflectors = append(pm.reflectors[:i], pm.reflectors[i+1:]...)
			pm.reflectors = append(pm.reflectors, bad)

			// Reset the promoted spare's state (it entered the active set
			// at position activeCount-1 after the shift)
			newState := pm.states[pm.reflectors[activeCount-1]]
			if newState != nil {
				newState.mu.Lock()
				newState.BaselineRTT = 0
				newState.DeltaEWMA = 0
				newState.MissedWindow = make([]bool, pm.cfg.ReflectorMisbehavingDetectionWindow)
				newState.MissedIdx = 0
				newState.mu.Unlock()
			}
			replaced = true
			i-- // re-evaluate this index since a new reflector shifted in
		}
	}
	return replaced
}

// UpdateBaseline updates the baseline RTT for a reflector using EWMA.
func (pm *PingerManager) UpdateBaseline(reflector string, rttUs float64) {
	state := pm.GetState(reflector)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.BaselineRTT == 0 {
		// First measurement — initialize baseline
		state.BaselineRTT = rttUs
		return
	}

	if rttUs < state.BaselineRTT {
		// Delay decreased — track new minimum quickly
		state.BaselineRTT = pm.cfg.AlphaBaselineDecrease*rttUs +
			(1-pm.cfg.AlphaBaselineDecrease)*state.BaselineRTT
	} else {
		// Delay increased — baseline moves slowly
		state.BaselineRTT = pm.cfg.AlphaBaselineIncrease*rttUs +
			(1-pm.cfg.AlphaBaselineIncrease)*state.BaselineRTT
	}
}

// UpdateDeltaEWMA updates the delay delta EWMA for a reflector.
func (pm *PingerManager) UpdateDeltaEWMA(reflector string, deltaUs float64) {
	state := pm.GetState(reflector)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	state.DeltaEWMA = pm.cfg.AlphaDeltaEWMA*deltaUs +
		(1-pm.cfg.AlphaDeltaEWMA)*state.DeltaEWMA
}
