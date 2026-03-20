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
	"os"
	"os/exec"
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
}

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

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		logger.Infof("netlink socket failed (%v), shaper will use tc exec fallback", err)
		return s
	}

	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		logger.Infof("netlink bind failed (%v), shaper will use tc exec fallback", err)
		return s
	}

	// Set a receive timeout so Recvfrom won't block indefinitely if the
	// kernel fails to emit an ACK. On timeout the netlink path returns an
	// error and the caller falls back to tc exec.
	tv := unix.Timeval{Sec: 5}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		logger.Infof("netlink SO_RCVTIMEO failed (%v), shaper will use tc exec fallback", err)
		return s
	}

	s.fd = fd
	return s
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

	useTc := s.fd < 0

	if !useTc {
		err := s.setRateNetlink(iface, rateKbps)
		if err != nil {
			// Invalidate cached ifindex — interface may have been
			// re-created with a different index (e.g. after replug).
			delete(s.ifIndices, iface)
			// Disable netlink for future calls to avoid retrying a
			// permanently broken path.
			s.logger.Infof("netlink failed on %s (%v), falling back to tc exec", iface, err)
			unix.Close(s.fd)
			s.fd = -1
			useTc = true
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
func (s *Shaper) setRateTc(iface string, rateKbps int) error {
	bw := fmt.Sprintf("%dkbit", rateKbps)
	output, err := execCommand("tc", "qdisc", "change", "dev", iface, "root", "cake", "bandwidth", bw)
	if err != nil {
		return fmt.Errorf("tc qdisc change on %s: %w: %s", iface, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// getIfindex returns the interface index, reading from sysfs and caching.
func (s *Shaper) getIfindex(iface string) (int32, error) {
	if idx, ok := s.ifIndices[iface]; ok {
		return idx, nil
	}

	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/ifindex", iface))
	if err != nil {
		return 0, fmt.Errorf("reading ifindex for %s: %w", iface, err)
	}
	val, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing ifindex for %s: %w", iface, err)
	}

	idx := int32(val)
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

// execCommand runs an external command with a timeout and returns combined output.
func execCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tcExecTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
