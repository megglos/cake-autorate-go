// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// The pingers bind their sockets to the ping interface (SO_BINDTODEVICE).
// With policy routing, that is not sufficient. mwan3 marks the packets of
// the router for the link of its policy. The route table of that link has
// no route through the ping interface, and the next rule of mwan3
// ("unreachable") drops the packets. Thus, each ping interface gets one
// rule before the rules of mwan3 (priority 1001 and higher): the packets of
// a socket bound to the interface use the main table, which has the default
// routes of all links. A socket that is not bound (for example the
// traffic of the LAN) does not match the rule.

// pingRulePriority is the priority of the rules of the ping interfaces.
const pingRulePriority = "900"

// pingRuleRe finds the device of a rule of a ping interface in the output
// of "ip -4 rule show".
var pingRuleRe = regexp.MustCompile(`\boif (\S+) lookup main\b`)

func pingRuleArgs(op, dev string) []string {
	return []string{"-4", "rule", op, "pref", pingRulePriority, "oif", dev, "lookup", "main"}
}

// AddPingRules adds the rule of each ping interface of the links. First, it
// removes the rules of an earlier process (for example after a crash). It
// returns the devices that got a rule. An error does not stop the program:
// without policy routing, the pings operate also without the rule.
func AddPingRules(links []LinkConfig, logger *Logger) []string {
	clearPingRules(logger)
	var devs []string
	seen := make(map[string]bool)
	for _, l := range links {
		dev := l.PingInterfaceName
		if dev == "" || seen[dev] {
			continue
		}
		seen[dev] = true
		if out, err := execCommand("ip", pingRuleArgs("add", dev)...); err != nil {
			logger.Errorf("[%s] no routing rule for the pings through %s (with mwan3, the pings can go through another link): %v: %s",
				l.Name, dev, err, strings.TrimSpace(string(out)))
			continue
		}
		devs = append(devs, dev)
	}
	return devs
}

// RemovePingRules removes the rules of the devices.
func RemovePingRules(devs []string, logger *Logger) {
	for _, dev := range devs {
		if err := removePingRule(dev); err != nil {
			logger.Errorf("%v (remove it with \"ip -4 rule del pref %s oif %s lookup main\")", err, pingRulePriority, dev)
		}
	}
}

// clearPingRules removes the rules of all ping interfaces, also of devices
// that are not in the config now.
func clearPingRules(logger *Logger) {
	out, err := execCommand("ip", "-4", "rule", "show", "pref", pingRulePriority)
	if err != nil {
		return // no ip command, or no IPv4: then no rule can stay
	}
	seen := make(map[string]bool)
	for _, m := range pingRuleRe.FindAllStringSubmatch(string(out), -1) {
		if dev := m[1]; !seen[dev] {
			seen[dev] = true
			if err := removePingRule(dev); err != nil {
				logger.Errorf("%v", err)
			}
		}
	}
}

// removePingRule removes all rules of dev.
func removePingRule(dev string) error {
	for range pinMaxRules {
		out, err := execCommand("ip", pingRuleArgs("del", dev)...)
		if err != nil {
			if pinAbsent(out) {
				return nil
			}
			return fmt.Errorf("ip rule del for the pings through %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("ip rule del for the pings through %s: more than %d rules", dev, pinMaxRules)
}
