// SPDX-License-Identifier: GPL-2.0

package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeRules is a rule list of "ip -4 rule" at priority 900: the device of
// each rule of a ping interface, and other rules (for example of the
// Pinner) that must stay.
type fakeRules struct {
	oif   []string
	other []string
}

func (f *fakeRules) install(t *testing.T) {
	t.Helper()
	old := execCommand
	execCommand = func(name string, args ...string) ([]byte, error) {
		a := strings.Join(args, " ")
		switch {
		case a == "-4 rule show pref 900":
			var b strings.Builder
			for _, d := range f.oif {
				b.WriteString("900:\tfrom all oif " + d + " lookup main\n")
			}
			for _, o := range f.other {
				b.WriteString("900:\tfrom all to " + o + " lookup 7370\n")
			}
			return []byte(b.String()), nil
		case strings.HasPrefix(a, "-4 rule add pref 900 oif "):
			f.oif = append(f.oif, args[6])
			return nil, nil
		case strings.HasPrefix(a, "-4 rule del pref 900 oif "):
			i := slices.Index(f.oif, args[6])
			if i < 0 {
				return []byte("RTNETLINK answers: No such file or directory"), errors.New("exit status 2")
			}
			f.oif = slices.Delete(f.oif, i, i+1)
			return nil, nil
		}
		t.Fatalf("unexpected command: %s %s", name, a)
		return nil, nil
	}
	t.Cleanup(func() { execCommand = old })
}

func TestPingRules_AddAndRemove(t *testing.T) {
	// Rules of an earlier process: one for a device that is not in the
	// config now, one twice. A rule of the Pinner stays.
	f := &fakeRules{oif: []string{"wwan0", "lan2", "lan2"}, other: []string{"1.2.3.4"}}
	f.install(t)
	links := []LinkConfig{
		{Name: "a", PingInterfaceName: "lan2"},
		{Name: "b", PingInterfaceName: "lan2"}, // same device: one rule
		{Name: "c"},                            // no ping interface: no rule
		{Name: "d", PingInterfaceName: "lan1"},
	}
	devs := AddPingRules(links, testLogger(t))
	if !slices.Equal(devs, []string{"lan2", "lan1"}) || !slices.Equal(f.oif, []string{"lan2", "lan1"}) {
		t.Fatalf("devices %v, rules %v; want one rule each for lan2 and lan1", devs, f.oif)
	}
	RemovePingRules(devs, testLogger(t))
	if len(f.oif) != 0 || len(f.other) != 1 {
		t.Errorf("after the stop: rules %v, other rules %v", f.oif, f.other)
	}
}
