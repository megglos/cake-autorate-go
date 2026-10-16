package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decodeHistory parses the JSON of HistoryJSON.
func decodeHistory(t *testing.T, data []byte) (h struct {
	Schema     int       `json:"schema"`
	Updated    time.Time `json:"updated"`
	IntervalMs int64     `json:"interval_ms"`
	Fields     []string  `json:"fields"`
	Links      []struct {
		Name    string      `json:"name"`
		Samples [][]float64 `json:"samples"`
	} `json:"links"`
}) {
	t.Helper()
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatalf("history not valid: %v %q", err, data)
	}
	return h
}

func TestHistoryRecord(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	b := NewStatusBoard("v1", start, []string{"a", "b"})
	b.KeepHistory()
	b.Set(LinkStatus{Name: "a",
		Download: DirStatus{ShaperKbps: 20000.4, AchievedKbps: 1234.6, DeltaMs: 3.14, ThresholdMs: 15},
		Upload:   DirStatus{ShaperKbps: 12000, AchievedKbps: 99.5, DeltaMs: 0.05, ThresholdMs: 10}})
	b.Record(start)
	data, err := b.HistoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	h := decodeHistory(t, data)
	if h.Schema != historySchema || h.IntervalMs != 1000 || !h.Updated.Equal(start) || len(h.Fields) != 9 {
		t.Errorf("header: %+v", h)
	}
	// Link b has no status: no history.
	if len(h.Links) != 1 || h.Links[0].Name != "a" || len(h.Links[0].Samples) != 1 {
		t.Fatalf("links: %+v", h.Links)
	}
	want := []float64{1_700_000_000_000, 20000, 1235, 3.1, 15, 12000, 100, 0.1, 10}
	if got := h.Links[0].Samples[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sample: got %v, want %v", got, want)
	}

	// The links keep the config order.
	b.Set(LinkStatus{Name: "b"})
	b.Record(start.Add(time.Second))
	data, _ = b.HistoryJSON()
	if h := decodeHistory(t, data); len(h.Links) != 2 || h.Links[0].Name != "a" || len(h.Links[0].Samples) != 2 {
		t.Errorf("links: %+v", h.Links)
	}
}

func TestHistoryKeepsOnlyTheLastSeconds(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	b := NewStatusBoard("v1", start, []string{"a"})
	b.KeepHistory()
	b.Set(LinkStatus{Name: "a"})
	for i := range historyLen + 50 {
		b.Record(start.Add(time.Duration(i) * statusInterval))
	}
	data, _ := b.HistoryJSON()
	s := decodeHistory(t, data).Links[0].Samples
	if len(s) != historyLen {
		t.Fatalf("got %d samples, want %d", len(s), historyLen)
	}
	last := start.Add(time.Duration(historyLen+49) * statusInterval).UnixMilli()
	if int64(s[len(s)-1][0]) != last || int64(s[0][0]) != last-int64(historyLen-1)*1000 {
		t.Errorf("times: first %v, last %v", s[0][0], s[len(s)-1][0])
	}

	// After a stop of the writer (a long time without a sample), the old
	// samples go away.
	b.Record(start.Add(time.Duration(historyLen+49)*statusInterval + (historySeconds+1)*time.Second))
	data, _ = b.HistoryJSON()
	if s := decodeHistory(t, data).Links[0].Samples; len(s) != 1 {
		t.Errorf("got %d samples after a long stop, want 1", len(s))
	}
}

func TestHistoryClockStepBack(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	b := NewStatusBoard("v1", start, []string{"a"})
	b.KeepHistory()
	b.Set(LinkStatus{Name: "a"})
	for i := range 5 {
		b.Record(start.Add(time.Duration(i) * time.Second))
	}
	// The clock goes back 2.5 s: the samples at 2, 3 and 4 s go away.
	b.Record(start.Add(1500 * time.Millisecond))
	data, _ := b.HistoryJSON()
	var times []int64
	for _, s := range decodeHistory(t, data).Links[0].Samples {
		times = append(times, int64(s[0])-start.UnixMilli())
	}
	if fmt.Sprint(times) != "[0 1000 1500]" {
		t.Errorf("times: %v", times)
	}
}

func TestHistoryRemovesLinkWithoutStatus(t *testing.T) {
	start := time.Now()
	b := NewStatusBoard("v1", start, []string{"a"})
	b.KeepHistory()
	b.Set(LinkStatus{Name: "a"})
	b.Record(start)
	b.mu.Lock()
	delete(b.links, "a")
	b.mu.Unlock()
	b.Record(start.Add(time.Second))
	data, _ := b.HistoryJSON()
	if h := decodeHistory(t, data); len(h.Links) != 0 {
		t.Errorf("links: %+v", h.Links)
	}
}

func TestHistoryWithoutBoard(t *testing.T) {
	var b *StatusBoard
	if _, err := b.HistoryJSON(); err == nil {
		t.Error("a nil board must give an error")
	}
	if historySource(nil) != nil {
		t.Error("historySource(nil) must be a nil interface")
	}
	if got := handleControlCommand("history", &ControlHandler{History: historySource(nil)}); got != "error: no history" {
		t.Errorf("answer: %q", got)
	}
}

type fakeHistory struct {
	data []byte
	err  error
}

func (f fakeHistory) HistoryJSON() ([]byte, error) { return f.data, f.err }

func TestHandleHistoryCommand(t *testing.T) {
	src := fakeHistory{data: []byte(`{"links":[]}`)}
	if got := handleControlCommand("history\n", &ControlHandler{History: src}); got != `{"links":[]}` {
		t.Errorf("answer: %q", got)
	}
	if got := handleControlCommand("history a", &ControlHandler{History: src}); got != "error: usage: history" {
		t.Errorf("answer with an argument: %q", got)
	}
	src.err = errors.New("broken\nboard")
	if got := handleControlCommand("history", &ControlHandler{History: src}); got != "error: broken board" {
		t.Errorf("answer with an error: %q", got)
	}
}

// A long history goes through the socket as one complete line.
func TestControlSocketHistory(t *testing.T) {
	start := time.Now()
	b := NewStatusBoard("v1", start, []string{"a"})
	b.KeepHistory()
	b.Set(LinkStatus{Name: "a", Download: DirStatus{ShaperKbps: 20000}})
	for i := range historyLen {
		b.Record(start.Add(time.Duration(i) * statusInterval))
	}
	path := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunControlSocket(ctx, path, &ControlHandler{Names: []string{"a"}, History: historySource(b)}, NewDiscardLogger())
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var conn net.Conn
	var err error
	for range 50 {
		if conn, err = net.Dial("unix", path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintln(conn, "history")
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if h := decodeHistory(t, []byte(strings.TrimSpace(answer))); len(h.Links) != 1 || len(h.Links[0].Samples) != historyLen {
		t.Errorf("got %d bytes, links %d", len(answer), len(h.Links))
	}
}

// Without status_history, the board keeps no samples.
func TestHistoryOff(t *testing.T) {
	start := time.Now()
	b := NewStatusBoard("v1", start, []string{"a"})
	b.Set(LinkStatus{Name: "a"})
	b.Record(start)
	if len(b.history) != 0 {
		t.Errorf("history without status_history: %v", b.history)
	}
	if _, err := b.HistoryJSON(); err == nil || !strings.Contains(err.Error(), "status_history") {
		t.Errorf("error: %v", err)
	}
	b.KeepHistory()
	b.Record(start.Add(time.Second))
	if data, err := b.HistoryJSON(); err != nil || len(decodeHistory(t, data).Links) != 1 {
		t.Errorf("history after KeepHistory: %v %s", err, data)
	}
}

func TestStatusHistoryNeedsStatusFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StatusHistory = true
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "status_history needs status_file") {
		t.Errorf("warnings: %q", w)
	}
	cfg.StatusFile = "/run/s.json"
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "status_history") {
		t.Errorf("warnings with a status file: %q", w)
	}
}
