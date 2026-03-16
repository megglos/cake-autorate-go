// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Shaper controls CAKE qdisc bandwidth settings via the tc command.
type Shaper struct {
	logger *Logger
}

// NewShaper creates a new Shaper.
func NewShaper(logger *Logger) *Shaper {
	return &Shaper{logger: logger}
}

// SetRate sets the CAKE bandwidth for the given interface.
// rateKbps is the target bandwidth in kbit/s.
func (s *Shaper) SetRate(iface string, rateKbps int) error {
	cmd := exec.Command("tc", "qdisc", "change", "dev", iface, "root", "cake", "bandwidth", fmt.Sprintf("%dkbit", rateKbps))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tc qdisc change on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// GetRate reads the current CAKE bandwidth for the given interface.
// Returns the rate in kbit/s.
func (s *Shaper) GetRate(iface string) (int, error) {
	cmd := exec.Command("tc", "qdisc", "show", "dev", iface, "root")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("tc qdisc show on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}

	// Parse output like: "qdisc cake 8001: root refcnt 2 bandwidth 20000Kbit ..."
	line := string(output)
	idx := strings.Index(line, "bandwidth ")
	if idx < 0 {
		return 0, fmt.Errorf("no CAKE bandwidth found on %s", iface)
	}
	rateStr := line[idx+len("bandwidth "):]
	// Find the end of the rate value
	endIdx := strings.IndexAny(rateStr, " \n\t")
	if endIdx > 0 {
		rateStr = rateStr[:endIdx]
	}

	// Parse rate with suffix (Kbit, Mbit, etc.)
	rateStr = strings.TrimSpace(rateStr)
	switch {
	case strings.HasSuffix(rateStr, "Kbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Kbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val), nil
	case strings.HasSuffix(rateStr, "Mbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Mbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val * 1000), nil
	case strings.HasSuffix(rateStr, "Gbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Gbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val * 1000000), nil
	case strings.HasSuffix(rateStr, "bit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "bit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val / 1000), nil
	default:
		return 0, fmt.Errorf("unrecognized rate format %q", rateStr)
	}
}

// WirePacketCompensationUs calculates the serialization delay in microseconds
// for a single MTU-sized packet at the given rate. This is added to the delay
// threshold to prevent false bufferbloat detection at low shaper rates.
func WirePacketCompensationUs(mtuBytes int, rateKbps int) float64 {
	if rateKbps <= 0 {
		return 0
	}
	mtuBits := float64(mtuBytes) * 8.0
	return (1000.0 * mtuBits) / float64(rateKbps)
}
