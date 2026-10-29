// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// The pingers bind their sockets to the ping interface (SO_BINDTODEVICE).
// With policy routing, that is not sufficient. mwan3 marks the packets of
// the router for the link of its policy. The route table of that link has
// no route through the ping interface, and the next rule of mwan3
// ("unreachable") drops the packets. Thus, each ping interface gets one
// rule for each address family before the rules of mwan3 (priority 1001
// and higher): the packets of a socket bound to the interface use the main
// table, which has the default routes of all links. A socket that is not
// bound (for example the traffic of the LAN) does not match the rule.
//
// The program removes only the rules that it added. It writes them to
// pingRuleState: after a crash, the next start removes them. If the same
// rule is already there (for example from the administrator), the program
// uses it and does not add or remove it.

// pingRulePriority is the priority of the rules of the ping interfaces.
const pingRulePriority = "900"

// pingRuleState is the file with the rules that the program added. A
// program that is not root (with CAP_NET_ADMIN) cannot write to /var/run:
// it uses its runtime directory (XDG_RUNTIME_DIR), else a directory of its
// own in the temporary directory. Tests change it.
var pingRuleState = func() string {
	uid := os.Geteuid()
	if uid == 0 {
		return "/var/run/cake-autorate-go.ping-rules"
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("cake-autorate-go-%d", uid))
	}
	return filepath.Join(dir, "cake-autorate-go.ping-rules")
}()

// checkPingRuleState makes sure that only this user can change the state
// file. The program removes the rules in the file with CAP_NET_ADMIN: a
// file of another user could remove rules that the program did not add.
// The directory must be of this user (or root) and not writable for
// others, and the file must be a regular file of this user. A program that
// is not root makes its directory (mode 0700) if it is not there.
func checkPingRuleState() error {
	uid := os.Geteuid()
	dir := filepath.Dir(pingRuleState)
	if uid != 0 {
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.IsDir() || (int(st.Uid) != uid && st.Uid != 0) || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a directory of user %d or root, not writable for others", dir, uid)
	}
	fi, err = os.Lstat(pingRuleState)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.Mode().IsRegular() || int(st.Uid) != uid || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a regular file of user %d, not writable for others", pingRuleState, uid)
	}
	return nil
}

// pingRule is a rule of a ping interface: the address family ("-4" or
// "-6") and the device.
type pingRule struct {
	fam, dev string
}

func (r pingRule) args(op string) []string {
	return []string{r.fam, "rule", op, "pref", pingRulePriority, "oif", r.dev, "lookup", "main"}
}

func (r pingRule) String() string { return r.fam + " " + r.dev }

// AddPingRules adds the rules of the ping interfaces of the links. First,
// it removes the rules of an earlier process. It returns the rules that it
// added. An error does not stop the program: without policy routing, the
// pings operate also without the rules.
func AddPingRules(links []LinkConfig, logger *Logger) []pingRule {
	if err := checkPingRuleState(); err != nil {
		logger.Errorf("no routing rules for the pings (with mwan3, the pings can go through another link): %v", err)
		return nil
	}
	// A stale rule that stays (failed removal) is still a rule of the
	// program: the stop and the next start try again.
	own := removeStalePingRules(logger)
	var devs []string
	for _, l := range links {
		if d := l.PingInterfaceName; d != "" && !slices.Contains(devs, d) {
			devs = append(devs, d)
		}
	}
	for _, dev := range devs {
		for _, fam := range []string{"-4", "-6"} {
			r := pingRule{fam, dev}
			if slices.Contains(own, r) || pingRuleExists(r) {
				continue
			}
			// Write the rule before it exists: after a crash in between,
			// the next start removes it. Without the record, the next start
			// could not know that the rule is of the program.
			if err := writePingRuleState(append(own, r), logger); err != nil {
				logger.Errorf("no routing rule for the pings through %s (with mwan3, the pings can go through another link): %v", dev, err)
				continue
			}
			if out, err := execCommand("ip", r.args("add")...); err != nil {
				if !pinNoFamily(out) {
					logger.Errorf("no routing rule for the pings through %s (with mwan3, the pings can go through another link): ip %s rule add: %v: %s",
						dev, fam, err, strings.TrimSpace(string(out)))
				}
				continue
			}
			own = append(own, r)
		}
	}
	if err := writePingRuleState(own, logger); err != nil {
		logger.Errorf("ping rules: %v", err)
	}
	return own
}

// RemovePingRules removes the rules that AddPingRules added. The state file
// keeps the rules that stay.
func RemovePingRules(rules []pingRule, logger *Logger) {
	if err := writePingRuleState(removePingRules(rules, logger), logger); err != nil {
		logger.Errorf("ping rules: %v", err)
	}
}

// removePingRules removes the rules and returns the rules that stay.
func removePingRules(rules []pingRule, logger *Logger) []pingRule {
	var left []pingRule
	for _, r := range rules {
		if err := removePingRule(r); err != nil {
			logger.Errorf("%v (remove it with \"ip %s rule del pref %s oif %s lookup main\")", err, r.fam, pingRulePriority, r.dev)
			left = append(left, r)
		}
	}
	return left
}

// removeStalePingRules removes the rules in pingRuleState (of an earlier
// process). It returns the rules that stay.
func removeStalePingRules(logger *Logger) []pingRule {
	data, err := os.ReadFile(pingRuleState)
	if err != nil {
		return nil
	}
	var stale []pingRule
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && (f[0] == "-4" || f[0] == "-6") && validateIfname(f[1]) == nil {
			stale = append(stale, pingRule{f[0], f[1]})
		}
	}
	return removePingRules(stale, logger)
}

// removePingRule removes one rule (also if the same rule is there two
// times: one of them is not of the program).
func removePingRule(r pingRule) error {
	out, err := execCommand("ip", r.args("del")...)
	if err != nil && !pinAbsent(out) && !pinNoFamily(out) {
		return fmt.Errorf("ip %s rule del for the pings through %s: %v: %s", r.fam, r.dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pingRuleExists tells if the rule is already there, without other
// selectors (a rule with "to SUBNET" does not cover all reflectors).
func pingRuleExists(r pingRule) bool {
	out, err := execCommand("ip", r.fam, "rule", "show", "pref", pingRulePriority)
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`(?m)^` + pingRulePriority + `:\s+from all oif ` + regexp.QuoteMeta(r.dev) + ` lookup main\s*$`)
	return re.Match(out)
}

// writePingRuleState writes the rules to pingRuleState. Without rules, it
// removes the file.
func writePingRuleState(rules []pingRule, logger *Logger) error {
	if len(rules) == 0 {
		if err := os.Remove(pingRuleState); err != nil && !os.IsNotExist(err) {
			logger.Debugf("ping rules: %v", err)
		}
		return nil
	}
	var b strings.Builder
	for _, r := range rules {
		b.WriteString(r.String() + "\n")
	}
	// Write a new file and rename it: a crash during the write must not
	// lose the earlier list.
	tmp := pingRuleState + ".new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("writing the list of the rules: %w", err)
	}
	if err := os.Rename(tmp, pingRuleState); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing the list of the rules: %w", err)
	}
	return nil
}
