package main

import (
	"testing"
)

func TestWirePacketCompensationUs_Normal(t *testing.T) {
	// 1500 bytes * 8 = 12000 bits. At 10000 kbps: 1000 * 12000 / 10000 = 1200 us
	got := WirePacketCompensationUs(1500, 10000)
	want := 1200.0
	if got != want {
		t.Errorf("WirePacketCompensationUs(1500, 10000) = %f, want %f", got, want)
	}
}

func TestWirePacketCompensationUs_ZeroRate(t *testing.T) {
	got := WirePacketCompensationUs(1500, 0)
	if got != 0 {
		t.Errorf("WirePacketCompensationUs with zero rate should be 0, got %f", got)
	}
}

func TestWirePacketCompensationUs_NegativeRate(t *testing.T) {
	got := WirePacketCompensationUs(1500, -1)
	if got != 0 {
		t.Errorf("WirePacketCompensationUs with negative rate should be 0, got %f", got)
	}
}

func TestWirePacketCompensationUs_SmallPacket(t *testing.T) {
	// 64 bytes (minimum Ethernet frame) at 1000 kbps
	// 64 * 8 = 512 bits. 1000 * 512 / 1000 = 512 us
	got := WirePacketCompensationUs(64, 1000)
	want := 512.0
	if got != want {
		t.Errorf("WirePacketCompensationUs(64, 1000) = %f, want %f", got, want)
	}
}

func TestShaper_SkipsDuplicateRates(t *testing.T) {
	logger := testLogger(t)
	s := &Shaper{
		logger:    logger,
		fd:        -1, // no netlink
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
		msgBuf:    make([]byte, 64),
		recvBuf:   make([]byte, 4096),
	}

	// Pre-set a cached rate
	s.lastRates["eth0"] = 20000

	// Setting same rate should be a no-op (returns nil without calling tc)
	err := s.SetRate("eth0", 20000)
	if err != nil {
		t.Errorf("setting duplicate rate should be no-op, got: %v", err)
	}
}

func TestShaper_RejectsNonPositiveRate(t *testing.T) {
	logger := testLogger(t)
	s := &Shaper{
		logger:    logger,
		fd:        -1,
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
		msgBuf:    make([]byte, 64),
		recvBuf:   make([]byte, 4096),
	}

	if err := s.SetRate("eth0", 0); err == nil {
		t.Error("expected error for zero rate")
	}
	if err := s.SetRate("eth0", -1); err == nil {
		t.Error("expected error for negative rate")
	}
}

func TestShaper_FallbackToTc(t *testing.T) {
	// Shaper with fd=-1 should use tc fallback path.
	// We can't easily test tc without root, but we can verify it doesn't panic.
	logger := testLogger(t)
	s := &Shaper{
		logger:    logger,
		fd:        -1,
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
		msgBuf:    make([]byte, 64),
		recvBuf:   make([]byte, 4096),
	}

	// This will fail (no tc/no interface) but should not panic
	err := s.SetRate("nonexistent-iface", 10000)
	if err == nil {
		t.Error("expected error when tc fails on nonexistent interface")
	}
}
