package main

import "testing"

// --- readInterfaceMTU tests ---

func TestReadInterfaceMTU_LoopbackInterface(t *testing.T) {
	logger := testLogger(t)
	mtu := readInterfaceMTU("lo", logger)
	// lo MTU is typically 65536 on Linux
	if mtu <= 0 {
		t.Errorf("expected positive MTU for lo, got %d", mtu)
	}
}

func TestReadInterfaceMTU_NonexistentInterface(t *testing.T) {
	logger := testLogger(t)
	mtu := readInterfaceMTU("nonexistent_iface_xyz", logger)
	if mtu != defaultMTU {
		t.Errorf("expected fallback to %d for nonexistent iface, got %d", defaultMTU, mtu)
	}
}
