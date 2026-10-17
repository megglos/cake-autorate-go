// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// The calibration and the bufferbloat test of the LuCI app use the browser
// to make the load (pinHosts). On a router with more than one WAN link
// (for example with mwan3), the routing can send this traffic through
// another link. The Pinner sends all traffic to the addresses of pinHosts
// through one link, for a time:
//
//   - a route table (pinTable) with the default route of the link, for
//     IPv4 and IPv6. Without an IPv6 route on the link, the IPv6 default
//     route of the table is "unreachable": the browser then uses IPv4.
//   - one "ip rule" for each address (to ADDR lookup pinTable), with the
//     priority pinPriority. mwan3 uses priorities from 1001: the rules of
//     the Pinner come first.

// pinTable is the route table of the Pinner.
const pinTable = "7370"

// pinPriority is the priority of the rules of the Pinner.
const pinPriority = "900"

// maxPinSeconds is the maximum time of "pin LINK SECONDS".
const maxPinSeconds = 3600

// pinLookupTimeout is the maximum time of the DNS lookup of pinHosts.
const pinLookupTimeout = 5 * time.Second

// pinMaxRules is the maximum number of rules that clear removes for each
// address family.
const pinMaxRules = 64

// pinHosts are the hosts of the load of the LuCI app.
var pinHosts = []string{"speed.cloudflare.com"}

// lookupPinHost returns the addresses of a host. Tests replace it.
var lookupPinHost = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// Pinner sends the traffic to pinHosts through one link (see above). Only
// one link at a time can have the traffic.
type Pinner struct {
	devices map[string]string // link name: device of the upload
	logger  *Logger

	mu    sync.Mutex
	link  string // the link with the traffic, "" if none
	timer *time.Timer
}

// NewPinner returns a Pinner for the links. The traffic goes through the
// device of the upload of a link (upload.interface).
func NewPinner(links []LinkConfig, logger *Logger) *Pinner {
	p := &Pinner{devices: make(map[string]string, len(links)), logger: logger}
	for _, l := range links {
		p.devices[l.Name] = l.Upload.Interface
	}
	return p
}

// pinRoute is the default route of a device: the gateway, or "" for a
// point-to-point device (for example PPPoE).
type pinRoute struct {
	gateway string
}

// Pin sends the traffic to pinHosts through the link for the time d. A
// second Pin of the same link sets the rules again (new addresses) and
// starts the time again. Pin refuses another link while a link has the
// traffic.
func (p *Pinner) Pin(link string, d time.Duration) error {
	dev, ok := p.devices[link]
	if !ok {
		return fmt.Errorf("unknown link %q", link)
	}
	if dev == "" {
		return fmt.Errorf("link %q has no upload interface", link)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link != "" && p.link != link {
		return fmt.Errorf("link %q has the traffic of the load, unpin it first", p.link)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pinLookupTimeout)
	defer cancel()
	var v4, v6 []string
	for _, host := range pinHosts {
		ips, err := lookupPinHost(ctx, host)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", host, err)
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				v4 = append(v4, ip.String())
			} else {
				v6 = append(v6, ip.String())
			}
		}
	}
	if len(v4) == 0 {
		return fmt.Errorf("no IPv4 address for %s", strings.Join(pinHosts, ", "))
	}
	route4, ok4, err := defaultRoute("-4", dev)
	if err != nil {
		return err
	}
	if !ok4 {
		return fmt.Errorf("device %s of link %q has no IPv4 default route", dev, link)
	}
	route6, ok6, err := defaultRoute("-6", dev)
	if err != nil {
		return err
	}

	// Remove the rules of an earlier pin: the addresses can change.
	p.stopTimer()
	clearPin()
	cmds := [][]string{routeCmd("-4", dev, route4, true)}
	cmds = append(cmds, routeCmd("-6", dev, route6, ok6))
	for _, a := range v4 {
		cmds = append(cmds, []string{"-4", "rule", "add", "to", a, "lookup", pinTable, "priority", pinPriority})
	}
	for _, a := range v6 {
		cmds = append(cmds, []string{"-6", "rule", "add", "to", a, "lookup", pinTable, "priority", pinPriority})
	}
	for _, c := range cmds {
		if out, err := execCommand("ip", c...); err != nil {
			clearPin()
			p.link = ""
			return fmt.Errorf("ip %s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
		}
	}
	p.link = link
	p.timer = time.AfterFunc(d, func() { p.expire(link) })
	p.logger.Infof("[%s] traffic to %s goes through %s for %v (%s)", link, strings.Join(pinHosts, ", "), dev, d, strings.Join(append(v4, v6...), " "))
	return nil
}

// Unpin removes the rules of the link. With link "", it removes the rules
// of any link. It is not an error if no link has the traffic.
func (p *Pinner) Unpin(link string) error {
	if link != "" {
		if _, ok := p.devices[link]; !ok {
			return fmt.Errorf("unknown link %q", link)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link == "" {
		return nil
	}
	if link != "" && link != p.link {
		return fmt.Errorf("link %q does not have the traffic of the load (link %q has it)", link, p.link)
	}
	p.unpinLocked()
	return nil
}

// Clear removes all rules and routes of the Pinner, also of an earlier
// process (after a crash). The program calls it at the start and at the
// stop.
func (p *Pinner) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTimer()
	clearPin()
	p.link = ""
}

// expire removes the rules at the end of the time of a pin.
func (p *Pinner) expire(link string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link != link || p.timer == nil {
		return
	}
	p.unpinLocked()
}

func (p *Pinner) unpinLocked() {
	p.logger.Infof("[%s] traffic to %s uses the routing of the router again", p.link, strings.Join(pinHosts, ", "))
	p.stopTimer()
	clearPin()
	p.link = ""
}

func (p *Pinner) stopTimer() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// clearPin removes the rules and the routes of pinTable. The errors are
// not important: "ip rule del" fails when no rule is left.
func clearPin() {
	for _, fam := range []string{"-4", "-6"} {
		for range pinMaxRules {
			if _, err := execCommand("ip", fam, "rule", "del", "lookup", pinTable); err != nil {
				break
			}
		}
		_, _ = execCommand("ip", fam, "route", "flush", "table", pinTable)
	}
}

// routeCmd returns the arguments of "ip" for the default route of
// pinTable. Without a route of the device (ok false), the route is
// "unreachable".
func routeCmd(fam, dev string, r pinRoute, ok bool) []string {
	if !ok {
		return []string{fam, "route", "replace", "unreachable", "default", "table", pinTable}
	}
	c := []string{fam, "route", "replace", "default"}
	if r.gateway != "" {
		c = append(c, "via", r.gateway)
	}
	return append(c, "dev", dev, "table", pinTable)
}

// defaultRoute finds the default route of the device in all route tables.
// The main table comes first (netifd adds the default route of each WAN
// interface there, also with mwan3). Then the first other table, but not
// pinTable. ok is false if the device has no default route.
func defaultRoute(fam, dev string) (r pinRoute, ok bool, err error) {
	out, err := execCommand("ip", fam, "route", "show", "table", "all", "default", "dev", dev)
	if err != nil {
		return r, false, fmt.Errorf("ip %s route show: %v: %s", fam, err, strings.TrimSpace(string(out)))
	}
	var other *pinRoute
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Only unicast routes: not "unreachable default ...".
		if len(f) == 0 || f[0] != "default" {
			continue
		}
		var cur pinRoute
		table := "main"
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				cur.gateway = f[i+1]
				// "via inet6 ADDR" (a gateway of another family).
				if (cur.gateway == "inet" || cur.gateway == "inet6") && i+2 < len(f) {
					cur.gateway = f[i+2]
				}
			case "table":
				table = f[i+1]
			}
		}
		switch table {
		case pinTable:
			continue
		case "main":
			return cur, true, nil
		}
		if other == nil {
			other = &cur
		}
	}
	if other != nil {
		return *other, true, nil
	}
	return r, false, nil
}

// errNoPinner is the error of the pin commands without a Pinner.
var errNoPinner = errors.New("no pinning")
