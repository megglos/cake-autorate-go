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

// LinkController runs the control loop for one WAN link. It owns the
// goroutines and the I/O (pingers, throughput monitor, shaper). It sends
// their events to a Core. The Core makes all control decisions.
type LinkController struct {
	name      string
	link      *LinkConfig
	cfg       *Config
	shaper    *Shaper
	pingerMgr *PingerManager
	monitor   *Monitor
	logger    *Logger
	core      *RecordingCore

	// Channels
	pingCh chan PingResult
	rateCh chan RateStats

	// runCtx is the context of Run. The pingers start in this context.
	runCtx context.Context
	// Cancellation for sub-goroutines (pingers)
	pingerCancel context.CancelFunc
}

// NewLinkController creates a new LinkController for a single WAN link.
// recorder can be nil.
func NewLinkController(link *LinkConfig, cfg *Config, shaper *Shaper, recorder *Recorder, logger *Logger) *LinkController {
	c := &LinkController{
		name:      link.Name,
		link:      link,
		cfg:       cfg,
		shaper:    shaper,
		pingerMgr: NewPingerManager(link, cfg, logger),
		monitor:   NewMonitor(link.Download.Interface, link.Upload.Interface, cfg.MonitorIntervalMs, logger),
		logger:    logger,
		pingCh:    make(chan PingResult, 100),
		rateCh:    make(chan RateStats, 10),
	}
	c.core = NewRecordingCore(link, cfg,
		readInterfaceMTU(link.Download.Interface, logger),
		readInterfaceMTU(link.Upload.Interface, logger),
		c, recorder, logger)
	return c
}

// Run starts the controller. Blocks until ctx is cancelled.
func (c *LinkController) Run(ctx context.Context) error {
	c.runCtx = ctx

	if c.cfg.StartupWaitS > 0 {
		c.logger.Infof("[%s] waiting %.1fs before starting", c.name, c.cfg.StartupWaitS)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(c.cfg.StartupWaitS * float64(time.Second))):
		}
	}

	// Set initial shaper rates
	c.core.Start(time.Now())

	// Start monitor
	go c.monitor.Run(ctx, c.rateCh)

	// Start pingers
	c.StartPingers()

	// Health check ticker
	healthTicker := time.NewTicker(time.Duration(c.cfg.ReflectorResponseDeadlineS * float64(time.Second)))
	defer healthTicker.Stop()

	c.logger.Infof("[%s] controller started in %s state", c.name, c.core.Core().State())
	c.logger.Infof("[%s] dl: %s %d/%d/%d kbps, ul: %s %d/%d/%d kbps",
		c.name,
		c.link.Download.Interface, c.link.Download.MinRateKbps, c.link.Download.BaseRateKbps, c.link.Download.MaxRateKbps,
		c.link.Upload.Interface, c.link.Upload.MinRateKbps, c.link.Upload.BaseRateKbps, c.link.Upload.MaxRateKbps)

	for {
		select {
		case <-ctx.Done():
			c.StopPingers()
			c.logger.Infof("[%s] controller stopped", c.name)
			return ctx.Err()

		case stats := <-c.rateCh:
			c.core.HandleRate(stats)

		case result := <-c.pingCh:
			c.core.HandlePing(result)

		case <-healthTicker.C:
			if promoted := c.pingerMgr.ReplaceUnhealthy(); len(promoted) > 0 {
				now := time.Now()
				for _, r := range promoted {
					c.core.ResetReflector(r, now)
				}
				// Restart pingers to pick up the new active set
				c.StopPingers()
				c.StartPingers()
			}
		}
	}
}

// StartPingers implements Actuator.
func (c *LinkController) StartPingers() {
	pingerCtx, cancel := context.WithCancel(c.runCtx)
	c.pingerCancel = cancel
	go c.pingerMgr.Run(pingerCtx, c.pingCh)
}

// StopPingers implements Actuator.
func (c *LinkController) StopPingers() {
	if c.pingerCancel != nil {
		c.pingerCancel()
		c.pingerCancel = nil
	}
}

// SetRate implements Actuator.
func (c *LinkController) SetRate(dir Direction, rateKbps int) {
	if err := c.shaper.SetRate(c.link.DirConfig(dir).Interface, rateKbps); err != nil {
		c.logger.Errorf("[%s] [%s] setting rate to %d kbps: %v", c.name, dir, rateKbps, err)
	}
}

// InvalidateShaper implements Actuator.
func (c *LinkController) InvalidateShaper() {
	c.shaper.InvalidateCache(c.link.Download.Interface)
	c.shaper.InvalidateCache(c.link.Upload.Interface)
}

// readInterfaceMTU reads the MTU for a network interface from sysfs.
// Falls back to defaultMTU (1500) if the read fails.
func readInterfaceMTU(iface string, logger *Logger) int {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/mtu", iface))
	if err != nil {
		logger.Infof("reading MTU for %s: %v, using default %d", iface, err, defaultMTU)
		return defaultMTU
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || val <= 0 {
		logger.Infof("parsing MTU for %s: %v, using default %d", iface, err, defaultMTU)
		return defaultMTU
	}
	return val
}
