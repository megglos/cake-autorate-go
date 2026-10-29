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
// arrived at recvMs, and the time in the reflector (from receive to
// transmit). It returns false if the reflector does not use standard
// times.
func (r timestampReply) oneWayDelays(recvMs uint32) (ul, dl, inReflector time.Duration, ok bool) {
	if r.receive&tsNonStandard != 0 || r.transmit&tsNonStandard != 0 {
		return 0, 0, 0, false
	}
	ul = time.Duration(tsDiffMs(r.originate, r.receive)) * time.Millisecond
	dl = time.Duration(tsDiffMs(r.transmit, recvMs)) * time.Millisecond
	inReflector = time.Duration(tsDiffMs(r.receive, r.transmit)) * time.Millisecond
	return ul, dl, inReflector, true
}

// ICMP echo messages (RFC 792). A reply gives only the RTT.
const (
	icmpTypeEchoReply   = 0
	icmpTypeEchoRequest = 8
	// icmpEchoLen is the length of an echo request: the header (8 bytes)
	// and 16 bytes of data.
	icmpEchoLen = 24
)

// marshalEchoRequest returns an ICMP echo request.
func marshalEchoRequest(id, seq uint16) []byte {
	b := make([]byte, icmpEchoLen)
	b[0] = icmpTypeEchoRequest
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

// parseEchoReply returns the ID and the sequence number of an ICMP echo
// reply. It returns false if b is not an echo reply.
func parseEchoReply(b []byte) (id, seq uint16, ok bool) {
	if len(b) < 8 || b[0] != icmpTypeEchoReply || b[1] != 0 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), true
}

// icmpKind is a kind of ICMP request of a pinger.
type icmpKind struct {
	// request returns a request, sent at now.
	request func(id, seq uint16, now time.Time) []byte
	// reply returns the ID and the sequence number of a reply that arrived
	// at recv. For a reply with one-way delays, it sets them in res. It
	// returns false if b is not a reply of this kind.
	reply func(b []byte, recv time.Time, res *PingResult) (id, seq uint16, ok bool)
}

// icmpEcho sends ICMP echo requests (ping_method icmp).
var icmpEcho = icmpKind{
	request: func(id, seq uint16, _ time.Time) []byte { return marshalEchoRequest(id, seq) },
	reply: func(b []byte, _ time.Time, _ *PingResult) (uint16, uint16, bool) {
		return parseEchoReply(b)
	},
}

// icmpTimestamp sends ICMP timestamp requests (ping_method
// icmp-timestamp).
var icmpTimestamp = icmpKind{
	request: func(id, seq uint16, now time.Time) []byte {
		return marshalTimestampRequest(id, seq, msSinceMidnightUTC(now))
	},
	reply: func(b []byte, recv time.Time, res *PingResult) (uint16, uint16, bool) {
		r, ok := parseTimestampReply(b)
		if !ok {
			return 0, 0, false
		}
		if ul, dl, in, ok := r.oneWayDelays(msSinceMidnightUTC(recv)); ok {
			res.OWD, res.UlOWD, res.DlOWD, res.ReflectorTime = true, ul, dl, in
		}
		return r.id, r.seq, true
	},
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

// resolveIPv4 returns the first IPv4 address of host. The lookup ends with
// ctx: a stop must not wait for a slow DNS server. Tests change it.
var resolveIPv4 = func(ctx context.Context, host string) (*net.IPAddr, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPv4 address")
	}
	return &net.IPAddr{IP: ips[0]}, nil
}

// runICMPPinger sends ICMP requests of one kind to one reflector. It sends
// a PingResult with the RTT (and the one-way delays of a timestamp reply)
// for each reply, and a timeout result for each request without a reply in
// the deadline.
//
// The socket is bound to the ping interface (SO_BINDTODEVICE). A hint for
// each packet (IP_PKTINFO) is not sufficient: with policy routing (for
// example mwan3), the router routes its own packets again after the mark,
// and then the requests can go through another link.
func (pm *PingerManager) runICMPPinger(ctx context.Context, reflector string, kind icmpKind, interval, deadline time.Duration, resultCh chan<- PingResult) error {
	dst, err := resolveIPv4(ctx, reflector)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", reflector, err)
	}
	conn, err := listenICMP(ctx, pm.link.PingSourceAddr, pm.link.PingInterfaceName)
	if err != nil {
		return fmt.Errorf("opening ICMP socket: %w", err)
	}
	pctx, cancel := context.WithCancel(ctx)
	// Stop and join the helper goroutines before return. Thus, no result of
	// this pinger comes after it returns.
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return // the socket is closed
			}
			recvTime := time.Now()
			res := PingResult{Reflector: reflector, Timestamp: recvTime}
			rid, rseq, ok := kind.reply(buf[:n], recvTime, &res)
			if !ok || rid != id {
				continue
			}
			if ip, isIP := from.(*net.IPAddr); !isIP || !ip.IP.Equal(dst.IP) {
				continue
			}
			mu.Lock()
			sent, found := pending[rseq]
			delete(pending, rseq)
			mu.Unlock()
			if !found {
				continue // late or unexpected reply
			}
			pm.recordHealth(reflector, false)
			res.RTT = recvTime.Sub(sent)
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
		if _, err := conn.WriteTo(kind.request(id, seq, now), dst); err != nil {
			if pctx.Err() != nil {
				return nil
			}
			pm.logger.Debugf("ping %s: %v", reflector, err)
		} else if pm.onSend != nil {
			pm.onSend(reflector)
		}

		select {
		case <-pctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
