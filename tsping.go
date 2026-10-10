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
	"math/rand"
	"net"
	"sync"
	"syscall"
	"time"
)

// ICMP timestamp messages (RFC 792). A reply contains three times: the
// originate time (set by the sender), the receive time and the transmit time
// (set by the reflector). Each time is in milliseconds since midnight UTC.
// Thus, the reply gives the delay of each direction, plus the clock offset
// between the two hosts.
const (
	icmpTypeTimestamp      = 13
	icmpTypeTimestampReply = 14
	icmpTimestampLen       = 20

	msPerDay = 24 * 60 * 60 * 1000
	// A reflector sets the high bit if its time is not in milliseconds
	// since midnight UTC (RFC 792). Then the time cannot give a delay.
	tsNonStandard = 1 << 31
)

// msSinceMidnightUTC returns the time in milliseconds since midnight UTC.
func msSinceMidnightUTC(t time.Time) uint32 {
	return uint32(t.UnixMilli() % msPerDay)
}

// tsDiffMs returns b - a in milliseconds for two times since midnight. The
// result is in the range [-12 h, +12 h). Thus, a difference across midnight
// is correct.
func tsDiffMs(a, b uint32) int64 {
	d := (int64(b) - int64(a)) % msPerDay
	if d >= msPerDay/2 {
		d -= msPerDay
	} else if d < -msPerDay/2 {
		d += msPerDay
	}
	return d
}

// marshalTimestampRequest returns an ICMP timestamp request.
func marshalTimestampRequest(id, seq uint16, originate uint32) []byte {
	b := make([]byte, icmpTimestampLen)
	b[0] = icmpTypeTimestamp
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	binary.BigEndian.PutUint32(b[8:], originate)
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

// timestampReply is the content of an ICMP timestamp reply.
type timestampReply struct {
	id, seq                      uint16
	originate, receive, transmit uint32
}

// parseTimestampReply returns the content of an ICMP timestamp reply. It
// returns false if b is not a timestamp reply.
func parseTimestampReply(b []byte) (timestampReply, bool) {
	if len(b) < icmpTimestampLen || b[0] != icmpTypeTimestampReply || b[1] != 0 {
		return timestampReply{}, false
	}
	return timestampReply{
		id:        binary.BigEndian.Uint16(b[4:]),
		seq:       binary.BigEndian.Uint16(b[6:]),
		originate: binary.BigEndian.Uint32(b[8:]),
		receive:   binary.BigEndian.Uint32(b[12:]),
		transmit:  binary.BigEndian.Uint32(b[16:]),
	}, true
}

// oneWayDelays returns the upload and the download delay of a reply that
// arrived at recvMs. It returns false if the reflector does not use
// standard times.
func (r timestampReply) oneWayDelays(recvMs uint32) (ul, dl time.Duration, ok bool) {
	if r.receive&tsNonStandard != 0 || r.transmit&tsNonStandard != 0 {
		return 0, 0, false
	}
	ul = time.Duration(tsDiffMs(r.originate, r.receive)) * time.Millisecond
	dl = time.Duration(tsDiffMs(r.transmit, recvMs)) * time.Millisecond
	return ul, dl, true
}

// icmpChecksum returns the internet checksum of b.
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// listenICMP opens a raw ICMP socket (CAP_NET_RAW is necessary). If
// ifname is not empty, the socket sends and receives only on that interface.
func listenICMP(ctx context.Context, source, ifname string) (net.PacketConn, error) {
	lc := net.ListenConfig{}
	if ifname != "" {
		lc.Control = func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				serr = syscall.BindToDevice(int(fd), ifname)
			}); err != nil {
				return err
			}
			return serr
		}
	}
	if source == "" {
		source = "0.0.0.0"
	}
	return lc.ListenPacket(ctx, "ip4:icmp", source)
}

// runTimestampPinger sends ICMP timestamp requests to one reflector. It
// sends a PingResult with the RTT and the one-way delays for each reply,
// and a timeout result for each request without a reply in the deadline.
func (pm *PingerManager) runTimestampPinger(ctx context.Context, reflector string, interval, deadline time.Duration, resultCh chan<- PingResult) error {
	dst, err := net.ResolveIPAddr("ip4", reflector)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", reflector, err)
	}
	conn, err := listenICMP(ctx, pm.link.PingSourceAddr, pm.link.PingInterfaceName)
	if err != nil {
		return fmt.Errorf("opening ICMP socket: %w", err)
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-pctx.Done()
		conn.Close()
	}()

	// The raw socket receives all ICMP messages of the host. The ID
	// identifies the replies of this pinger.
	id := uint16(rand.Uint32())
	var mu sync.Mutex
	pending := make(map[uint16]time.Time) // seq -> send time

	send := func(res PingResult) {
		select {
		case resultCh <- res:
		case <-pctx.Done():
		}
	}

	// Receiver
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return // the socket is closed
			}
			recvTime := time.Now()
			reply, ok := parseTimestampReply(buf[:n])
			if !ok || reply.id != id {
				continue
			}
			if ip, isIP := from.(*net.IPAddr); !isIP || !ip.IP.Equal(dst.IP) {
				continue
			}
			mu.Lock()
			sent, found := pending[reply.seq]
			delete(pending, reply.seq)
			mu.Unlock()
			if !found {
				continue // late or unexpected reply
			}
			pm.recordHealth(reflector, false)
			res := PingResult{Reflector: reflector, RTT: recvTime.Sub(sent), Timestamp: recvTime}
			if ul, dl, ok := reply.oneWayDelays(msSinceMidnightUTC(recvTime)); ok {
				res.OWD, res.UlOWD, res.DlOWD = true, ul, dl
			}
			send(res)
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq uint16
	for {
		now := time.Now()

		// Timeouts
		var expired int
		mu.Lock()
		for s, sent := range pending {
			if now.Sub(sent) > deadline {
				delete(pending, s)
				expired++
			}
		}
		mu.Unlock()
		for range expired {
			pm.recordHealth(reflector, true)
			send(PingResult{Reflector: reflector, Timestamp: now, Timeout: true})
		}

		// Request
		seq++
		mu.Lock()
		pending[seq] = now
		mu.Unlock()
		if _, err := conn.WriteTo(marshalTimestampRequest(id, seq, msSinceMidnightUTC(now)), dst); err != nil {
			if pctx.Err() != nil {
				return nil
			}
			pm.logger.Debugf("timestamp ping %s: %v", reflector, err)
		}

		select {
		case <-pctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
