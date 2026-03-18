package main

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
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

func TestShaper_InvalidateCache(t *testing.T) {
	logger := testLogger(t)
	s := &Shaper{
		logger:    logger,
		fd:        -1,
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
		msgBuf:    make([]byte, 64),
		recvBuf:   make([]byte, 4096),
	}

	// Populate caches
	s.lastRates["eth0"] = 20000
	s.ifIndices["eth0"] = 42

	// Invalidate
	s.InvalidateCache("eth0")

	if _, ok := s.lastRates["eth0"]; ok {
		t.Error("expected lastRates to be cleared after InvalidateCache")
	}
	if _, ok := s.ifIndices["eth0"]; ok {
		t.Error("expected ifIndices to be cleared after InvalidateCache")
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

// --- Netlink message builder tests ---

func TestBuildCakeRateMsg_Size(t *testing.T) {
	buf := make([]byte, cakeRateMsgSize)
	buildCakeRateMsg(buf, 1, 42, 20000)

	msgLen := binary.NativeEndian.Uint32(buf[offNlmsgLen:])
	if msgLen != cakeRateMsgSize {
		t.Errorf("nlmsg_len = %d, want %d", msgLen, cakeRateMsgSize)
	}
}

func TestBuildCakeRateMsg_Header(t *testing.T) {
	buf := make([]byte, cakeRateMsgSize)
	buildCakeRateMsg(buf, 7, 3, 10000)

	msgType := binary.NativeEndian.Uint16(buf[offNlmsgType:])
	if msgType != unix.RTM_NEWQDISC {
		t.Errorf("nlmsg_type = %d, want RTM_NEWQDISC(%d)", msgType, unix.RTM_NEWQDISC)
	}

	flags := binary.NativeEndian.Uint16(buf[offNlmsgFlags:])
	wantFlags := uint16(unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_REPLACE)
	if flags != wantFlags {
		t.Errorf("nlmsg_flags = 0x%x, want 0x%x", flags, wantFlags)
	}

	seq := binary.NativeEndian.Uint32(buf[offNlmsgSeq:])
	if seq != 7 {
		t.Errorf("nlmsg_seq = %d, want 7", seq)
	}

	pid := binary.NativeEndian.Uint32(buf[offNlmsgPid:])
	if pid != 0 {
		t.Errorf("nlmsg_pid = %d, want 0", pid)
	}
}

func TestBuildCakeRateMsg_Tcmsg(t *testing.T) {
	buf := make([]byte, cakeRateMsgSize)
	buildCakeRateMsg(buf, 1, 42, 10000)

	if buf[offTcmFamily] != 0 {
		t.Errorf("tcm_family = %d, want 0", buf[offTcmFamily])
	}

	ifindex := binary.NativeEndian.Uint32(buf[offTcmIfindex:])
	if ifindex != 42 {
		t.Errorf("tcm_ifindex = %d, want 42", ifindex)
	}

	parent := binary.NativeEndian.Uint32(buf[offTcmParent:])
	if parent != tcHRoot {
		t.Errorf("tcm_parent = 0x%x, want TC_H_ROOT(0x%x)", parent, tcHRoot)
	}
}

func TestBuildCakeRateMsg_KindIsCake(t *testing.T) {
	buf := make([]byte, cakeRateMsgSize)
	buildCakeRateMsg(buf, 1, 1, 10000)

	kindLen := binary.NativeEndian.Uint16(buf[offKindLen:])
	if kindLen != 9 {
		t.Errorf("TCA_KIND nla_len = %d, want 9", kindLen)
	}

	kindType := binary.NativeEndian.Uint16(buf[offKindType:])
	if kindType != tcaKind {
		t.Errorf("TCA_KIND nla_type = %d, want %d", kindType, tcaKind)
	}

	kind := string(buf[offKindVal : offKindVal+4])
	if kind != "cake" {
		t.Errorf("kind string = %q, want %q", kind, "cake")
	}
	if buf[offKindVal+4] != 0 {
		t.Error("kind string missing null terminator")
	}
}

func TestBuildCakeRateMsg_RateEncoding(t *testing.T) {
	tests := []struct {
		rateKbps    int
		wantBytesPS uint64
	}{
		{10000, 1250000},  // 10 Mbit/s = 1,250,000 bytes/s
		{100000, 12500000}, // 100 Mbit/s
		{1000, 125000},     // 1 Mbit/s
	}

	for _, tc := range tests {
		buf := make([]byte, cakeRateMsgSize)
		buildCakeRateMsg(buf, 1, 1, tc.rateKbps)

		got := binary.NativeEndian.Uint64(buf[offRateVal:])
		if got != tc.wantBytesPS {
			t.Errorf("rate for %d kbps: got %d bytes/s, want %d", tc.rateKbps, got, tc.wantBytesPS)
		}
	}
}

func TestBuildCakeRateMsg_NestedOptions(t *testing.T) {
	buf := make([]byte, cakeRateMsgSize)
	buildCakeRateMsg(buf, 1, 1, 10000)

	optsLen := binary.NativeEndian.Uint16(buf[offOptsLen:])
	if optsLen != 16 {
		t.Errorf("TCA_OPTIONS nla_len = %d, want 16", optsLen)
	}

	optsType := binary.NativeEndian.Uint16(buf[offOptsType:])
	wantType := uint16(tcaOptions | nlaFNested)
	if optsType != wantType {
		t.Errorf("TCA_OPTIONS nla_type = 0x%x, want 0x%x", optsType, wantType)
	}

	rateLen := binary.NativeEndian.Uint16(buf[offRateLen:])
	if rateLen != 12 {
		t.Errorf("TCA_CAKE_BASE_RATE64 nla_len = %d, want 12", rateLen)
	}

	rateType := binary.NativeEndian.Uint16(buf[offRateType:])
	if rateType != tcaCakeBaseRate64 {
		t.Errorf("TCA_CAKE_BASE_RATE64 nla_type = %d, want %d", rateType, tcaCakeBaseRate64)
	}
}
