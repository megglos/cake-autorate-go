// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// PingResult holds the result of a single ping to a reflector.
type PingResult struct {
	Reflector string
	RTT       time.Duration
	Timestamp time.Time
	Timeout   bool

	// OWD is true if the reply contains one-way delays (ICMP timestamps).
	// UlOWD is the delay from this host to the reflector, DlOWD is the delay
	// back. The two values contain the clock offset between the hosts, with
	// a different sign. Thus, only a change of each value is a delay
	// change. The resolution is 1 ms.
	OWD   bool
	UlOWD time.Duration
	DlOWD time.Duration
	// ReflectorTime is the time in the reflector, from the receive to the
	// transmit time. The RTT contains it, the one-way delays do not.
	ReflectorTime time.Duration
}

// ReflectorState is the health data of a reflector. Core keeps the delay
// data (baseline RTT, delta EWMA).
type ReflectorState struct {
	mu           sync.Mutex
	MissedWindow []bool // sliding window of missed responses
	MissedIdx    int
	Active       bool
}

// PingerManager manages ICMP pingers to multiple reflectors.
type PingerManager struct {
	cfg        *Config
	link       *LinkConfig
	logger     *Logger
	reflectors []string
	states     map[string]*ReflectorState
	mu         sync.RWMutex
	// linkLoss is true while more than half of the active reflectors miss
	// replies (see ReplaceUnhealthy). It prevents a log message at each
	// health check.
	linkLoss bool
	// onSend, if set, gets each request that a pinger sent.
	// The calibration probe counts them: a request without a result
	// at the end of the probe is lost. It must be safe for parallel calls.
	onSend func(reflector string)
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
// It staggers pings across reflectors within each interval. When ctx is
// cancelled, it returns after all pingers have stopped.
func (pm *PingerManager) Run(ctx context.Context, resultCh chan<- PingResult) {
	active := pm.ActiveReflectors()
	interval := time.Duration(pm.cfg.PingIntervalMs) * time.Millisecond
	stagger := interval / time.Duration(len(active))
	deadline := time.Duration(pm.cfg.ReflectorResponseDeadlineS * float64(time.Second))

	pm.logger.Infof("[%s] starting %d pingers with %v interval", pm.link.Name, len(active), interval)

	var wg sync.WaitGroup
	for i, reflector := range active {
		state := pm.GetState(reflector)
		state.mu.Lock()
		state.Active = true
		state.mu.Unlock()

		// Stagger the start of each pinger
		delay := stagger * time.Duration(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			pm.runPinger(ctx, reflector, interval, deadline, delay, resultCh)
		}()
	}

	<-ctx.Done()
	wg.Wait()
}

func (pm *PingerManager) runPinger(ctx context.Context, reflector string, interval, deadline, initialDelay time.Duration, resultCh chan<- PingResult) {
	// Ensure the reflector is marked inactive when this goroutine exits,
	// regardless of the reason (error, context cancellation, etc.).
	defer func() {
		state := pm.GetState(reflector)
		if state != nil {
			state.mu.Lock()
			state.Active = false
			state.mu.Unlock()
		}
	}()

	if initialDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialDelay):
		}
	}

	kind := icmpEcho
	if pm.cfg.PingMethod == PingMethodTimestamp {
		kind = icmpTimestamp
	}
	err := pm.runICMPPinger(ctx, reflector, kind, interval, deadline, resultCh)
	if err != nil && ctx.Err() == nil {
		pm.logger.Errorf("pinger %s: %v", reflector, err)
		// Record a miss and send a timeout: then the controller sees at
		// once that the pinger stopped.
		pm.recordHealth(reflector, true)
		select {
		case resultCh <- PingResult{Reflector: reflector, Timestamp: time.Now(), Timeout: true}:
		case <-ctx.Done():
		}
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

// ReplaceUnhealthy examines the health of the reflectors. It replaces each
// reflector that does not operate correctly with a spare from the pool. It
// returns the spares that it made active. If it returns spares, reset their
// delay data and start the pingers again with the new set.
func (pm *PingerManager) ReplaceUnhealthy() []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	activeCount := pm.cfg.PingerCount
	if activeCount > len(pm.reflectors) {
		activeCount = len(pm.reflectors)
	}

	missedOf := func(r string) (missed, window int) {
		state := pm.states[r]
		if state == nil {
			return 0, 0
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		for _, m := range state.MissedWindow {
			if m {
				missed++
			}
		}
		return missed, len(state.MissedWindow)
	}

	// If more than half of the active reflectors miss replies at the same
	// time, the loss is on the link (for example, under high load), not in
	// the reflectors. A replacement then removes good reflectors, and the
	// new reflectors get their baselines while the link has a queue. One
	// reflector alone does not show a loss on the link: with pinger_count
	// 1, a reflector that misses replies is replaced.
	//
	// A loss on the link is small and affects all reflectors. If some
	// reflectors miss half or more of their pings and other active
	// reflectors are healthy, those reflectors do not operate (for example,
	// they stopped replying). They are replaced also in a loss on the link.
	thr := pm.cfg.ReflectorMisbehavingDetectionThr
	bad, healthy := 0, 0
	for _, r := range pm.reflectors[:activeCount] {
		switch m, _ := missedOf(r); {
		case m >= thr:
			bad++
		case m < thr:
			healthy++
		}
	}
	linkLoss := bad >= 2 && 2*bad > activeCount
	if linkLoss && !pm.linkLoss {
		pm.logger.Infof("[%s] %d of %d reflectors miss replies at the same time: loss on the link, only reflectors that do not operate are replaced",
			pm.link.Name, bad, activeCount)
	}
	pm.linkLoss = linkLoss
	// replace tells if a reflector with the missed replies must go.
	replace := func(missed, window int) bool {
		if missed < thr {
			return false
		}
		if !linkLoss {
			return true
		}
		return healthy > 0 && 2*missed >= window
	}

	var promoted []string
	for i := 0; i < activeCount; i++ {
		state := pm.states[pm.reflectors[i]]
		if state == nil {
			continue
		}
		missed, window := missedOf(pm.reflectors[i])

		if replace(missed, window) && len(pm.reflectors) > activeCount {
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
			spare := pm.reflectors[activeCount-1]
			promoted = append(promoted, spare)
			newState := pm.states[spare]
			if newState != nil {
				newState.mu.Lock()
				newState.MissedWindow = make([]bool, pm.cfg.ReflectorMisbehavingDetectionWindow)
				newState.MissedIdx = 0
				newState.mu.Unlock()
			}
			i-- // re-evaluate this index since a new reflector shifted in
		}
	}
	return promoted
}

// ReflectorStatus is the health of a reflector, for the status file.
type ReflectorStatus struct {
	Address string `json:"address"`
	Active  bool   `json:"active"`
	// Missed is the number of missed replies in the detection window.
	Missed int `json:"missed"`
}

// Snapshot returns the health of all reflectors in the order of the pool.
// The reflectors of the selected set come first. Active is true while the
// pinger of the reflector runs.
func (pm *PingerManager) Snapshot() []ReflectorStatus {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]ReflectorStatus, 0, len(pm.reflectors))
	for _, r := range pm.reflectors {
		st := ReflectorStatus{Address: r}
		if state := pm.states[r]; state != nil {
			state.mu.Lock()
			st.Active = state.Active
			for _, m := range state.MissedWindow {
				if m {
					st.Missed++
				}
			}
			state.mu.Unlock()
		}
		out = append(out, st)
	}
	return out
}
