// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// RateStats holds the achieved throughput for both directions.
type RateStats struct {
	DlRateKbps float64
	UlRateKbps float64
	Timestamp  time.Time
}

// Monitor reads interface statistics and calculates achieved throughput.
type Monitor struct {
	dlIface  string
	ulIface  string
	interval time.Duration
	logger   *Logger
}

// NewMonitor creates a new interface rate monitor.
func NewMonitor(dlIface, ulIface string, intervalMs int, logger *Logger) *Monitor {
	return &Monitor{
		dlIface:  dlIface,
		ulIface:  ulIface,
		interval: time.Duration(intervalMs) * time.Millisecond,
		logger:   logger,
	}
}

// Run starts the monitor loop, sending RateStats to the provided channel.
func (m *Monitor) Run(ctx context.Context, ch chan<- RateStats) {
	prevDlBytes, err := readIfaceBytes(m.dlIface, "rx")
	if err != nil {
		m.logger.Errorf("reading initial dl bytes: %v", err)
		return
	}
	prevUlBytes, err := readIfaceBytes(m.ulIface, "tx")
	if err != nil {
		m.logger.Errorf("reading initial ul bytes: %v", err)
		return
	}
	prevTime := time.Now()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed <= 0 {
				continue
			}

			dlBytes, err := readIfaceBytes(m.dlIface, "rx")
			if err != nil {
				m.logger.Debugf("reading dl bytes: %v", err)
				continue
			}
			ulBytes, err := readIfaceBytes(m.ulIface, "tx")
			if err != nil {
				m.logger.Debugf("reading ul bytes: %v", err)
				continue
			}

			dlDelta := dlBytes - prevDlBytes
			ulDelta := ulBytes - prevUlBytes

			// Handle counter wraparound
			if dlDelta < 0 {
				dlDelta = 0
			}
			if ulDelta < 0 {
				ulDelta = 0
			}

			stats := RateStats{
				DlRateKbps: float64(dlDelta*8) / (elapsed * 1000.0),
				UlRateKbps: float64(ulDelta*8) / (elapsed * 1000.0),
				Timestamp:  now,
			}

			prevDlBytes = dlBytes
			prevUlBytes = ulBytes
			prevTime = now

			select {
			case ch <- stats:
			default:
				// Drop stats if channel is full (non-blocking)
			}
		}
	}
}

// readIfaceBytes reads the byte counter for an interface from sysfs.
// direction is "rx" or "tx".
func readIfaceBytes(iface, direction string) (int64, error) {
	path := fmt.Sprintf("/sys/class/net/%s/statistics/%s_bytes", iface, direction)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	val, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	return val, nil
}
