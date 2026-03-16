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
	"unsafe"

	"golang.org/x/sys/unix"
)

// Netlink/TC constants for CAKE qdisc.
const (
	tcaKind    = 1 // TCA_KIND
	tcaOptions = 2 // TCA_OPTIONS

	tcaCakeBaseRate64 = 2 // TCA_CAKE_BASE_RATE64

	tcHRoot = 0xFFFFFFFF // TC_H_ROOT

	nlaFNested  = (1 << 15) // NLA_F_NESTED
	nlaHdrLen   = 4         // sizeof(struct nlattr)
	nlaAlignTo  = 4
	sizeofTcMsg = int(unsafe.Sizeof(tcMsg{}))
)

// tcMsg mirrors the kernel's struct tcmsg.
type tcMsg struct {
	Family  uint8
	Pad     [3]byte
	Ifindex int32
	Handle  uint32
	Parent  uint32
	Info    uint32
}

// Shaper controls CAKE qdisc bandwidth settings via netlink.
// Falls back to the tc command for operations not supported via netlink.
type Shaper struct {
	logger    *Logger
	mu        sync.Mutex
	fd        int // netlink socket
	seq       uint32
	ifIndices map[string]int32 // cached interface indices
	lastRates map[string]int   // last applied rate per interface, to skip no-ops
}

// NewShaper creates a new Shaper with a persistent netlink socket.
func NewShaper(logger *Logger) *Shaper {
	s := &Shaper{
		logger:    logger,
		fd:        -1,
		ifIndices: make(map[string]int32),
		lastRates: make(map[string]int),
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
func (s *Shaper) setRateNetlink(iface string, rateKbps int) error {
	ifindex, err := s.getIfindex(iface)
	if err != nil {
		return err
	}

	// Build the netlink message:
	//   nlmsghdr + tcmsg + TCA_KIND("cake") + TCA_OPTIONS{ TCA_CAKE_BASE_RATE64 }
	s.seq++
	seq := s.seq

	// TCA_CAKE_BASE_RATE64: rate in bytes/sec (uint64)
	bytesPerSec := uint64(rateKbps) * 125 // kbit/s -> bytes/s: *1000/8 = *125
	rateNla := buildNla(tcaCakeBaseRate64, uint64ToBytes(bytesPerSec))

	// TCA_OPTIONS (nested) containing the rate attribute
	optionsNla := buildNla(tcaOptions|nlaFNested, rateNla)

	// TCA_KIND = "cake\0"
	kindNla := buildNla(tcaKind, append([]byte("cake"), 0))

	// tcmsg
	tc := tcMsg{
		Ifindex: ifindex,
		Parent:  tcHRoot,
	}
	tcBytes := (*(*[sizeofTcMsg]byte)(unsafe.Pointer(&tc)))[:]

	// Assemble payload: tcmsg + kind + options
	payload := make([]byte, 0, len(tcBytes)+len(kindNla)+len(optionsNla))
	payload = append(payload, tcBytes...)
	payload = append(payload, kindNla...)
	payload = append(payload, optionsNla...)

	// nlmsghdr
	msgLen := uint32(unix.SizeofNlMsghdr + len(payload))
	hdr := unix.NlMsghdr{
		Len:   msgLen,
		Type:  unix.RTM_NEWQDISC,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK,
		Seq:   seq,
	}
	hdrBytes := (*(*[unix.SizeofNlMsghdr]byte)(unsafe.Pointer(&hdr)))[:]

	msg := make([]byte, 0, msgLen)
	msg = append(msg, hdrBytes...)
	msg = append(msg, payload...)

	// Send
	if err := unix.Sendto(s.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send on %s: %w", iface, err)
	}

	// Receive ACK
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(s.fd, buf, 0)
	if err != nil {
		return fmt.Errorf("netlink recv on %s: %w", iface, err)
	}

	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return fmt.Errorf("netlink parse on %s: %w", iface, err)
	}

	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR {
			// Error payload is a 4-byte errno followed by the original header
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

// buildNla constructs a netlink attribute (struct nlattr + payload), 4-byte aligned.
func buildNla(attrType int, data []byte) []byte {
	nlaLen := nlaHdrLen + len(data)
	aligned := (nlaLen + nlaAlignTo - 1) &^ (nlaAlignTo - 1)
	buf := make([]byte, aligned)
	binary.LittleEndian.PutUint16(buf[0:2], uint16(nlaLen))
	binary.LittleEndian.PutUint16(buf[2:4], uint16(attrType))
	copy(buf[nlaHdrLen:], data)
	return buf
}

func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// execCommand runs an external command and returns combined output.
func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
