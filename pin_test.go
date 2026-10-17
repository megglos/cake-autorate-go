// SPDX-License-Identifier: GPL-2.0

package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeIP replaces execCommand and lookupPinHost. routes has the output of
// "ip -4|-6 route show ..." for each family. The rules that "ip rule del"
// can remove are counted.
type fakeIP struct {
	routes map[string]string
	calls  []string
	rules  map[string]int // family: rules of pinTable
	fail   string         // a call with this text fails
}

func newFakeIP(t *testing.T, routes map[string]string, addrs ...string) *fakeIP {
	t.Helper()
	f := &fakeIP{routes: routes, rules: map[string]int{}}
	oldExec, oldLookup := execCommand, lookupPinHost
	t.Cleanup(func() { execCommand, lookupPinHost = oldExec, oldLookup })
	execCommand = func(name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		f.calls = append(f.calls, call)
		if f.fail != "" && strings.Contains(call, f.fail) {
			return []byte("RTNETLINK answers: Operation not permitted"), &exec.ExitError{}
		}
		switch {
		case slices.Contains(args, "show"):
			return []byte(f.routes[args[0]]), nil
		case args[1] == "rule" && args[2] == "add":
			f.rules[args[0]]++
		case args[1] == "rule" && args[2] == "del":
			if f.rules[args[0]] == 0 {
				return []byte("RTNETLINK answers: No such file or directory"), &exec.ExitError{}
			}
			f.rules[args[0]]--
		}
		return nil, nil
	}
	lookupPinHost = func(ctx context.Context, host string) ([]net.IP, error) {
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
	return f
}

// has reports whether a call is in the list.
func (f *fakeIP) has(call string) bool { return slices.Contains(f.calls, call) }

func pinTestLinks() []LinkConfig {
	return []LinkConfig{
		{Name: "wan", Upload: DirectionConfig{Interface: "eth1"}},
		{Name: "lte", Upload: DirectionConfig{Interface: "wwan0"}},
	}
}

// With mwan3, the main table and the table of mwan3 have the default
// route. The pin uses the route of the main table, for IPv4 and IPv6, and
// adds one rule for each address.
func TestPinner_PinAddsRoutesAndRules(t *testing.T) {
	f := newFakeIP(t, map[string]string{
		"-4": "default via 10.0.0.1 table 1 proto static metric 10\ndefault via 10.0.0.254 proto static metric 10\n",
		"-6": "default via fe80::1 proto static metric 512 pref medium\n",
	}, "162.159.140.220", "172.66.0.218", "2606:4700:7::da")
	p := NewPinner(pinTestLinks(), testLogger(t))
	if err := p.Pin("wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip -4 route show table all default dev eth1",
		"ip -4 route replace default via 10.0.0.254 dev eth1 table 7370",
		"ip -6 route replace default via fe80::1 dev eth1 table 7370",
		"ip -4 rule add to 162.159.140.220 lookup 7370 priority 900",
		"ip -4 rule add to 172.66.0.218 lookup 7370 priority 900",
		"ip -6 rule add to 2606:4700:7::da lookup 7370 priority 900",
	} {
		if !f.has(want) {
			t.Errorf("no call %q in %q", want, f.calls)
		}
	}
	if f.rules["-4"] != 2 || f.rules["-6"] != 1 {
		t.Errorf("rules %v, want 2 IPv4 and 1 IPv6", f.rules)
	}
	if err := p.Unpin(""); err != nil {
		t.Fatal(err)
	}
	if f.rules["-4"] != 0 || f.rules["-6"] != 0 || !f.has("ip -6 route flush table 7370") {
		t.Errorf("after unpin: rules %v, calls %q", f.rules, f.calls)
	}
}

// A link without IPv6 gets an unreachable IPv6 route: the browser then
// uses IPv4, not another link. A point-to-point device (PPPoE) has no
// gateway. The table of the Pinner is not a route of the device.
func TestPinner_NoIPv6AndNoGateway(t *testing.T) {
	f := newFakeIP(t, map[string]string{
		"-4": "default dev eth1 table 7370 scope link\ndefault dev eth1 proto static scope link metric 10\n",
		"-6": "unreachable default table 7370 metric 1024 pref medium\n",
	}, "162.159.140.220", "2606:4700:7::da")
	p := NewPinner(pinTestLinks(), testLogger(t))
	if err := p.Pin("wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip -4 route replace default dev eth1 table 7370",
		"ip -6 route replace unreachable default table 7370",
		"ip -6 rule add to 2606:4700:7::da lookup 7370 priority 900",
	} {
		if !f.has(want) {
			t.Errorf("no call %q in %q", want, f.calls)
		}
	}
}

// Without an IPv4 default route of the device, the pin fails and adds no
// rules.
func TestPinner_NoIPv4Route(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	err := p.Pin("wan", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "no IPv4 default route") {
		t.Errorf("error %v, want no IPv4 default route", err)
	}
	if f.rules["-4"] != 0 {
		t.Errorf("rules %v, want none", f.rules)
	}
}

// Only one link can have the traffic. The same link can pin again.
func TestPinner_OneLinkAtATime(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 dev eth1\n", "-6": ""}, "162.159.140.220")
	f.routes["-4"] = "default via 10.0.0.1 proto static\n"
	p := NewPinner(pinTestLinks(), testLogger(t))
	if err := p.Pin("wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := p.Pin("lte", time.Minute); err == nil || !strings.Contains(err.Error(), `link "wan" has the traffic`) {
		t.Errorf("pin of a second link: %v", err)
	}
	if err := p.Unpin("lte"); err == nil {
		t.Error("unpin of a link without the traffic: no error")
	}
	if err := p.Pin("wan", time.Minute); err != nil {
		t.Errorf("second pin of the same link: %v", err)
	}
	if f.rules["-4"] != 1 {
		t.Errorf("rules %v after the second pin, want 1 (the old rule is removed)", f.rules)
	}
	if err := p.Unpin("wan"); err != nil {
		t.Fatal(err)
	}
	if err := p.Pin("lte", time.Minute); err != nil {
		t.Errorf("pin after unpin: %v", err)
	}
}

// At the end of the time, the rules go away.
func TestPinner_Expires(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	if err := p.Pin("wan", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		link, rules := p.link, f.rules["-4"]
		p.mu.Unlock()
		if link == "" && rules == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the time: link %q, rules %d", link, rules)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A failed rule removes the rules that were added.
func TestPinner_FailedRuleRemovesAll(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220", "172.66.0.218")
	f.fail = "rule add to 172.66.0.218"
	p := NewPinner(pinTestLinks(), testLogger(t))
	if err := p.Pin("wan", time.Minute); err == nil {
		t.Fatal("no error")
	}
	if f.rules["-4"] != 0 || p.link != "" {
		t.Errorf("rules %v, link %q after the error, want none", f.rules, p.link)
	}
}

func TestControl_PinCommands(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	h := &ControlHandler{Names: []string{"wan", "lte"}, Pin: NewPinner(pinTestLinks(), testLogger(t))}
	for _, c := range []struct{ cmd, want string }{
		{"pin wan", "error: usage: pin LINK SECONDS"},
		{"pin wan 0", "error: SECONDS must be 1 to 3600"},
		{"pin wan 3601", "error: SECONDS must be 1 to 3600"},
		{"pin wan x", "error: SECONDS must be 1 to 3600"},
		{"pin dsl 60", `error: unknown link "dsl"`},
		{"unpin wan lte", "error: usage: unpin [LINK]"},
		{"unpin", "ok"},
		{"pin wan 60", "ok"},
		{"unpin wan", "ok"},
	} {
		if got := handleControlCommand(c.cmd, h); got != c.want {
			t.Errorf("%q: %q, want %q", c.cmd, got, c.want)
		}
	}
	if f.rules["-4"] != 0 {
		t.Errorf("rules %v at the end, want none", f.rules)
	}
	if got := handleControlCommand("pin wan 60", &ControlHandler{}); got != "error: no pinning" {
		t.Errorf("without Pinner: %q", got)
	}
}

// The Pinner with the kernel, in a network namespace. Run as root in a new
// namespace, for example:
//
//	CAKE_PIN_NETNS=1 unshare -rn go test -run TestPinner_Kernel .
//
// Two links (two dummy devices with a default route each, in the main
// table). The route to a pinned address uses the device of the pinned
// link, the route to another address uses the main table.
func TestPinner_Kernel(t *testing.T) {
	if os.Getenv("CAKE_PIN_NETNS") == "" {
		t.Skip("set CAKE_PIN_NETNS=1 and run as root in a new network namespace")
	}
	ip := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	for _, c := range [][]string{
		{"link", "add", "wan1", "type", "dummy"},
		{"link", "add", "wan2", "type", "dummy"},
		{"link", "set", "wan1", "up"},
		{"link", "set", "wan2", "up"},
		{"addr", "add", "10.1.0.2/24", "dev", "wan1"},
		{"addr", "add", "10.2.0.2/24", "dev", "wan2"},
		{"route", "add", "default", "via", "10.1.0.1", "dev", "wan1", "metric", "10"},
		{"route", "add", "default", "via", "10.2.0.1", "dev", "wan2", "metric", "20"},
	} {
		ip(c...)
	}
	oldLookup := lookupPinHost
	t.Cleanup(func() { lookupPinHost = oldLookup })
	lookupPinHost = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")}, nil
	}
	links := []LinkConfig{
		{Name: "a", Upload: DirectionConfig{Interface: "wan1"}},
		{Name: "b", Upload: DirectionConfig{Interface: "wan2"}},
	}
	p := NewPinner(links, testLogger(t))
	if err := p.Pin("b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := ip("-4", "route", "get", "192.0.2.10"); !strings.Contains(got, "dev wan2") {
		t.Errorf("route to the pinned address: %s", got)
	}
	if got := ip("-4", "route", "get", "192.0.2.11"); !strings.Contains(got, "dev wan1") {
		t.Errorf("route to another address: %s", got)
	}
	// wan2 has no IPv6 route: the IPv6 address is unreachable.
	if out, err := exec.Command("ip", "-6", "route", "get", "2001:db8::10").CombinedOutput(); err == nil && !strings.Contains(string(out), "unreachable") {
		t.Errorf("IPv6 route to the pinned address: %s", out)
	}
	p.Clear()
	if got := ip("-4", "route", "get", "192.0.2.10"); !strings.Contains(got, "dev wan1") {
		t.Errorf("route to the address after the clear: %s", got)
	}
	if got := ip("-4", "rule", "show"); strings.Contains(got, "7370") {
		t.Errorf("rules after the clear: %s", got)
	}
}
