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
	// failDel: "ip rule del" fails while rules exist (a failed removal).
	failDel bool
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
			if f.failDel && f.rules[args[0]] > 0 {
				return []byte("RTNETLINK answers: Operation not permitted"), &exec.ExitError{}
			}
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
// route. The pin uses the IPv4 route of the main table and adds one rule
// for each address. IPv6 is unreachable, also if the link has IPv6: with
// native IPv6 on more than one link, the browser can use a source
// address of another link.
func TestPinner_PinAddsRoutesAndRules(t *testing.T) {
	f := newFakeIP(t, map[string]string{
		"-4": "default via 10.0.0.1 table 1 proto static metric 10\ndefault via 10.0.0.254 proto static metric 10\n",
		"-6": "default via fe80::1 proto static metric 512 pref medium\n",
	}, "162.159.140.220", "172.66.0.218", "2606:4700:7::da")
	p := NewPinner(pinTestLinks(), testLogger(t))
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip -4 route show table all default dev eth1",
		"ip -4 route replace default via 10.0.0.254 dev eth1 table 7370",
		"ip -6 route replace unreachable default table 7370",
		"ip -4 rule add to 162.159.140.220 lookup 7370 priority 900",
		"ip -4 rule add to 172.66.0.218 lookup 7370 priority 900",
		"ip -6 rule add to 2606:4700:7::da lookup 7370 priority 900",
		// Existing connections to the addresses lose their NAT of another
		// link.
		"conntrack -D -d 162.159.140.220",
		"conntrack -D -d 2606:4700:7::da",
	} {
		if !f.has(want) {
			t.Errorf("no call %q in %q", want, f.calls)
		}
	}
	if f.rules["-4"] != 2 || f.rules["-6"] != 1 {
		t.Errorf("rules %v, want 2 IPv4 and 1 IPv6", f.rules)
	}
	if err := p.Unpin(PinOwnerControl, ""); err != nil {
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
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
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
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	err := p.Pin(PinOwnerControl, "wan", time.Minute)
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
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := p.Pin(PinOwnerControl, "lte", time.Minute); err == nil || !strings.Contains(err.Error(), `link "wan" has the traffic`) {
		t.Errorf("pin of a second link: %v", err)
	}
	if err := p.Unpin(PinOwnerControl, "lte"); err == nil {
		t.Error("unpin of a link without the traffic: no error")
	}
	// A second test of the same link (another browser tab) does not share
	// the pin: the end of one test would remove it.
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err == nil {
		t.Error("second pin of the same link: no error")
	}
	if f.rules["-4"] != 1 {
		t.Errorf("rules %v after the second pin, want 1", f.rules)
	}
	if err := p.Unpin(PinOwnerControl, "wan"); err != nil {
		t.Fatal(err)
	}
	if err := p.Pin(PinOwnerControl, "lte", time.Minute); err != nil {
		t.Errorf("pin after unpin: %v", err)
	}
}

// At the end of the time, the rules go away.
func TestPinner_Expires(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", 50*time.Millisecond); err != nil {
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
	// Stop the timer while the fake is installed.
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err == nil {
		t.Fatal("no error")
	}
	if f.rules["-4"] != 0 || p.link != "" {
		t.Errorf("rules %v, link %q after the error, want none", f.rules, p.link)
	}
}

// A calibration session replaces the pin of a bufferbloat test (control
// socket). The control socket cannot replace or remove the pin of the
// session.
func TestPinner_Owners(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := p.Pin(PinOwnerCalibration, "wan", time.Minute); err != nil {
		t.Fatalf("session after control: %v", err)
	}
	if err := p.Unpin(PinOwnerControl, "wan"); err == nil || !strings.Contains(err.Error(), "a calibration") {
		t.Errorf("unpin of the control socket during the session: %v", err)
	}
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err == nil {
		t.Error("pin of the control socket during the session: no error")
	}
	if f.rules["-4"] != 1 || p.owner != PinOwnerCalibration {
		t.Errorf("rules %v, owner %q: the session must keep its pin", f.rules, p.owner)
	}
	if err := p.Unpin(PinOwnerCalibration, "wan"); err != nil || f.rules["-4"] != 0 {
		t.Errorf("unpin of the session: %v, rules %v", err, f.rules)
	}
}

// The timer of a replaced pin fires while the new pin waits for the lock:
// it must not remove the new pin.
func TestPinner_OldTimerKeepsNewPin(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	old := p.gen
	if err := p.Pin(PinOwnerCalibration, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	p.expire(old)
	if p.link != "wan" || f.rules["-4"] != 1 {
		t.Errorf("after the old timer: link %q, rules %v; want the new pin", p.link, f.rules)
	}
}

// A failed setup with a failed rollback keeps the pin: the timer tries the
// removal again.
func TestPinner_FailedRollbackRetries(t *testing.T) {
	old := pinRetry
	pinRetry = 20 * time.Millisecond
	t.Cleanup(func() { pinRetry = old })
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220", "172.66.0.218")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	f.fail, f.failDel = "rule add to 172.66.0.218", true
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err == nil {
		t.Fatal("no error")
	}
	p.mu.Lock()
	link, rules := p.link, f.rules["-4"]
	p.mu.Unlock()
	if link != "wan" || rules != 1 {
		t.Fatalf("after the failed rollback: link %q, rules %d; want the pin to stay", link, rules)
	}
	if err := p.Unpin(PinOwnerControl, "wan"); err == nil {
		t.Error("unpin with a failed removal: no error")
	}
	p.mu.Lock()
	f.failDel = false
	p.mu.Unlock()
	waitPinGone(t, p, f)
}

// A failed Unpin keeps the timer of the pin: at the end of the time, the
// timer tries the removal again.
func TestPinner_FailedUnpinKeepsTimer(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerCalibration, "wan", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.fail = "-4 rule del"
	if err := p.Unpin(PinOwnerCalibration, "wan"); err == nil {
		t.Fatal("no error")
	}
	p.mu.Lock()
	f.fail = ""
	p.mu.Unlock()
	waitPinGone(t, p, f)
}

// Extend moves the end of a pin: the old time does not remove it.
func TestPinner_Extend(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerCalibration, "wan", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := p.Extend(PinOwnerCalibration, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := p.Extend(PinOwnerControl, "wan", time.Minute); err == nil {
		t.Error("extend of another owner: no error")
	}
	time.Sleep(150 * time.Millisecond)
	p.mu.Lock()
	link, rules := p.link, f.rules["-4"]
	p.mu.Unlock()
	if link != "wan" || rules != 1 {
		t.Errorf("after the old time: link %q, rules %d; want the pin", link, rules)
	}
}

// A failed removal at the start (rules of a crashed process) tries again:
// with the timer, and with Unpin.
func TestPinner_FailedClearRetries(t *testing.T) {
	old := pinRetry
	pinRetry = 20 * time.Millisecond
	t.Cleanup(func() { pinRetry = old })
	f := newFakeIP(t, map[string]string{"-4": "", "-6": ""})
	f.rules["-4"] = 2 // rules of the crashed process
	f.failDel = true
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	p.Clear()
	if err := p.Unpin(PinOwnerControl, ""); err == nil {
		t.Error("unpin with a failed removal: no error")
	}
	p.mu.Lock()
	f.failDel = false
	p.mu.Unlock()
	waitPinGone(t, p, f)
	p.mu.Lock()
	pending := p.pending
	p.mu.Unlock()
	if pending {
		t.Error("removal still pending after the retry")
	}
}

// After Close, Pin refuses: a control command that is still open at the
// stop cannot pin again.
func TestPinner_CloseRefusesPin(t *testing.T) {
	newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	p.Close()
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err == nil || !strings.Contains(err.Error(), "stops") {
		t.Errorf("pin after Close: %v", err)
	}
}

// waitPinGone waits until the Pinner has no pin and no rules.
func waitPinGone(t *testing.T, p *Pinner, f *fakeIP) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		link, rules := p.link, f.rules["-4"]
		p.mu.Unlock()
		if link == "" && rules == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("link %q, rules %d; want no pin", link, rules)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A gateway outside the subnet (onlink) and an IPv6 gateway of an IPv4
// route keep their attributes.
func TestPinner_RouteAttributes(t *testing.T) {
	f := newFakeIP(t, map[string]string{
		"-4": "default via inet6 fe80::1 proto static onlink\n",
		"-6": "default via fe80::1 metric 1024 onlink pref medium\n",
	}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip -4 route replace default via inet6 fe80::1 dev eth1 table 7370 onlink",
	} {
		if !f.has(want) {
			t.Errorf("no call %q in %q", want, f.calls)
		}
	}
}

// A failed removal keeps the pin: Unpin reports the error, and the next
// Unpin tries again.
func TestPinner_FailedRemovalKeepsPin(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "wan", time.Minute); err != nil {
		t.Fatal(err)
	}
	f.fail = "-4 rule del"
	if err := p.Unpin(PinOwnerControl, "wan"); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("unpin with a failed removal: %v", err)
	}
	if p.link != "wan" || f.rules["-4"] != 1 {
		t.Errorf("link %q, rules %v; want the pin to stay", p.link, f.rules)
	}
	f.fail = ""
	if err := p.Unpin(PinOwnerControl, "wan"); err != nil || p.link != "" || f.rules["-4"] != 0 {
		t.Errorf("second unpin: %v, link %q, rules %v", err, p.link, f.rules)
	}
}

func TestControl_PinCommands(t *testing.T) {
	f := newFakeIP(t, map[string]string{"-4": "default via 10.0.0.1 proto static\n", "-6": ""}, "162.159.140.220")
	p := NewPinner(pinTestLinks(), testLogger(t))
	t.Cleanup(p.Clear)
	h := &ControlHandler{Names: []string{"wan", "lte"}, Pin: p}
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
		// Only wan1 has IPv6.
		{"-6", "addr", "add", "2001:db8:1::2/64", "dev", "wan1", "nodad"},
		{"-6", "route", "add", "default", "via", "2001:db8:1::1", "dev", "wan1"},
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
	t.Cleanup(p.Clear)
	if err := p.Pin(PinOwnerControl, "b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := ip("-4", "route", "get", "192.0.2.10"); !strings.Contains(got, "dev wan2") {
		t.Errorf("route to the pinned address: %s", got)
	}
	if got := ip("-4", "route", "get", "192.0.2.11"); !strings.Contains(got, "dev wan1") {
		t.Errorf("route to another address: %s", got)
	}
	// IPv6 to the pinned address is unreachable (not through wan1), and
	// another IPv6 address uses wan1.
	if out, err := exec.Command("ip", "-6", "route", "get", "2001:db8::10").CombinedOutput(); err == nil && !strings.Contains(string(out), "unreachable") {
		t.Errorf("IPv6 route to the pinned address: %s", out)
	}
	if got := ip("-6", "route", "get", "2001:db8::11"); !strings.Contains(got, "dev wan1") {
		t.Errorf("IPv6 route to another address: %s", got)
	}
	p.Clear()
	if got := ip("-4", "route", "get", "192.0.2.10"); !strings.Contains(got, "dev wan1") {
		t.Errorf("route to the address after the clear: %s", got)
	}
	if got := ip("-4", "rule", "show"); strings.Contains(got, "7370") {
		t.Errorf("rules after the clear: %s", got)
	}
}
