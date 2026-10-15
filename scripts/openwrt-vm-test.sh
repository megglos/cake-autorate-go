#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Tests the package cake-autorate-go in the OpenWrt VM of
# scripts/openwrt-vm.sh: start, status file, control socket, reload, invalid
# configuration, disable and stop.
#
# Usage: scripts/openwrt-vm-test.sh
# Start the VM and install the package first:
#   scripts/openwrt-vm.sh start
#   scripts/openwrt-vm.sh install cake-autorate-go-*.apk
#
# The test changes the configuration in the VM. The exit code is 1 if a
# test fails.

set -uo pipefail

VM="$(dirname "$0")/openwrt-vm.sh"
FAILED=0

vm() {
	"$VM" ssh "$@"
}

ok() {
	echo "ok   $1"
}

fail() {
	echo "FAIL $1"
	FAILED=1
}

# check NAME CMD: runs CMD in the VM. The test passes if the exit code is 0.
check() {
	local name=$1
	shift
	if vm "$@"; then ok "$name"; else fail "$name"; fi
}

# wait_for NAME CMD: like check, but tries again for 20 s.
wait_for() {
	local name=$1
	shift
	for _ in $(seq 20); do
		if vm "$@" 2>/dev/null; then
			ok "$name"
			return
		fi
		sleep 1
	done
	fail "$name"
}

pid() {
	vm 'pidof cake-autorate-go || true'
}

# control CMD: sends CMD to the control socket and prints the answer.
control() {
	vm "ucode -e '
		import * as socket from \"socket\";
		let s = socket.connect(\"/var/run/cake-autorate/control.sock\");
		s.send(\"$1\\n\");
		print(s.recv(256));
	'"
}

echo "== preparation"
# The test uses CAKE on eth1 (upload) and br-lan (download, the traffic to
# the LAN). The socket module is only for the control socket test.
vm 'apk add ucode-mod-socket >/dev/null' || fail "install ucode-mod-socket"
vm '/etc/init.d/sqm stop 2>/dev/null
	tc qdisc replace dev eth1 root cake bandwidth 20mbit
	tc qdisc replace dev br-lan root cake bandwidth 80mbit' || fail "set up CAKE"
vm '/etc/init.d/cake-autorate stop 2>/dev/null
	uci -q delete cake-autorate.wan
	uci set cake-autorate.global.enabled=1
	uci set cake-autorate.global.ping_method=icmp
	uci set cake-autorate.wan=link
	uci set cake-autorate.wan.dl_interface=br-lan
	uci set cake-autorate.wan.dl_min_rate_kbps=5000
	uci set cake-autorate.wan.dl_base_rate_kbps=20000
	uci set cake-autorate.wan.dl_max_rate_kbps=80000
	uci set cake-autorate.wan.ul_interface=eth1
	uci set cake-autorate.wan.ul_min_rate_kbps=5000
	uci set cake-autorate.wan.ul_base_rate_kbps=10000
	uci set cake-autorate.wan.ul_max_rate_kbps=35000
	uci add_list cake-autorate.wan.reflectors=1.1.1.1
	uci add_list cake-autorate.wan.reflectors=8.8.8.8
	uci add_list cake-autorate.wan.reflectors=9.9.9.9
	uci commit cake-autorate
	# reload_config sends an event only for a file that it knows from its
	# last run. A package installed after the boot is not known.
	reload_config' || fail "write the configuration"

echo "== start"
check "service is enabled" '/etc/init.d/cake-autorate enabled'
vm '/etc/init.d/cake-autorate start'
wait_for "program runs" 'pidof cake-autorate-go >/dev/null'
check "generated config has adjust true" 'grep -q "\"adjust\": true" /var/etc/cake-autorate.yaml'
wait_for "status file has the link" 'ucode -e "import { readfile } from \"fs\"; let s = json(readfile(\"/var/run/cake-autorate/status.json\")); exit(s.links[0].name == \"wan\" ? 0 : 1);"'
check "control socket has mode 0600" 'ls -l /var/run/cake-autorate/control.sock | grep -q "^srw------- "'
check "status directory has mode 0755" 'ls -ld /var/run/cake-autorate | grep -q "^drwxr-xr-x "'

echo "== control socket"
a=$(control list)
[ "$a" = "wan" ] && ok "list" || fail "list (answer: $a)"
a=$(control "pause wan dl")
[ "$a" = "ok" ] && ok "pause" || fail "pause (answer: $a)"
check "pause sets the base rate" 'tc qdisc show dev br-lan | grep -q "bandwidth 20Mbit"'
a=$(control "resume wan")
[ "$a" = "ok" ] && ok "resume" || fail "resume (answer: $a)"
a=$(control "pause nolink")
case $a in "error: unknown link"*) ok "unknown link" ;; *) fail "unknown link (answer: $a)" ;; esac

echo "== reload"
p1=$(pid)
vm '/etc/init.d/cake-autorate reload'
sleep 2
[ "$(pid)" = "$p1" ] && ok "reload without change keeps the process" || fail "reload without change restarted the process"
vm 'uci set cake-autorate.wan.ul_base_rate_kbps=12000 && uci commit cake-autorate && reload_config'
wait_for "changed config is in the generated file" 'grep -q "\"base_rate_kbps\": 12000" /var/etc/cake-autorate.yaml'
wait_for "changed config restarts the process" "p=\$(pidof cake-autorate-go) && [ -n \"\$p\" ] && [ \"\$p\" != \"$p1\" ]"

echo "== invalid configuration"
vm 'uci set cake-autorate.wan.dl_min_rate_kbps=fast && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
check "converter error is in the log" 'logread -e cake-autorate | grep -q "dl_min_rate_kbps: \"fast\" is not an integer"'
wait_for "program does not run" '! pidof cake-autorate-go >/dev/null'
vm 'uci set cake-autorate.wan.dl_min_rate_kbps=90000 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
check "check-config error is in the log" 'logread -e cake-autorate | grep -q "configuration check failed"'
vm 'uci set cake-autorate.wan.dl_min_rate_kbps=5000 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "program runs again" 'pidof cake-autorate-go >/dev/null'

echo "== disable and stop"
vm 'uci set cake-autorate.wan.enabled=0 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "disabled link: program does not run" '! pidof cake-autorate-go >/dev/null'
check "disabled message is in the log" 'logread -e cake-autorate | grep -q "disabled, not started"'
vm 'uci set cake-autorate.wan.enabled=1 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "program runs again" 'pidof cake-autorate-go >/dev/null'
vm '/etc/init.d/cake-autorate stop'
wait_for "stop removes the status file and the socket" '! pidof cake-autorate-go >/dev/null && [ ! -e /var/run/cake-autorate/status.json ] && [ ! -e /var/run/cake-autorate/control.sock ]'

if [ "$FAILED" -ne 0 ]; then
	echo "== log"
	vm 'logread -e cake-autorate | tail -40'
	exit 1
fi
echo "all tests passed"
