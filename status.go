// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// statusSchema is the version of the format of the status file. Increase it
// when a change is not backward compatible.
const statusSchema = 1

// statusInterval is the time between two writes of the status file.
const statusInterval = time.Second

// DirStatus is the status of one direction of a link.
type DirStatus struct {
	Interface string `json:"interface"`
	Adjust    bool   `json:"adjust"`
	// Paused is true while a user paused the rate control of the
	// direction through the control socket.
	Paused       bool    `json:"paused"`
	ShaperKbps   float64 `json:"shaper_kbps"`
	AchievedKbps float64 `json:"achieved_kbps"`
	LoadPercent  float64 `json:"load_percent"`
	MinKbps      int     `json:"min_kbps"`
	BaseKbps     int     `json:"base_kbps"`
	MaxKbps      int     `json:"max_kbps"`
	// DeltaMs is the delay delta of the last ping reply.
	DeltaMs float64 `json:"delta_ms"`
	// ThresholdMs is the delay threshold of the direction.
	ThresholdMs float64 `json:"threshold_ms"`
	// HighDelay is true if the delta of the last ping reply was above the
	// threshold. The controller adds the time to send one packet at the
	// shaper rate to the threshold. Thus, a delta a little above
	// ThresholdMs at a low rate is not a high delay.
	HighDelay bool `json:"high_delay"`
	// BufferbloatCount is the number of pings with a high delay in the
	// detection window.
	BufferbloatCount int `json:"bufferbloat_count"`
	// CrossDelayMs is the learned delay from the other direction. It is 0
	// if learn_cross_delay is off.
	CrossDelayMs float64 `json:"cross_delay_ms"`
	// RateChanges is the number of rate changes since the start.
	RateChanges int `json:"rate_changes"`
}

// ReflectorDelayStatus is the status of a reflector of a link.
type ReflectorDelayStatus struct {
	ReflectorStatus
	// BaselineMs is the baseline RTT. It is 0 if there is no reply yet.
	BaselineMs float64 `json:"baseline_ms"`
}

// LinkStatus is the status of one link.
type LinkStatus struct {
	Name       string                 `json:"name"`
	State      string                 `json:"state"`
	Download   DirStatus              `json:"download"`
	Upload     DirStatus              `json:"upload"`
	Reflectors []ReflectorDelayStatus `json:"reflectors"`
}

// StatusFile is the content of the status file.
type StatusFile struct {
	Schema  int          `json:"schema"`
	Version string       `json:"version"`
	Started time.Time    `json:"started"`
	Updated time.Time    `json:"updated"`
	Links   []LinkStatus `json:"links"`
}

// Snapshot returns the status of a link. Call it from the goroutine that
// uses the Core. It does not change the Core.
func (c *Core) Snapshot() LinkStatus {
	st := LinkStatus{Name: c.name, State: c.state.String()}
	for _, dir := range Directions() {
		ds, dc := c.getDirState(dir), c.link.DirConfig(dir)
		out := DirStatus{
			Interface:        dc.Interface,
			Adjust:           dc.Adjust,
			Paused:           ds.paused,
			ShaperKbps:       ds.shaperRateKbps,
			AchievedKbps:     ds.achievedKbps,
			LoadPercent:      ds.loadPercent * 100,
			MinKbps:          dc.MinRateKbps,
			BaseKbps:         dc.BaseRateKbps,
			MaxKbps:          dc.MaxRateKbps,
			DeltaMs:          roundMs(ds.lastDeltaUs),
			ThresholdMs:      dc.OWDDeltaDelayThrMs,
			HighDelay:        ds.lastHighDelay,
			BufferbloatCount: ds.delayWindowCount,
			CrossDelayMs:     roundMs(ds.crossDelayUs),
			RateChanges:      ds.rateChanges,
		}
		if dir == DL {
			st.Download = out
		} else {
			st.Upload = out
		}
	}
	return st
}

// roundMs converts microseconds to milliseconds with 0.1 ms resolution.
func roundMs(us float64) float64 { return math.Round(us/100) / 10 }

// StatusBoard collects the status of the links. The link controllers write
// to it. The status writer reads it.
type StatusBoard struct {
	mu      sync.Mutex
	version string
	started time.Time
	order   []string
	links   map[string]LinkStatus
	// history has the samples of each link, the oldest first. recorded
	// is the time of the last Record.
	history  map[string][]historySample
	recorded time.Time
}

// NewStatusBoard creates a board for the links in the given order.
func NewStatusBoard(version string, started time.Time, links []string) *StatusBoard {
	return &StatusBoard{version: version, started: started, order: links, links: make(map[string]LinkStatus)}
}

// Set stores the status of a link.
func (b *StatusBoard) Set(st LinkStatus) {
	b.mu.Lock()
	b.links[st.Name] = st
	b.mu.Unlock()
}

// File returns the content of the status file. It leaves out the links
// that have no status yet.
func (b *StatusBoard) File(now time.Time) StatusFile {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := StatusFile{Schema: statusSchema, Version: b.version, Started: b.started, Updated: now, Links: []LinkStatus{}}
	for _, name := range b.order {
		if st, ok := b.links[name]; ok {
			f.Links = append(f.Links, st)
		}
	}
	return f
}

// writeStatusFile writes the status to path. It writes a temporary file and
// renames it. Thus, a reader never sees a partial file.
func writeStatusFile(path string, f StatusFile) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Create the temporary file with a random name, so that nobody can
	// place a link at a known name.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	// Set the mode on the open file, not on the path. Another user could
	// replace the path by a link.
	err = tmp.Chmod(0o644)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// RunStatusWriter writes the status file each statusInterval until ctx ends.
// Then it removes the file. Thus, a reader sees that the program stopped.
func RunStatusWriter(ctx context.Context, b *StatusBoard, path string, logger *Logger) {
	write := func(now time.Time) {
		if err := writeStatusFile(path, b.File(now)); err != nil {
			logger.Errorf("writing status file: %v", err)
		}
	}
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			os.Remove(path)
			return
		case now := <-ticker.C:
			write(now)
			b.Record(now)
		}
	}
}
