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
	"regexp"
	"slices"
	"strings"
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

// pingRuleState is the file with the rules that the program added. Tests
// change it.
var pingRuleState = "/var/run/cake-autorate-go.ping-rules"

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
	removeStalePingRules(logger)
	var devs []string
	for _, l := range links {
		if d := l.PingInterfaceName; d != "" && !slices.Contains(devs, d) {
			devs = append(devs, d)
		}
	}
	var own []pingRule
	for _, dev := range devs {
		for _, fam := range []string{"-4", "-6"} {
			r := pingRule{fam, dev}
			if pingRuleExists(r) {
				continue
			}
			// Write the rule before it exists: after a crash in between,
			// the next start removes it.
			writePingRuleState(append(own, r), logger)
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
	writePingRuleState(own, logger)
	return own
}

// RemovePingRules removes the rules that AddPingRules added.
func RemovePingRules(rules []pingRule, logger *Logger) {
	var left []pingRule
	for _, r := range rules {
		if err := removePingRule(r); err != nil {
			logger.Errorf("%v (remove it with \"ip %s rule del pref %s oif %s lookup main\")", err, r.fam, pingRulePriority, r.dev)
			left = append(left, r)
		}
	}
	writePingRuleState(left, logger)
}

// removeStalePingRules removes the rules in pingRuleState (of an earlier
// process).
func removeStalePingRules(logger *Logger) {
	data, err := os.ReadFile(pingRuleState)
	if err != nil {
		return
	}
	var stale []pingRule
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && (f[0] == "-4" || f[0] == "-6") && validateIfname(f[1]) == nil {
			stale = append(stale, pingRule{f[0], f[1]})
		}
	}
	RemovePingRules(stale, logger)
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

// pingRuleExists tells if the rule is already there.
func pingRuleExists(r pingRule) bool {
	out, err := execCommand("ip", r.fam, "rule", "show", "pref", pingRulePriority)
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`\boif ` + regexp.QuoteMeta(r.dev) + ` lookup main\b`)
	return re.Match(out)
}

// writePingRuleState writes the rules to pingRuleState. Without rules, it
// removes the file.
func writePingRuleState(rules []pingRule, logger *Logger) {
	if len(rules) == 0 {
		if err := os.Remove(pingRuleState); err != nil && !os.IsNotExist(err) {
			logger.Debugf("ping rules: %v", err)
		}
		return
	}
	var b strings.Builder
	for _, r := range rules {
		b.WriteString(r.String() + "\n")
	}
	if err := os.WriteFile(pingRuleState, []byte(b.String()), 0o644); err != nil {
		logger.Debugf("ping rules: %v", err)
	}
}
