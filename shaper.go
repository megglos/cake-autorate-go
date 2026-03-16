// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

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
		msgBuf:    make([]byte, 64), // pre-allocate for netlink messages (exact size)
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

// SetRate sets the CAKE bandwidth for the given interface.
// rateKbps is the target bandwidth in kbit/s.
func (s *Shaper) SetRate(iface string, rateKbps int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Skip if rate hasn't changed
	if s.lastRates[iface] == rateKbps {
		return nil
	}

	var err error
	if s.fd >= 0 {
		err = s.setRateNetlink(iface, rateKbps)
	} else {
		err = s.setRateTc(iface, rateKbps)
	}
	if err == nil {
		s.lastRates[iface] = rateKbps
	}
	return err
}

// setRateNetlink sets the CAKE bandwidth via a raw netlink RTM_NEWQDISC message.
// Reuses pre-allocated buffers to avoid allocations on the hot path.
func (s *Shaper) setRateNetlink(iface string, rateKbps int) error {
	ifindex, err := s.getIfindex(iface)
	if err != nil {
		return err
	}

	// Build the netlink message into the reusable buffer:
	//   nlmsghdr + tcmsg + TCA_KIND("cake\0") + TCA_OPTIONS{ TCA_CAKE_BASE_RATE64 }
	//
	// Layout (all sizes fixed):
	//   nlmsghdr:          16 bytes
	//   tcmsg:             20 bytes
	//   TCA_KIND nla:       4 (hdr) + 5 ("cake\0") + 3 (pad) = 12 bytes
	//   TCA_OPTIONS nla:    4 (hdr) + [TCA_CAKE_BASE_RATE64: 4 (hdr) + 8 (u64)] = 16 bytes
	//   Total:             64 bytes
	const msgSize = 64

	s.seq++
	buf := s.msgBuf[:msgSize]

	// nlmsghdr (16 bytes)
	binary.LittleEndian.PutUint32(buf[0:4], msgSize)                                      // nlmsg_len
	binary.LittleEndian.PutUint16(buf[4:6], unix.RTM_NEWQDISC)                            // nlmsg_type
	binary.LittleEndian.PutUint16(buf[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK)            // nlmsg_flags
	binary.LittleEndian.PutUint32(buf[8:12], s.seq)                                       // nlmsg_seq
	binary.LittleEndian.PutUint32(buf[12:16], 0)                                          // nlmsg_pid

	// tcmsg (20 bytes at offset 16)
	buf[16] = 0                                                                            // tcm_family
	buf[17] = 0; buf[18] = 0; buf[19] = 0                                                 // pad
	binary.LittleEndian.PutUint32(buf[20:24], uint32(ifindex))                             // tcm_ifindex
	binary.LittleEndian.PutUint32(buf[24:28], 0)                                          // tcm_handle
	binary.LittleEndian.PutUint32(buf[28:32], tcHRoot)                                    // tcm_parent
	binary.LittleEndian.PutUint32(buf[32:36], 0)                                          // tcm_info

	// TCA_KIND nla (12 bytes at offset 36): "cake\0" padded to 8 bytes
	binary.LittleEndian.PutUint16(buf[36:38], 9)                                          // nla_len = 4 + 5
	binary.LittleEndian.PutUint16(buf[38:40], tcaKind)                                    // nla_type
	buf[40] = 'c'; buf[41] = 'a'; buf[42] = 'k'; buf[43] = 'e'; buf[44] = 0             // "cake\0"
	buf[45] = 0; buf[46] = 0; buf[47] = 0                                                 // alignment padding

	// TCA_OPTIONS nla (16 bytes at offset 48): nested, contains TCA_CAKE_BASE_RATE64
	binary.LittleEndian.PutUint16(buf[48:50], 16)                                         // nla_len = 4 + 12
	binary.LittleEndian.PutUint16(buf[50:52], tcaOptions|nlaFNested)                      // nla_type

	// TCA_CAKE_BASE_RATE64 nla (12 bytes at offset 52)
	binary.LittleEndian.PutUint16(buf[52:54], 12)                                         // nla_len = 4 + 8
	binary.LittleEndian.PutUint16(buf[54:56], tcaCakeBaseRate64)                           // nla_type
	bytesPerSec := uint64(rateKbps) * 125                                                  // kbit/s -> bytes/s
	binary.LittleEndian.PutUint64(buf[56:64], bytesPerSec)                                // rate value

	// Send
	if err := unix.Sendto(s.fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
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
				errno := int32(binary.LittleEndian.Uint32(m.Data[:4]))
				if errno == 0 {
					return nil // ACK (success)
				}
				return fmt.Errorf("netlink error on %s: %s", iface, unix.Errno(-errno))
			}
		}
	}

	return fmt.Errorf("unexpected netlink response on %s", iface)
}

// setRateTc sets the CAKE bandwidth by executing the tc command (fallback).
func (s *Shaper) setRateTc(iface string, rateKbps int) error {
	cmd := fmt.Sprintf("%dkbit", rateKbps)
	output, err := execCommand("tc", "qdisc", "change", "dev", iface, "root", "cake", "bandwidth", cmd)
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

// execCommand runs an external command and returns combined output.
func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
