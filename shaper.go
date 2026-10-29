// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Netlink/TC constants for CAKE qdisc.
const (
	tcaKind    = 1 // TCA_KIND
	tcaOptions = 2 // TCA_OPTIONS

	tcaCakeBaseRate64 = 2 // TCA_CAKE_BASE_RATE64

	tcHRoot = 0xFFFFFFFF // TC_H_ROOT

	nlaFNested = (1 << 15) // NLA_F_NESTED
)

// Shaper controls CAKE qdisc bandwidth settings via netlink.
// Falls back to the tc command for operations not supported via netlink.
type Shaper struct {
	logger    *Logger
	mu        sync.Mutex
	fd        int // netlink socket
	seq       uint32
	ifIndices map[string]int32 // cached interface indices
	lastRates map[string]int   // last applied rate per interface, to skip no-ops
	msgBuf    []byte           // reusable buffer for building netlink messages
	recvBuf   []byte           // reusable buffer for receiving netlink responses

	// netlinkRetryAt is the time to open the netlink socket again after a
	// netlink error. While fd < 0 and before this time, SetRate uses tc. A
	// zero time means no retry.
	netlinkRetryAt time.Time
	now            func() time.Time // the clock; nil means time.Now
}

// netlinkRetryInterval is the time between attempts to use netlink again
// after a netlink error. Thus, after a temporary error (for example, a USB
// modem that is not connected yet), the shaper uses tc exec only for this
// time, not for the remaining time of the process.
const netlinkRetryInterval = 60 * time.Second

// NewShaper creates a new Shaper with a persistent netlink socket.
func NewShaper(logger *Logger) *Shaper {
	s := &Shaper{
		logger:    logger,
		fd:        -1,
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
		msgBuf:    make([]byte, cakeRateMsgSize), // pre-allocate for netlink messages
		recvBuf:   make([]byte, 4096),   // pre-allocate for netlink responses
	}

	fd, err := openNetlink()
	if err != nil {
		logger.Infof("%v, shaper will use tc exec fallback", err)
		s.netlinkRetryAt = s.clock().Add(netlinkRetryInterval)
		return s
	}
	s.fd = fd
	return s
}

// openNetlink opens and binds a NETLINK_ROUTE socket.
func openNetlink() (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return -1, fmt.Errorf("netlink socket failed (%w)", err)
	}

	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("netlink bind failed (%w)", err)
	}

	// Set a receive timeout so Recvfrom won't block indefinitely if the
	// kernel fails to emit an ACK. On timeout the netlink path returns an
	// error and the caller falls back to tc exec.
	tv := unix.Timeval{Sec: 5}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("netlink SO_RCVTIMEO failed (%w)", err)
	}
	return fd, nil
}

func (s *Shaper) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// netlinkReady reports whether SetRate can use netlink. If the socket is
// closed and the retry time is reached, it opens a new socket. A new socket
// also removes late replies to old requests. The caller holds s.mu.
func (s *Shaper) netlinkReady() bool {
	if s.fd >= 0 {
		return true
	}
	if s.netlinkRetryAt.IsZero() || s.clock().Before(s.netlinkRetryAt) {
		return false
	}
	fd, err := openNetlink()
	if err != nil {
		s.netlinkRetryAt = s.clock().Add(netlinkRetryInterval)
		return false
	}
	s.logger.Infof("netlink is available again")
	s.fd = fd
	return true
}

// Close releases the netlink socket.
func (s *Shaper) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd >= 0 {
		unix.Close(s.fd)
		s.fd = -1
	}
}

// InvalidateCache clears cached rate and interface index for the given
// interface, forcing the next SetRate to re-apply unconditionally. Call
// this after events that may have reset the qdisc (link flap, hotplug).
func (s *Shaper) InvalidateCache(iface string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastRates, iface)
	delete(s.ifIndices, iface)
}

// SetRate sets the CAKE bandwidth for the given interface.
// rateKbps is the target bandwidth in kbit/s.
func (s *Shaper) SetRate(iface string, rateKbps int) error {
	if rateKbps <= 0 {
		return fmt.Errorf("rate must be positive, got %d", rateKbps)
	}

	s.mu.Lock()

	// Skip if rate hasn't changed
	if s.lastRates[iface] == rateKbps {
		s.mu.Unlock()
		return nil
	}

	if s.netlinkReady() {
		err := s.setRateNetlink(iface, rateKbps)
		if err != nil {
			// Invalidate cached ifindex — interface may have been
			// re-created with a different index (e.g. after replug).
			delete(s.ifIndices, iface)
			// Use tc until the retry time. Then open a new socket.
			s.logger.Infof("netlink failed on %s (%v), using tc exec for %v", iface, err, netlinkRetryInterval)
			unix.Close(s.fd)
			s.fd = -1
			s.netlinkRetryAt = s.clock().Add(netlinkRetryInterval)
		} else {
			s.lastRates[iface] = rateKbps
			s.mu.Unlock()
			return nil
		}
	}

	// Release the lock before running tc exec, which can block up to
	// tcExecTimeout (5s). This avoids serializing rate updates for all
	// interfaces behind one slow tc invocation.
	s.mu.Unlock()

	err := s.setRateTc(iface, rateKbps)
	if err == nil {
		s.mu.Lock()
		s.lastRates[iface] = rateKbps
		s.mu.Unlock()
	}
	return err
}

// cakeRateMsg is the fixed size of a netlink RTM_NEWQDISC message for CAKE
// bandwidth updates: nlmsghdr(16) + tcmsg(20) + TCA_KIND(12) + TCA_OPTIONS(16) = 64.
const cakeRateMsgSize = 64

// Named offsets into the netlink message buffer.
const (
	offNlmsgLen   = 0
	offNlmsgType  = 4
	offNlmsgFlags = 6
	offNlmsgSeq   = 8
	offNlmsgPid   = 12
	offTcmFamily  = 16 // tcmsg starts at 16
	offTcmIfindex = 20
	offTcmHandle  = 24
	offTcmParent  = 28
	offTcmInfo    = 32
	offKindLen    = 36 // TCA_KIND nla
	offKindType   = 38
	offKindVal    = 40 // "cake\0"
	offOptsLen    = 48 // TCA_OPTIONS nla (nested)
	offOptsType   = 50
	offRateLen    = 52 // TCA_CAKE_BASE_RATE64 nla
	offRateType   = 54
	offRateVal    = 56
)

// buildCakeRateMsg writes a netlink RTM_NEWQDISC message for setting the CAKE
// base rate into buf. buf must be at least cakeRateMsgSize bytes.
// This is a pure function with no side effects, suitable for unit testing.
func buildCakeRateMsg(buf []byte, seq uint32, ifindex int32, rateKbps int) {
	// nlmsghdr (16 bytes)
	binary.NativeEndian.PutUint32(buf[offNlmsgLen:], cakeRateMsgSize)
	binary.NativeEndian.PutUint16(buf[offNlmsgType:], unix.RTM_NEWQDISC)
	binary.NativeEndian.PutUint16(buf[offNlmsgFlags:], unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_REPLACE)
	binary.NativeEndian.PutUint32(buf[offNlmsgSeq:], seq)
	binary.NativeEndian.PutUint32(buf[offNlmsgPid:], 0)

	// tcmsg (20 bytes)
	buf[offTcmFamily] = 0
	buf[offTcmFamily+1] = 0
	buf[offTcmFamily+2] = 0
	buf[offTcmFamily+3] = 0
	binary.NativeEndian.PutUint32(buf[offTcmIfindex:], uint32(ifindex))
	binary.NativeEndian.PutUint32(buf[offTcmHandle:], 0)
	binary.NativeEndian.PutUint32(buf[offTcmParent:], tcHRoot)
	binary.NativeEndian.PutUint32(buf[offTcmInfo:], 0)

	// TCA_KIND nla: "cake\0" padded to 8 bytes
	binary.NativeEndian.PutUint16(buf[offKindLen:], 9) // nla_len = 4 + 5
	binary.NativeEndian.PutUint16(buf[offKindType:], tcaKind)
	copy(buf[offKindVal:], "cake\x00")
	buf[45] = 0 // alignment padding
	buf[46] = 0
	buf[47] = 0

	// TCA_OPTIONS nla (nested): contains TCA_CAKE_BASE_RATE64
	binary.NativeEndian.PutUint16(buf[offOptsLen:], 16) // nla_len = 4 + 12
	binary.NativeEndian.PutUint16(buf[offOptsType:], tcaOptions|nlaFNested)

	// TCA_CAKE_BASE_RATE64 nla
	binary.NativeEndian.PutUint16(buf[offRateLen:], 12) // nla_len = 4 + 8
	binary.NativeEndian.PutUint16(buf[offRateType:], tcaCakeBaseRate64)
	bytesPerSec := uint64(rateKbps) * 125 // kbit/s -> bytes/s
	binary.NativeEndian.PutUint64(buf[offRateVal:], bytesPerSec)
}

// setRateNetlink sets the CAKE bandwidth via a raw netlink RTM_NEWQDISC message.
// Reuses pre-allocated buffers to avoid allocations on the hot path.
func (s *Shaper) setRateNetlink(iface string, rateKbps int) error {
	ifindex, err := s.getIfindex(iface)
	if err != nil {
		return err
	}

	s.seq++
	buildCakeRateMsg(s.msgBuf[:cakeRateMsgSize], s.seq, ifindex, rateKbps)

	// Send
	if err := unix.Sendto(s.fd, s.msgBuf[:cakeRateMsgSize], 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send on %s: %w", iface, err)
	}

	// Receive ACK using pre-allocated buffer
	n, _, err := unix.Recvfrom(s.fd, s.recvBuf, 0)
	if err != nil {
		return fmt.Errorf("netlink recv on %s: %w", iface, err)
	}

	msgs, err := syscall.ParseNetlinkMessage(s.recvBuf[:n])
	if err != nil {
		return fmt.Errorf("netlink parse on %s: %w", iface, err)
	}

	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR {
			if len(m.Data) >= 4 {
				errno := int32(binary.NativeEndian.Uint32(m.Data[:4]))
				if errno == 0 {
					return nil // ACK (success)
				}
				return fmt.Errorf("netlink error on %s: %s", iface, unix.Errno(-errno))
			}
		}
	}

	return fmt.Errorf("unexpected netlink response on %s", iface)
}

// tcExecTimeout is the maximum time to wait for a tc command to complete.
const tcExecTimeout = 5 * time.Second

// setRateTc sets the CAKE bandwidth by executing the tc command (fallback).
// The root qdisc must be CAKE: the tc of OpenWrt (tc-tiny) replaces
// another root qdisc with a new CAKE qdisc with default options on "tc
// qdisc change ... cake" (netlink refuses this change).
func (s *Shaper) setRateTc(iface string, rateKbps int) error {
	output, err := execCommand("tc", "qdisc", "show", "dev", iface, "root")
	if err != nil {
		return fmt.Errorf("tc qdisc show on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}
	if f := strings.Fields(string(output)); len(f) < 2 || f[0] != "qdisc" || f[1] != "cake" {
		return fmt.Errorf("the root qdisc of %s is not CAKE: %s", iface, strings.TrimSpace(string(output)))
	}
	bw := fmt.Sprintf("%dkbit", rateKbps)
	output, err = execCommand("tc", "qdisc", "change", "dev", iface, "root", "cake", "bandwidth", bw)
	if err != nil {
		return fmt.Errorf("tc qdisc change on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// getIfindex returns the interface index (with netlink, through the net
// package) and caches it.
func (s *Shaper) getIfindex(iface string) (int32, error) {
	if idx, ok := s.ifIndices[iface]; ok {
		return idx, nil
	}

	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return 0, fmt.Errorf("ifindex for %s: %w", iface, err)
	}

	idx := int32(ifi.Index)
	s.ifIndices[iface] = idx
	return idx, nil
}

// GetRate reads the current CAKE bandwidth for the given interface.
// Returns the rate in kbit/s. Uses tc exec since this is not in the hot path.
func (s *Shaper) GetRate(iface string) (int, error) {
	output, err := execCommand("tc", "qdisc", "show", "dev", iface, "root")
	if err != nil {
		return 0, fmt.Errorf("tc qdisc show on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}

	// Parse output like: "qdisc cake 8001: root refcnt 2 bandwidth 20000Kbit ..."
	line := string(output)
	idx := strings.Index(line, "bandwidth ")
	if idx < 0 {
		return 0, fmt.Errorf("no CAKE bandwidth found on %s", iface)
	}
	rateStr := line[idx+len("bandwidth "):]
	endIdx := strings.IndexAny(rateStr, " \n\t")
	if endIdx > 0 {
		rateStr = rateStr[:endIdx]
	}

	rateStr = strings.TrimSpace(rateStr)
	switch {
	case strings.HasSuffix(rateStr, "Kbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Kbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val), nil
	case strings.HasSuffix(rateStr, "Mbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Mbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val * 1000), nil
	case strings.HasSuffix(rateStr, "Gbit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "Gbit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val * 1000000), nil
	case strings.HasSuffix(rateStr, "bit"):
		val, err := strconv.ParseFloat(strings.TrimSuffix(rateStr, "bit"), 64)
		if err != nil {
			return 0, fmt.Errorf("parsing rate %q: %w", rateStr, err)
		}
		return int(val / 1000), nil
	default:
		return 0, fmt.Errorf("unrecognized rate format %q", rateStr)
	}
}

// WirePacketCompensationUs calculates the serialization delay in microseconds
// for a single MTU-sized packet at the given rate. This is added to the delay
// threshold to prevent false bufferbloat detection at low shaper rates.
func WirePacketCompensationUs(mtuBytes int, rateKbps int) float64 {
	if rateKbps <= 0 {
		return 0
	}
	mtuBits := float64(mtuBytes) * 8.0
	return (1000.0 * mtuBits) / float64(rateKbps)
}

// execCommand runs an external command with a timeout and returns combined
// output. Tests replace it.
var execCommand = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tcExecTimeout)
	defer cancel()
	caps := childCaps()
	if len(caps) > 0 {
		// The command gets the capabilities: it must not come from a
		// directory in PATH that the caller can change.
		path, err := trustedPath(name)
		if err != nil {
			return nil, err
		}
		name = path
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if len(caps) > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: caps}
		// A command with ambient capabilities does not run in the secure
		// mode of a setuid or setcap program: the dynamic loader would use
		// LD_PRELOAD and similar variables of the caller. Thus, the command
		// gets a fixed environment.
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	}
	return cmd.CombinedOutput()
}

// trustedDirs are the directories of the system commands (ip, tc,
// conntrack).
var trustedDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// trustedPath returns the path of a command in trustedDirs. The error
// wraps exec.ErrNotFound if the command is not there.
func trustedPath(name string) (string, error) {
	for _, d := range trustedDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s in %v: %w", name, trustedDirs, exec.ErrNotFound)
}

// childCaps returns the capabilities that a command (tc, ip) must get
// from a program that is not root: CAP_NET_ADMIN and CAP_NET_RAW, if the
// program has them (for example as file capabilities). Without ambient
// capabilities, the command does not get them, and it fails with
// "Operation not permitted". Root needs no ambient capabilities.
var childCaps = sync.OnceValue(func() []uintptr {
	if os.Geteuid() == 0 {
		return nil
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return nil
	}
	var caps []uintptr
	for _, c := range []uintptr{unix.CAP_NET_ADMIN, unix.CAP_NET_RAW} {
		if data[c/32].Permitted&(1<<(c%32)) != 0 {
			caps = append(caps, c)
		}
	}
	return caps
})
