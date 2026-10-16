// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"encoding/json"
	"errors"
	"math"
	"time"
)

// historySchema is the version of the format of the history. Increase it
// when a change is not backward compatible.
const historySchema = 1

// historySeconds is the time that the history keeps. The status writer
// adds one sample for each link each statusInterval.
const historySeconds = 900

// historyLen is the maximum number of samples for each link.
const historyLen = int(historySeconds * time.Second / statusInterval)

// historyFields are the names of the values of a sample, in their order.
// The rates are in kbit/s, the delays in ms.
var historyFields = []string{
	"t_ms",
	"dl_shaper_kbps", "dl_achieved_kbps", "dl_delta_ms", "dl_threshold_ms",
	"ul_shaper_kbps", "ul_achieved_kbps", "ul_delta_ms", "ul_threshold_ms",
}

// historySample is one sample of a link: the time and the values of
// historyFields after "t_ms".
type historySample struct {
	t int64
	v [8]float32
}

// MarshalJSON writes a sample as an array: [t_ms, value, ...]. A rate has
// no decimals, a delay has one.
func (s historySample) MarshalJSON() ([]byte, error) {
	out := make([]any, 0, 1+len(s.v))
	out = append(out, s.t)
	for i, v := range s.v {
		if i%4 < 2 {
			out = append(out, math.Round(float64(v)))
		} else {
			out = append(out, math.Round(float64(v)*10)/10)
		}
	}
	return json.Marshal(out)
}

func newHistorySample(now time.Time, st LinkStatus) historySample {
	s := historySample{t: now.UnixMilli()}
	for i, d := range []DirStatus{st.Download, st.Upload} {
		s.v[i*4] = float32(d.ShaperKbps)
		s.v[i*4+1] = float32(d.AchievedKbps)
		s.v[i*4+2] = float32(d.DeltaMs)
		s.v[i*4+3] = float32(d.ThresholdMs)
	}
	return s
}

// LinkHistory is the history of one link, the oldest sample first.
type LinkHistory struct {
	Name    string          `json:"name"`
	Samples []historySample `json:"samples"`
}

// History is the answer of the control command "history".
type History struct {
	Schema     int           `json:"schema"`
	Updated    time.Time     `json:"updated"`
	IntervalMs int64         `json:"interval_ms"`
	Fields     []string      `json:"fields"`
	Links      []LinkHistory `json:"links"`
}

// HistorySource gives the history of the links as JSON.
type HistorySource interface {
	HistoryJSON() ([]byte, error)
}

// historySource returns the board as a HistorySource, or nil if there is no
// board. A nil *StatusBoard in the interface is not a nil interface.
func historySource(b *StatusBoard) HistorySource {
	if b == nil {
		return nil
	}
	return b
}

// KeepHistory turns the history on. Without it, Record does nothing and
// HistoryJSON gives an error.
func (b *StatusBoard) KeepHistory() {
	b.mu.Lock()
	b.keepHistory = true
	b.mu.Unlock()
}

// Record adds a sample for each link with a status. It removes the
// samples that are older than historySeconds, and the links that have no
// status. A clock step back (for example, NTP after the start) removes the
// samples after the new time, so that the times always increase.
func (b *StatusBoard) Record(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.keepHistory {
		return
	}
	b.recorded = now
	if b.history == nil {
		b.history = make(map[string][]historySample)
	}
	oldest := now.Add(-historySeconds * time.Second).UnixMilli()
	for _, name := range b.order {
		st, ok := b.links[name]
		if !ok {
			delete(b.history, name)
			continue
		}
		h := b.history[name]
		s := newHistorySample(now, st)
		// Keep only the samples before the new one and in the time.
		end := len(h)
		for end > 0 && h[end-1].t >= s.t {
			end--
		}
		start := 0
		for start < end && h[start].t < oldest {
			start++
		}
		if end-start >= historyLen {
			start = end - historyLen + 1
		}
		h = append(h[:0], h[start:end]...)
		b.history[name] = append(h, s)
	}
}

// HistoryJSON returns the history of the links in the order of the
// config. A nil board (no status file) has no history.
func (b *StatusBoard) HistoryJSON() ([]byte, error) {
	if b == nil {
		return nil, errors.New("no history: the status file is off")
	}
	if !b.keepHistory {
		return nil, errors.New("no history: status_history is off")
	}
	b.mu.Lock()
	h := History{
		Schema:     historySchema,
		Updated:    b.recorded,
		IntervalMs: statusInterval.Milliseconds(),
		Fields:     historyFields,
		Links:      []LinkHistory{},
	}
	for _, name := range b.order {
		if s, ok := b.history[name]; ok {
			h.Links = append(h.Links, LinkHistory{Name: name, Samples: append([]historySample(nil), s...)})
		}
	}
	b.mu.Unlock()
	return json.Marshal(h)
}
