// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"strings"
)

// LinkMode is the trade-off between delay and throughput of a link. A mode
// sets the options refractory_on_decrease, increase_to_delay_thr and
// auto_max_rate:
//
//   - latency: all off.
//   - balanced: refractory_on_decrease.
//   - bandwidth: all on.
//   - custom: the values of the config.
type LinkMode string

// Link modes.
const (
	ModeLatency   LinkMode = "latency"
	ModeBalanced  LinkMode = "balanced"
	ModeBandwidth LinkMode = "bandwidth"
	ModeCustom    LinkMode = "custom"
)

// linkModes are the modes, from the lowest delay to the highest throughput,
// and then custom.
var linkModes = []LinkMode{ModeLatency, ModeBalanced, ModeBandwidth, ModeCustom}

// parseLinkMode returns the mode with the name s.
func parseLinkMode(s string) (LinkMode, error) {
	for _, m := range linkModes {
		if string(m) == s {
			return m, nil
		}
	}
	names := make([]string, len(linkModes))
	for i, m := range linkModes {
		names[i] = string(m)
	}
	return "", fmt.Errorf("unknown mode %q, use %s", s, strings.Join(names, ", "))
}

// defaultMode returns the mode for a config without the key "mode": custom
// if the config turns on one of the options of the modes (a config from
// before the modes), else balanced.
func (c *Config) defaultMode() LinkMode {
	if c.RefractoryOnDecrease || c.IncreaseToDelayThr || c.AutoMaxRate {
		return ModeCustom
	}
	return ModeBalanced
}

// linkModeOf returns the mode of the link after the start: the mode of the
// link, or the top-level mode. An empty mode is custom (a config that
// LoadConfig did not read, for example in the tests).
func linkModeOf(cfg *Config, link *LinkConfig) LinkMode {
	m := cfg.Mode
	if link.Mode != nil {
		m = *link.Mode
	}
	if m == "" {
		return ModeCustom
	}
	return LinkMode(m)
}

// Mode returns the current mode.
func (c *Core) Mode() LinkMode { return c.mode }

// SetMode changes the mode. The new options apply from the next ping
// reply. The time at the maximum (auto_max_rate) starts again. If the new
// mode has no auto_max_rate, the learned maximum is removed, and a rate
// above max_rate_kbps decreases to it.
func (c *Core) SetMode(m LinkMode) {
	if m == c.mode {
		return
	}
	c.mode = m
	for _, dir := range Directions() {
		ds := c.getDirState(dir)
		ds.autoMaxBreak()
		if c.autoMaxRate() {
			continue
		}
		ds.autoMaxKbps = 0
		if maxKbps := c.maxRateKbps(dir); ds.shaperRateKbps > maxKbps && !ds.paused {
			ds.shaperRateKbps = maxKbps
			c.applyRate(dir)
		}
	}
}

// refractoryOnDecrease reports whether refractory_on_decrease applies in
// the current mode.
func (c *Core) refractoryOnDecrease() bool {
	switch c.mode {
	case ModeLatency:
		return false
	case ModeBalanced, ModeBandwidth:
		return true
	}
	return c.cfg.RefractoryOnDecrease
}

// increaseToDelayThr reports whether increase_to_delay_thr applies in the
// current mode.
func (c *Core) increaseToDelayThr() bool {
	switch c.mode {
	case ModeLatency, ModeBalanced:
		return false
	case ModeBandwidth:
		return true
	}
	return c.cfg.IncreaseToDelayThr
}

// autoMaxRate reports whether auto_max_rate applies in the current mode.
func (c *Core) autoMaxRate() bool {
	switch c.mode {
	case ModeLatency, ModeBalanced:
		return false
	case ModeBandwidth:
		return true
	}
	return c.cfg.AutoMaxRate
}

// maxModeSeconds is the maximum time of "mode LINK MODE SECONDS".
const maxModeSeconds = 86400
