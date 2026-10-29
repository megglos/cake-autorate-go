// SPDX-License-Identifier: GPL-2.0

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestTsDiffMs(t *testing.T) {
	for _, tc := range []struct {
		a, b uint32
		want int64
	}{
		{1000, 1025, 25},
		{1025, 1000, -25},
		{msPerDay - 10, 15, 25},  // across midnight
		{15, msPerDay - 10, -25}, // clock offset across midnight
	} {
		if got := tsDiffMs(tc.a, tc.b); got != tc.want {
			t.Errorf("tsDiffMs(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTimestampRequest_Format(t *testing.T) {
	b := marshalTimestampRequest(0x1234, 7, 5000)
	if len(b) != icmpTimestampLen || b[0] != icmpTypeTimestamp {
		t.Fatalf("bad request: % x", b)
	}
	// The checksum of a message with a correct checksum is 0.
	if c := icmpChecksum(b); c != 0 {
		t.Errorf("checksum check gives %#x, want 0", c)
	}
}

func TestTimestampReply_OneWayDelays(t *testing.T) {
	// The reflector clock is 100 ms ahead. Upload delay 30 ms, download
	// delay 10 ms.
	b := marshalTimestampRequest(1, 2, 5000)
	b[0] = icmpTypeTimestampReply
	put := func(off int, v uint32) {
		b[off], b[off+1], b[off+2], b[off+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	put(12, 5000+30+100) // receive
	put(16, 5000+31+100) // transmit (1 ms in the reflector)
	r, ok := parseTimestampReply(b)
	if !ok || r.id != 1 || r.seq != 2 {
		t.Fatalf("parse: %+v %v", r, ok)
	}
	ul, dl, in, ok := r.oneWayDelays(5000 + 41)
	if !ok {
		t.Fatal("standard times must give delays")
	}
	if in != time.Millisecond {
		t.Errorf("time in the reflector %v, want 1ms", in)
	}
	// The offset is in the two values, with a different sign. The sum is
	// the RTT without the time in the reflector.
	if ul != 130*time.Millisecond || dl != -90*time.Millisecond {
		t.Errorf("ul %v dl %v, want 130ms and -90ms", ul, dl)
	}

	put(16, (5000+31)|tsNonStandard)
	r, _ = parseTimestampReply(b)
	if _, _, _, ok := r.oneWayDelays(5041); ok {
		t.Error("non-standard times must not give delays")
	}
}

func TestParseTimestampReply_RejectsOtherMessages(t *testing.T) {
	if _, ok := parseTimestampReply(marshalTimestampRequest(1, 1, 1)); ok {
		t.Error("a request is not a reply")
	}
	if _, ok := parseTimestampReply([]byte{icmpTypeTimestampReply, 0, 0}); ok {
		t.Error("a short message is not a reply")
	}
}

// The Linux kernel replies to ICMP timestamp requests. This test needs
// CAP_NET_RAW. Without it, the test is skipped. To run it without root:
//
//	unshare -rn sh -c 'ip link set lo up && go test -run TestTimestampPinger_Loopback -v'
func TestTimestampPinger_Loopback(t *testing.T) {
	conn, err := listenICMP(context.Background(), "", "")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
			t.Skip("CAP_NET_RAW is necessary")
		}
		t.Fatal(err)
	}
	conn.Close()

	cfg := testConfig()
	cfg.PingMethod = PingMethodTimestamp
	cfg.Links[0].Reflectors = []string{"127.0.0.1"}
	pm := NewPingerManager(&cfg.Links[0], cfg, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make(chan PingResult, 16)
	go pm.Run(ctx, results)

	var got []PingResult
	for len(got) < 3 {
		select {
		case r := <-results:
			got = append(got, r)
		case <-ctx.Done():
			t.Fatalf("got %d results in 2 s, want 3", len(got))
		}
	}
	for _, r := range got {
		if r.Timeout || !r.OWD {
			t.Fatalf("want replies with one-way delays, got %+v", r)
		}
		// Same clock: each one-way delay is 0 or 1 ms.
		if r.UlOWD < 0 || r.UlOWD > 2*time.Millisecond || r.DlOWD < 0 || r.DlOWD > 2*time.Millisecond {
			t.Errorf("one-way delays on loopback: ul %v dl %v", r.UlOWD, r.DlOWD)
		}
	}
}

func TestLoadConfig_PingMethodTimestamp(t *testing.T) {
	path := t.TempDir() + "/c.yaml"
	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := `
links:
  - name: wan
    download: {interface: ifb-wan, min_rate_kbps: 1000, base_rate_kbps: 2000, max_rate_kbps: 3000}
    upload: {interface: wan, min_rate_kbps: 1000, base_rate_kbps: 2000, max_rate_kbps: 3000}
`
	// Without reflectors, the timestamp method uses its own default list.
	write("ping_method: icmp-timestamp\n" + link)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Links[0].Reflectors, defaultTimestampReflectors) {
		t.Errorf("reflectors %v, want the timestamp defaults", cfg.Links[0].Reflectors)
	}

	// Reflectors in the config stay.
	write("ping_method: icmp-timestamp\nreflectors: [192.0.2.1]\n" + link)
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Links[0].Reflectors, []string{"192.0.2.1"}) {
		t.Errorf("reflectors %v, want the configured list", cfg.Links[0].Reflectors)
	}

	// The default method is ICMP echo.
	write(link)
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if cfg.PingMethod != PingMethodEcho {
		t.Errorf("default ping_method %q, want %q", cfg.PingMethod, PingMethodEcho)
	}

	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("echo with the default reflectors gives warnings: %v", w)
	}

	// The echo reflectors with the timestamp method give a warning.
	write("ping_method: icmp-timestamp\nreflectors: [1.1.1.1, 94.140.14.14]\n" + link)
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 1 {
		t.Errorf("want 1 warning, got %v", w)
	}

	// Only echo reflectors with the timestamp method give an error. Before,
	// the program replaced a top-level list equal to the echo defaults.
	write("ping_method: icmp-timestamp\nreflectors: [1.1.1.1, 8.8.8.8, 9.9.9.9, 149.112.112.112, 1.0.0.1, 8.8.4.4, 208.67.222.222, 208.67.220.220]\n" + link)
	if _, err := LoadConfig(path); err == nil {
		t.Error("only echo reflectors with icmp-timestamp must give an error")
	}

	// A link can set its own reflectors. A link without reflectors gets the
	// default list for the ping method.
	write("ping_method: icmp-timestamp\n" + link + "    reflectors: [192.0.2.1]\n" + `
  - name: wan2
    download: {interface: ifb-wan2, min_rate_kbps: 1000, base_rate_kbps: 2000, max_rate_kbps: 3000}
    upload: {interface: wan2, min_rate_kbps: 1000, base_rate_kbps: 2000, max_rate_kbps: 3000}
`)
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Links[0].Reflectors, []string{"192.0.2.1"}) {
		t.Errorf("link reflectors %v, want the configured list", cfg.Links[0].Reflectors)
	}
	if !slices.Equal(cfg.Links[1].Reflectors, defaultTimestampReflectors) {
		t.Errorf("link without reflectors: %v, want the timestamp defaults", cfg.Links[1].Reflectors)
	}

	write("ping_method: udp\n" + link)
	if _, err := LoadConfig(path); err == nil {
		t.Error("an unknown ping_method must give an error")
	}
}

func TestEchoRequest_Format(t *testing.T) {
	b := marshalEchoRequest(0x1234, 7)
	if len(b) != icmpEchoLen || b[0] != icmpTypeEchoRequest {
		t.Fatalf("bad request: % x", b)
	}
	if c := icmpChecksum(b); c != 0 {
		t.Errorf("checksum check gives %#x, want 0", c)
	}
	if _, _, ok := parseEchoReply(b); ok {
		t.Error("a request is not a reply")
	}
	b[0] = icmpTypeEchoReply
	if id, seq, ok := parseEchoReply(b); !ok || id != 0x1234 || seq != 7 {
		t.Errorf("parse: id %#x seq %d ok %v", id, seq, ok)
	}
	if _, _, ok := parseEchoReply(b[:4]); ok {
		t.Error("a short message is not a reply")
	}

	b6 := marshalEchoRequest6(0x1234, 7)
	if b6[0] != icmpv6TypeEchoRequest {
		t.Fatalf("bad ICMPv6 request: % x", b6)
	}
	if _, _, ok := parseEchoReply6(b6); ok {
		t.Error("an ICMPv6 request is not a reply")
	}
	b6[0] = icmpv6TypeEchoReply
	if id, seq, ok := parseEchoReply6(b6); !ok || id != 0x1234 || seq != 7 {
		t.Errorf("parse ICMPv6: id %#x seq %d ok %v", id, seq, ok)
	}
}

// Echo pings to an IPv6 reflector (pro-bing supported them). It needs
// CAP_NET_RAW and IPv6 on lo.
func TestEchoPinger_IPv6Loopback(t *testing.T) {
	needRawSocket(t)
	if c, err := listenRaw(context.Background(), "ip6:ipv6-icmp", "", ""); err != nil {
		if rawTests() {
			t.Fatalf("no raw ICMPv6 socket: %v", err)
		}
		t.Skipf("no raw ICMPv6 socket: %v", err)
	} else {
		c.Close()
	}
	got := pings(t, PingMethodEcho, "::1", "lo", 50, 1, 500*time.Millisecond)
	if replies(got) < 3 {
		t.Fatalf("got %d replies from ::1 in 500 ms, want 3 or more", replies(got))
	}
}

// A request without a reply gives a timeout after the deadline, also if the
// next request comes much later. 192.0.2.1 (TEST-NET-1) has no route in
// the test network namespace: no reply.
func TestPinger_DeadlineIndependentOfInterval(t *testing.T) {
	needRawSocket(t)
	start := time.Now()
	got := pings(t, PingMethodEcho, "192.0.2.1", "", 5000, 0.2, 600*time.Millisecond)
	if len(got) != 1 || !got[0].Timeout {
		t.Fatalf("results %+v, want one timeout", got)
	}
	if d := got[0].Timestamp.Sub(start); d > 500*time.Millisecond {
		t.Errorf("timeout after %v, want about 0.2 to 0.3 s (not at the next request after 5 s)", d)
	}
}

// loopbackPings runs the pinger of the link to 127.0.0.1 for the time d
// and returns the results.
func loopbackPings(t *testing.T, method, ifname string, d time.Duration) []PingResult {
	return pings(t, method, "127.0.0.1", ifname, 50, 1, d)
}

// pings runs the pinger of the link to the reflector for the time d, with
// the ping interval (ms) and the deadline (s), and returns the results.
func pings(t *testing.T, method, reflector, ifname string, intervalMs int, deadlineS float64, d time.Duration) []PingResult {
	t.Helper()
	cfg := testConfig()
	cfg.PingMethod = method
	cfg.PingIntervalMs = intervalMs
	cfg.ReflectorResponseDeadlineS = deadlineS
	cfg.Links[0].Reflectors = []string{reflector}
	cfg.Links[0].PingInterfaceName = ifname
	pm := NewPingerManager(&cfg.Links[0], cfg, testLogger(t))
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	results := make(chan PingResult, 256)
	done := make(chan struct{})
	go func() {
		pm.Run(ctx, results)
		close(done)
	}()
	<-done
	close(results)
	var got []PingResult
	for r := range results {
		got = append(got, r)
	}
	return got
}

func replies(rs []PingResult) int {
	n := 0
	for _, r := range rs {
		if !r.Timeout {
			n++
		}
	}
	return n
}

// The echo pinger uses the raw socket of the timestamp pinger. This test
// needs CAP_NET_RAW. To run it without root:
//
//	unshare -rn sh -c 'ip link set lo up && go test -run TestEchoPinger -v'
func TestEchoPinger_Loopback(t *testing.T) {
	needRawSocket(t)
	got := loopbackPings(t, PingMethodEcho, "lo", 500*time.Millisecond)
	if replies(got) < 3 {
		t.Fatalf("got %d replies in 500 ms, want 3 or more", replies(got))
	}
	for _, r := range got {
		if !r.Timeout && (r.OWD || r.RTT <= 0) {
			t.Errorf("an echo reply has an RTT and no one-way delays: %+v", r)
		}
	}
}

// A pinger bound to an interface must send only through it: pings to
// 127.0.0.1 through a dummy interface get no reply. It needs CAP_NET_ADMIN
// for the dummy interface (unshare -rn gives it).
func TestEchoPinger_BoundToInterface(t *testing.T) {
	needRawSocket(t)
	if err := exec.Command("ip", "link", "add", "capdummy0", "type", "dummy").Run(); err != nil {
		if rawTests() {
			t.Fatal("cannot add a dummy interface")
		}
		t.Skip("cannot add a dummy interface (CAP_NET_ADMIN and ip are necessary)")
	}
	defer exec.Command("ip", "link", "del", "capdummy0").Run()
	if err := exec.Command("ip", "link", "set", "capdummy0", "up").Run(); err != nil {
		t.Fatal(err)
	}
	if n := replies(loopbackPings(t, PingMethodEcho, "capdummy0", 500*time.Millisecond)); n != 0 {
		t.Errorf("got %d replies through the dummy interface, want 0", n)
	}
}

// needRawSocket skips the test without CAP_NET_RAW.
func needRawSocket(t *testing.T) {
	t.Helper()
	conn, err := listenICMP(context.Background(), "", "")
	if err != nil {
		if (errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission)) && !rawTests() {
			t.Skip("CAP_NET_RAW is necessary")
		}
		t.Fatal(err)
	}
	conn.Close()
}

// rawTests tells if the tests with raw sockets must run (CI sets
// CAKE_RAW_TESTS=1): then a missing capability is an error, not a skip.
func rawTests() bool { return os.Getenv("CAKE_RAW_TESTS") == "1" }

// An IPv6 reflector keeps its zone (a link-local address needs it).
func TestResolveIP_KeepsZone(t *testing.T) {
	a, err := resolveIP(context.Background(), "fe80::1%lo")
	if err != nil {
		t.Fatal(err)
	}
	if a.Zone != "lo" || a.IP.String() != "fe80::1" {
		t.Errorf("got %v zone %q, want fe80::1 zone lo", a.IP, a.Zone)
	}
	if a, err := resolveIP(context.Background(), "127.0.0.1"); err != nil || a.IP.To4() == nil {
		t.Errorf("IPv4: %v %v", a, err)
	}
}
