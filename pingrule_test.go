// SPDX-License-Identifier: GPL-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// fakeRules is the list of the rules of the ping interfaces at priority
// 900, for each address family ("-4 lan2"). noV6 simulates a kernel
// without IPv6.
type fakeRules struct {
	rules []string
	noV6  bool
	// restricted are rules with a destination selector ("-4 lan2"): they
	// are not the rule of a ping interface.
	restricted []string
	// failDel are the rules that "ip rule del" cannot remove.
	failDel []string
}

func (f *fakeRules) install(t *testing.T) {
	t.Helper()
	old, oldState := execCommand, pingRuleState
	pingRuleState = t.TempDir() + "/ping-rules"
	execCommand = func(name string, args ...string) ([]byte, error) {
		fam := args[0]
		if fam == "-6" && f.noV6 {
			return []byte("RTNETLINK answers: Address family not supported by protocol"), errors.New("exit status 2")
		}
		a := strings.Join(args[1:], " ")
		switch {
		case a == "rule show pref 900":
			var b strings.Builder
			for _, r := range f.rules {
				if f := strings.Fields(r); f[0] == fam {
					b.WriteString("900:\tfrom all oif " + f[1] + " lookup main\n")
				}
			}
			for _, r := range f.restricted {
				if f := strings.Fields(r); f[0] == fam {
					b.WriteString("900:\tfrom all to 192.0.2.0/24 oif " + f[1] + " lookup main\n")
				}
			}
			return []byte(b.String()), nil
		case strings.HasPrefix(a, "rule add pref 900 oif "):
			f.rules = append(f.rules, fam+" "+args[6])
			return nil, nil
		case strings.HasPrefix(a, "rule del pref 900 oif "):
			if slices.Contains(f.failDel, fam+" "+args[6]) {
				return []byte("RTNETLINK answers: Operation not permitted"), errors.New("exit status 2")
			}
			i := slices.Index(f.rules, fam+" "+args[6])
			if i < 0 {
				return []byte("RTNETLINK answers: No such file or directory"), errors.New("exit status 2")
			}
			f.rules = slices.Delete(f.rules, i, i+1)
			return nil, nil
		}
		t.Fatalf("unexpected command: %s %v", name, args)
		return nil, nil
	}
	t.Cleanup(func() { execCommand, pingRuleState = old, oldState })
}

func TestPingRules_AddAndRemove(t *testing.T) {
	f := &fakeRules{}
	f.install(t)
	links := []LinkConfig{
		{Name: "a", PingInterfaceName: "lan2"},
		{Name: "b", PingInterfaceName: "lan2"}, // same device: one rule each family
		{Name: "c"},                            // no ping interface: no rule
		{Name: "d", PingInterfaceName: "lan1"},
	}
	rules := AddPingRules(links, testLogger(t))
	want := []string{"-4 lan2", "-6 lan2", "-4 lan1", "-6 lan1"}
	if !slices.Equal(f.rules, want) || len(rules) != 4 {
		t.Fatalf("rules %v (own %v), want %v", f.rules, rules, want)
	}
	if data, _ := os.ReadFile(pingRuleState); string(data) != strings.Join(want, "\n")+"\n" {
		t.Errorf("state file %q", data)
	}
	RemovePingRules(rules, testLogger(t))
	if len(f.rules) != 0 {
		t.Errorf("after the stop: rules %v", f.rules)
	}
	if _, err := os.Stat(pingRuleState); !os.IsNotExist(err) {
		t.Errorf("the state file stays after the stop: %v", err)
	}
}

// The program removes only its own rules: a rule of the administrator
// stays, also with the same device.
func TestPingRules_KeepOtherRules(t *testing.T) {
	f := &fakeRules{rules: []string{"-4 lan2", "-4 wwan0"}}
	f.install(t)
	rules := AddPingRules([]LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}, testLogger(t))
	if len(rules) != 1 || rules[0] != (pingRule{"-6", "lan2"}) {
		t.Fatalf("own rules %v, want only -6 lan2 (-4 lan2 is already there)", rules)
	}
	RemovePingRules(rules, testLogger(t))
	if !slices.Equal(f.rules, []string{"-4 lan2", "-4 wwan0"}) {
		t.Errorf("rules after the stop %v, want the two rules of the administrator", f.rules)
	}
}

// After a crash, the next start removes the rules in the state file, and
// only those.
func TestPingRules_RemoveStale(t *testing.T) {
	f := &fakeRules{rules: []string{"-4 lan9", "-4 wwan0"}, noV6: true}
	f.install(t)
	if err := os.WriteFile(pingRuleState, []byte("-4 lan9\nbad line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := AddPingRules([]LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}, testLogger(t))
	if !slices.Equal(f.rules, []string{"-4 wwan0", "-4 lan2"}) || len(rules) != 1 {
		t.Errorf("rules %v (own %v): the stale rule must go, the other must stay, no IPv6 rule", f.rules, rules)
	}
}

// A rule with other selectors (here "to 192.0.2.0/24") does not cover all
// reflectors: the program adds its own rule.
func TestPingRules_RestrictedRuleIsNotEnough(t *testing.T) {
	f := &fakeRules{restricted: []string{"-4 lan2"}, noV6: true}
	f.install(t)
	rules := AddPingRules([]LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}, testLogger(t))
	if len(rules) != 1 || !slices.Equal(f.rules, []string{"-4 lan2"}) {
		t.Errorf("own rules %v, rules %v: want the program's own rule for lan2", rules, f.rules)
	}
}

// A stale rule that the start cannot remove stays a rule of the program:
// the stop tries again, and the state file keeps it until then.
func TestPingRules_FailedStaleRemovalStaysOwned(t *testing.T) {
	f := &fakeRules{rules: []string{"-4 lan9"}, noV6: true, failDel: []string{"-4 lan9"}}
	f.install(t)
	if err := os.WriteFile(pingRuleState, []byte("-4 lan9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := AddPingRules([]LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}, testLogger(t))
	if !slices.Contains(rules, pingRule{"-4", "lan9"}) {
		t.Fatalf("own rules %v: the stale rule must stay owned", rules)
	}
	if data, _ := os.ReadFile(pingRuleState); !strings.Contains(string(data), "-4 lan9") {
		t.Errorf("state file %q lost the stale rule", data)
	}
	f.failDel = nil
	RemovePingRules(rules, testLogger(t))
	if len(f.rules) != 0 {
		t.Errorf("after the stop: rules %v", f.rules)
	}
	if _, err := os.Stat(pingRuleState + ".new"); !os.IsNotExist(err) {
		t.Errorf("the temporary state file stays: %v", err)
	}
}

// Without the record in the state file, the program adds no rule: after a
// crash, the next start could not know that the rule is of the program.
func TestPingRules_NoRuleWithoutState(t *testing.T) {
	f := &fakeRules{noV6: true}
	f.install(t)
	pingRuleState = t.TempDir() + "/no/such/dir/ping-rules"
	rules := AddPingRules([]LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}, testLogger(t))
	if len(rules) != 0 || len(f.rules) != 0 {
		t.Errorf("own rules %v, rules %v: want no rule without the state file", rules, f.rules)
	}
}

// The program reads and writes only a state file that no other user can
// change: else a file of another user could make it remove rules that it
// did not add.
func TestPingRules_UnsafeStateIsRefused(t *testing.T) {
	f := &fakeRules{rules: []string{"-4 wan0"}, noV6: true}
	f.install(t)
	links := []LinkConfig{{Name: "a", PingInterfaceName: "lan2"}}

	// A directory that others can write.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	pingRuleState = dir + "/ping-rules"
	if err := os.WriteFile(pingRuleState, []byte("-4 wan0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rules := AddPingRules(links, testLogger(t)); len(rules) != 0 || !slices.Equal(f.rules, []string{"-4 wan0"}) {
		t.Errorf("writable directory: own %v, rules %v; want no change", rules, f.rules)
	}

	// A symbolic link as the state file.
	dir = t.TempDir()
	target := t.TempDir() + "/other"
	if err := os.WriteFile(target, []byte("-4 wan0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pingRuleState = dir + "/ping-rules"
	if err := os.Symlink(target, pingRuleState); err != nil {
		t.Fatal(err)
	}
	if rules := AddPingRules(links, testLogger(t)); len(rules) != 0 || !slices.Equal(f.rules, []string{"-4 wan0"}) {
		t.Errorf("symbolic link: own %v, rules %v; want no change", rules, f.rules)
	}
}

func TestTrustedPath(t *testing.T) {
	if p, err := trustedPath("sh"); err != nil || !strings.HasSuffix(p, "/sh") || !strings.HasPrefix(p, "/") {
		t.Errorf("sh: %q %v", p, err)
	}
	if _, err := trustedPath("no-such-command-cav"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("missing command: %v, want exec.ErrNotFound", err)
	}
}
