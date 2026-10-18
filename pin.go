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
	"os/exec"
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
//   - a route table (pinTable) with the IPv4 default route of the link.
//     The IPv6 default route of the table is "unreachable": the browser
//     then uses IPv4. With native IPv6 on more than one WAN link, the
//     browser can use a source address of another link: the provider of
//     the pinned link can refuse these packets.
//   - one "ip rule" for each address (to ADDR lookup pinTable), with the
//     priority pinPriority. mwan3 uses priorities from 1001: the rules of
//     the Pinner come first.

// pinTable is the route table of the Pinner.
const pinTable = "7370"

// pinPriority is the priority of the rules of the Pinner.
const pinPriority = "900"

// maxPinSeconds is the maximum time of "pin LINK SECONDS".
const maxPinSeconds = 3600

// pinRetry is the time until the next try of a failed removal at the end
// of a pin. Tests change it.
var pinRetry = 10 * time.Second

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

// The owners of a pin. A calibration session replaces a pin of the control
// socket (for example of a bufferbloat test). The control socket cannot
// replace or remove the pin of a session: else the load of the session
// could use another link.
const (
	PinOwnerControl     = "control"
	PinOwnerCalibration = "calibration"
)

// Pinner sends the traffic to pinHosts through one link (see above). Only
// one link at a time can have the traffic.
type Pinner struct {
	devices map[string]string // link name: device of the upload
	logger  *Logger

	mu    sync.Mutex
	link  string // the link with the traffic, "" if none
	owner string // the owner of the pin
	timer *time.Timer
	// gen identifies the pin: a timer of an earlier pin can fire while a
	// new pin waits for the lock. Then it must not remove the new pin.
	gen int
	// closed is true after Close: Pin refuses.
	closed bool
	// pending is true after a failed removal without a pin (Clear): a
	// timer tries again, and Unpin tries again.
	pending bool
	// addrs are the addresses of the pin.
	addrs []string
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
// point-to-point device (for example PPPoE). family is "inet6" for an IPv4
// route with an IPv6 gateway ("via inet6 ADDR"), else "". onlink is true
// for a gateway outside the subnets of the device.
type pinRoute struct {
	gateway string
	family  string
	onlink  bool
}

// Pin sends the traffic to pinHosts through the link for the time d. A
// calibration session replaces a pin of the control socket. Else Pin
// refuses while a pin exists: two tests (for example in two browser tabs)
// must not share a pin, because the end of one test removes it.
func (p *Pinner) Pin(owner, link string, d time.Duration) error {
	dev, ok := p.devices[link]
	if !ok {
		return fmt.Errorf("unknown link %q", link)
	}
	if dev == "" {
		return fmt.Errorf("link %q has no upload interface", link)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("the service stops")
	}
	replace := owner == PinOwnerCalibration && p.owner == PinOwnerControl
	if p.link != "" && !replace {
		if p.owner == PinOwnerCalibration {
			return fmt.Errorf("a calibration of link %q has the traffic of the load", p.link)
		}
		return fmt.Errorf("a test of link %q has the traffic of the load", p.link)
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

	// Remove the rules of a replaced pin (or of an earlier process). If
	// that fails, the replaced pin stays with its timer.
	if err := clearPin(); err != nil {
		return err
	}
	p.stopTimer()
	// From here, the Pinner tracks the rules: a failed removal tries
	// again.
	p.link, p.owner = link, owner
	p.gen++
	gen := p.gen
	cmds := [][]string{routeCmd("-4", dev, route4, true), routeCmd("-6", dev, pinRoute{}, false)}
	for _, a := range v4 {
		cmds = append(cmds, []string{"-4", "rule", "add", "to", a, "lookup", pinTable, "priority", pinPriority})
	}
	for _, a := range v6 {
		cmds = append(cmds, []string{"-6", "rule", "add", "to", a, "lookup", pinTable, "priority", pinPriority})
	}
	noIPv6 := false
	for _, c := range cmds {
		if noIPv6 && c[0] == "-6" {
			continue
		}
		if out, err := execCommand("ip", c...); err != nil {
			// A kernel without IPv6 (ipv6.disable=1): the browser can only
			// use IPv4.
			if c[0] == "-6" && pinNoFamily(out) {
				noIPv6 = true
				continue
			}
			err = fmt.Errorf("ip %s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
			if uerr := p.unpinLocked(); uerr != nil {
				p.logger.Errorf("[%s] %v (next try in %v)", link, uerr, pinRetry)
			}
			return err
		}
	}
	p.pending = false
	p.addrs = append(v4, v6...)
	p.timer = time.AfterFunc(d, func() { p.expire(gen) })
	p.logger.Infof("[%s] traffic to %s goes through %s for %v (%s)", link, strings.Join(pinHosts, ", "), dev, d, strings.Join(p.addrs, " "))
	p.flushConntrack()
	return nil
}

// Extend sets the time of the pin of the owner and the link to d from now.
// A calibration session uses it when its end moves.
func (p *Pinner) Extend(owner, link string, d time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link != link || p.owner != owner {
		return fmt.Errorf("link %q has no pin of the %s", link, owner)
	}
	// A new generation: the callback of the old timer can have started
	// already, and Stop cannot cancel it.
	p.stopTimer()
	p.gen++
	gen := p.gen
	p.timer = time.AfterFunc(d, func() { p.expire(gen) })
	return nil
}

// flushConntrack removes the connection tracking entries of the addresses
// of the pin. An existing connection keeps the NAT of the link of its
// start: after a change of the route, its packets go through the new link
// with the address of the old link, and the provider can refuse them.
// Without the tool conntrack (package conntrack), the entries stay: the
// browser then opens a new connection after a timeout.
func (p *Pinner) flushConntrack() {
	for _, a := range p.addrs {
		out, err := execCommand("conntrack", "-D", "-d", a)
		if errors.Is(err, exec.ErrNotFound) {
			p.logger.Infof("no conntrack tool: existing connections to %s keep their link", strings.Join(pinHosts, ", "))
			return
		}
		// conntrack fails when it finds no entry: not important.
		_ = out
	}
}

// Unpin removes the rules of the link. With link "", it removes the rules
// of any link. It is not an error if no link has the traffic. Only the
// owner of the pin can remove it.
func (p *Pinner) Unpin(owner, link string) error {
	if link != "" {
		if _, ok := p.devices[link]; !ok {
			return fmt.Errorf("unknown link %q", link)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link == "" {
		if p.pending {
			if err := clearPin(); err != nil {
				return err
			}
			p.clearDone()
		}
		return nil
	}
	if link != "" && link != p.link {
		return fmt.Errorf("link %q does not have the traffic of the load (link %q has it)", link, p.link)
	}
	if owner != p.owner {
		// The control socket does not remove the pin of a session. A
		// session does not remove a pin that replaced its own.
		if owner == PinOwnerControl {
			return fmt.Errorf("a calibration of link %q has the traffic of the load", p.link)
		}
		return nil
	}
	return p.unpinLocked()
}

// Clear removes all rules and routes of the Pinner, also of an earlier
// process (after a crash). The program calls it at the start.
func (p *Pinner) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTimer()
	p.link, p.owner = "", ""
	if err := clearPin(); err != nil {
		// Rules can stay: try again later (also at the next Unpin).
		p.logger.Errorf("%v (next try in %v)", err, pinRetry)
		p.pending = true
		p.timer = time.AfterFunc(pinRetry, p.retryClear)
		return
	}
	p.clearDone()
}

// clearDone ends a removal without a pin: the connections to the
// addresses of the last pin (if any) lose their NAT of the pinned link.
// While the removal fails, the addresses stay for the next try.
func (p *Pinner) clearDone() {
	p.pending = false
	p.flushConntrack()
	p.addrs = nil
}

// retryClear tries again a failed removal of Clear.
func (p *Pinner) retryClear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pending || p.link != "" {
		return
	}
	p.timer = nil
	if err := clearPin(); err != nil {
		p.logger.Errorf("%v (next try in %v)", err, pinRetry)
		p.timer = time.AfterFunc(pinRetry, p.retryClear)
		return
	}
	p.clearDone()
}

// Close removes all rules and routes, and Pin refuses after it. The
// program calls it at the stop: a control command that is still open
// cannot pin again after the removal.
func (p *Pinner) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.Clear()
}

// expire removes the rules at the end of the time of the pin gen.
func (p *Pinner) expire(gen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link == "" || p.gen != gen {
		return
	}
	// This timer fired: a failed removal starts a new one.
	p.timer = nil
	if err := p.unpinLocked(); err != nil {
		p.logger.Errorf("[%s] %v (next try in %v)", p.link, err, pinRetry)
	}
}

// unpinLocked removes the rules of the pin. If that fails, the pin stays
// with its timer (or a new timer of pinRetry): a next Unpin or the timer
// tries again.
func (p *Pinner) unpinLocked() error {
	if err := clearPin(); err != nil {
		if p.timer == nil {
			gen := p.gen
			p.timer = time.AfterFunc(pinRetry, func() { p.expire(gen) })
		}
		return err
	}
	p.stopTimer()
	p.logger.Infof("[%s] traffic to %s uses the routing of the router again", p.link, strings.Join(pinHosts, ", "))
	p.flushConntrack()
	p.link, p.owner, p.addrs = "", "", nil
	return nil
}

func (p *Pinner) stopTimer() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// clearPin removes the rules and the routes of pinTable. "ip rule del"
// fails when no rule is left, and "ip route flush" fails when the table
// does not exist: these errors are the end. Other errors are failures.
func clearPin() error {
	var errs []error
	for _, fam := range []string{"-4", "-6"} {
		for i := 0; ; i++ {
			out, err := execCommand("ip", fam, "rule", "del", "lookup", pinTable)
			if err != nil {
				if !pinAbsent(out) && !pinNoFamily(out) {
					errs = append(errs, fmt.Errorf("ip %s rule del: %v: %s", fam, err, strings.TrimSpace(string(out))))
				}
				break
			}
			if i >= pinMaxRules {
				errs = append(errs, fmt.Errorf("ip %s rule del: more than %d rules of table %s", fam, pinMaxRules, pinTable))
				break
			}
		}
		if out, err := execCommand("ip", fam, "route", "flush", "table", pinTable); err != nil && !pinAbsent(out) && !pinNoFamily(out) {
			errs = append(errs, fmt.Errorf("ip %s route flush: %v: %s", fam, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// pinNoFamily reports whether the output of "ip" tells that the kernel
// does not support the address family (for example IPv6 with
// ipv6.disable=1).
func pinNoFamily(out []byte) bool {
	return strings.Contains(string(out), "Address family not supported")
}

// pinAbsent reports whether the output of "ip" tells that the rule or the
// table does not exist.
func pinAbsent(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "No such file or directory") || strings.Contains(s, "does not exist")
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
		c = append(c, "via")
		if r.family != "" {
			c = append(c, r.family)
		}
		c = append(c, r.gateway)
	}
	c = append(c, "dev", dev, "table", pinTable)
	if r.onlink {
		c = append(c, "onlink")
	}
	return c
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
		for i := 1; i < len(f); i++ {
			switch {
			case f[i] == "onlink":
				cur.onlink = true
			case f[i] == "via" && i+1 < len(f):
				cur.gateway = f[i+1]
				// "via inet6 ADDR" (a gateway of another family).
				if (cur.gateway == "inet" || cur.gateway == "inet6") && i+2 < len(f) {
					cur.family, cur.gateway = f[i+1], f[i+2]
				}
			case f[i] == "table" && i+1 < len(f):
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
