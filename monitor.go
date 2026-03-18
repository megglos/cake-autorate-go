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
	"io"
	"os"
	"strconv"
	"time"
)

// RateStats holds the achieved throughput for both directions.
type RateStats struct {
	DlRateKbps float64
	UlRateKbps float64
	Timestamp  time.Time
}

// sysfsCounter holds a persistent file descriptor for a sysfs statistics file.
// Using pread avoids the open/read/close cycle on every sample.
// If a read fails (e.g. interface removed), reopen() can refresh the fd.
type sysfsCounter struct {
	file *os.File
	path string
	buf  [32]byte // sysfs counters are at most ~20 digits
}

// openSysfsCounter opens a sysfs statistics file for persistent reading.
func openSysfsCounter(iface, stat string) (*sysfsCounter, error) {
	path := fmt.Sprintf("/sys/class/net/%s/statistics/%s", iface, stat)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &sysfsCounter{file: f, path: path}, nil
}

// read returns the current counter value using pread (no seek needed).
func (sc *sysfsCounter) read() (int64, error) {
	n, err := sc.file.ReadAt(sc.buf[:], 0)
	if n == 0 {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("empty read from %s", sc.path)
	}
	// sysfs commonly returns io.EOF alongside valid data; any other
	// error indicates a real problem (e.g. device removed mid-read).
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("reading %s: %w", sc.path, err)
	}
	// Trim trailing whitespace (newline)
	end := n
	for end > 0 && (sc.buf[end-1] == '\n' || sc.buf[end-1] == ' ') {
		end--
	}
	return strconv.ParseInt(string(sc.buf[:end]), 10, 64)
}

// reopen closes the stale file descriptor and opens a fresh one.
// Returns nil on success, allowing the caller to resume reading.
func (sc *sysfsCounter) reopen() error {
	sc.file.Close()
	f, err := os.Open(sc.path)
	if err != nil {
		return err
	}
	sc.file = f
	return nil
}

// close releases the file descriptor.
func (sc *sysfsCounter) close() {
	sc.file.Close()
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
// Opens persistent file descriptors to sysfs counters to avoid open/close overhead.
func (m *Monitor) Run(ctx context.Context, ch chan<- RateStats) {
	dlCounter, err := openSysfsCounter(m.dlIface, "rx_bytes")
	if err != nil {
		m.logger.Errorf("opening dl counter: %v", err)
		return
	}
	defer dlCounter.close()

	ulCounter, err := openSysfsCounter(m.ulIface, "tx_bytes")
	if err != nil {
		m.logger.Errorf("opening ul counter: %v", err)
		return
	}
	defer ulCounter.close()

	prevDlBytes, err := dlCounter.read()
	if err != nil {
		m.logger.Errorf("reading initial dl bytes: %v", err)
		return
	}
	prevUlBytes, err := ulCounter.read()
	if err != nil {
		m.logger.Errorf("reading initial ul bytes: %v", err)
		return
	}
	prevTime := time.Now()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// dlStale/ulStale track whether the fd needs reopening after a read error.
	dlStale := false
	ulStale := false

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

			// Attempt to reopen stale file descriptors (interface may have reappeared).
			// When either counter is reopened, reset both baselines and prevTime
			// to avoid a bogus rate spike in the other direction.
			if dlStale {
				if err := dlCounter.reopen(); err != nil {
					m.logger.Debugf("reopening dl counter: %v", err)
					continue
				}
				m.logger.Infof("dl counter reopened for %s", m.dlIface)
				dlStale = false
				prevDlBytes, err = dlCounter.read()
				if err != nil {
					m.logger.Debugf("reading dl bytes after reopen: %v", err)
					dlStale = true
					continue
				}
				if !ulStale {
					if v, e := ulCounter.read(); e == nil {
						prevUlBytes = v
					}
				}
				prevTime = now
				continue
			}
			if ulStale {
				if err := ulCounter.reopen(); err != nil {
					m.logger.Debugf("reopening ul counter: %v", err)
					continue
				}
				m.logger.Infof("ul counter reopened for %s", m.ulIface)
				ulStale = false
				prevUlBytes, err = ulCounter.read()
				if err != nil {
					m.logger.Debugf("reading ul bytes after reopen: %v", err)
					ulStale = true
					continue
				}
				if !dlStale {
					if v, e := dlCounter.read(); e == nil {
						prevDlBytes = v
					}
				}
				prevTime = now
				continue
			}

			dlBytes, err := dlCounter.read()
			if err != nil {
				m.logger.Debugf("reading dl bytes: %v", err)
				dlStale = true
				continue
			}
			ulBytes, err := ulCounter.read()
			if err != nil {
				m.logger.Debugf("reading ul bytes: %v", err)
				ulStale = true
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
