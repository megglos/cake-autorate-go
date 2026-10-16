#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Tests the packages cake-autorate-go and luci-app-cake-autorate-go in the
# OpenWrt VM of scripts/openwrt-vm.sh: start, status file, control socket,
# reload, invalid configuration, disable, stop, and the files, the rpcd
# plugin and the ACL of the LuCI app.
#
# Usage: scripts/openwrt-vm-test.sh
# Start the VM and install the packages first:
#   scripts/openwrt-vm.sh start
#   scripts/openwrt-vm.sh install cake-autorate-go-*.apk luci-app-cake-autorate-go-*.apk
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
# the LAN). The LuCI app installs the socket module. Install it also for
# a test of cake-autorate-go without the LuCI app.
vm 'apk add ucode-mod-socket >/dev/null' || fail "install ucode-mod-socket"
vm '/etc/init.d/sqm stop 2>/dev/null
	tc qdisc replace dev eth1 root cake bandwidth 20mbit &&
	tc qdisc replace dev br-lan root cake bandwidth 80mbit' || fail "set up CAKE"
vm '/etc/init.d/cake-autorate stop 2>/dev/null
	uci -q delete cake-autorate.wan
	uci set cake-autorate.global.enabled=1
	uci set cake-autorate.global.ping_method=icmp
	uci set cake-autorate.global.status_history=1
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

echo "== rpcd plugin"
# rpc METHOD ARGS: calls the method of the rpcd plugin of the LuCI app.
rpc() {
	vm "ubus call cake-autorate $1 '$2'"
}
a=$(rpc pause '{"link":"wan","dir":"ul"}')
case $a in *'"result": "ok"'*) ok "rpc pause" ;; *) fail "rpc pause (answer: $a)" ;; esac
wait_for "status file shows the pause" 'jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].upload.paused" | grep -qx true'
check "status file shows no pause of the other direction" 'jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].download.paused" | grep -qx false'
a=$(rpc resume '{"link":"wan"}')
case $a in *'"result": "ok"'*) ok "rpc resume" ;; *) fail "rpc resume (answer: $a)" ;; esac
a=$(rpc pause '{"link":"nolink"}')
case $a in *'unknown link'*) ok "rpc unknown link" ;; *) fail "rpc unknown link (answer: $a)" ;; esac
# A line break in the name must not add a second command.
a=$(rpc pause '{"link":"wan\nresume wan"}')
case $a in *'Invalid link name'*) ok "rpc invalid link name" ;; *) fail "rpc invalid link name (answer: $a)" ;; esac
a=$(rpc pause '{"link":"wan","dir":"both"}')
case $a in *'Invalid direction'*) ok "rpc invalid direction" ;; *) fail "rpc invalid direction (answer: $a)" ;; esac
# A stopped process does not answer. The plugin must stop after its
# timeout of 5 s.
vm 'kill -STOP $(pidof cake-autorate-go)'
t0=$(date +%s)
a=$(rpc pause '{"link":"wan"}')
t=$(( $(date +%s) - t0 ))
vm 'kill -CONT $(pidof cake-autorate-go)'
case $a in *'No answer from the service: '*) [ "$t" -le 10 ] && ok "rpc timeout (${t}s)" || fail "rpc timeout took ${t}s" ;; *) fail "rpc timeout (answer: $a)" ;; esac
# The history is longer than one read of the socket: the plugin must read
# the complete line. Wait for some samples (one each second).
wait_for "history has samples" "ubus call cake-autorate history | jsonfilter -e @.data | jsonfilter -e '@.links[0].samples[5]' >/dev/null"
a=$(rpc history '{}' | wc -c)
[ "$a" -gt 256 ] && ok "rpc history (${a} bytes)" || fail "rpc history (${a} bytes)"
check "rpc history is valid JSON" "ubus call cake-autorate history | jsonfilter -e @.data | jsonfilter -e '@.fields[0]' | grep -qx t_ms"
# Calibration: start, the status file shows it, stop. The session needs
# no load for this check.
a=$(rpc calibrate_status '{}')
case $a in *'state'*) ok "rpc calibrate_status" ;; *) fail "rpc calibrate_status (answer: $a)" ;; esac
a=$(rpc calibrate_start '{"link":"wan","load_s":20}')
case $a in *'"result": "ok"'*) ok "rpc calibrate_start" ;; *) fail "rpc calibrate_start (answer: $a)" ;; esac
wait_for "status file shows the calibration" 'jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].calibrating" | grep -qx true'
a=$(rpc calibrate_stop '{}')
case $a in *'"result": "ok"'*) ok "rpc calibrate_stop" ;; *) fail "rpc calibrate_stop (answer: $a)" ;; esac
wait_for "status file shows the end of the calibration" 'jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].calibrating" | grep -qx false'
a=$(rpc calibrate_status '{}')
case $a in *'stopped'*) ok "rpc calibrate_status after the stop" ;; *) fail "rpc calibrate_status after the stop (answer: $a)" ;; esac
a=$(rpc calibrate_start '{"link":"wan\nresume wan"}')
case $a in *'Invalid link name'*) ok "rpc calibrate_start invalid link name" ;; *) fail "rpc calibrate_start invalid link name (answer: $a)" ;; esac
a=$(rpc calibrate_start '{"link":"wan","load_s":5}')
case $a in *'load time'*) ok "rpc calibrate_start invalid load time" ;; *) fail "rpc calibrate_start invalid load time (answer: $a)" ;; esac
a=$(rpc qdisc '{"interface":"eth1"}')
case $a in *'"kind": "cake"'*) ok "rpc qdisc with CAKE" ;; *) fail "rpc qdisc with CAKE (answer: $a)" ;; esac
a=$(rpc qdisc '{"interface":"lo"}')
case $a in *'"kind": "noqueue"'*) ok "rpc qdisc without CAKE" ;; *) fail "rpc qdisc without CAKE (answer: $a)" ;; esac
a=$(rpc qdisc '{"interface":"nosuch0"}')
case $a in *'Cannot find device'*) ok "rpc qdisc of a missing interface" ;; *) fail "rpc qdisc of a missing interface (answer: $a)" ;; esac
# A shell character in the name must not run a command.
a=$(rpc qdisc '{"interface":"lo;touch /tmp/qdisc-injected"}')
case $a in *'Invalid interface name'*) ok "rpc qdisc invalid interface name" ;; *) fail "rpc qdisc invalid interface name (answer: $a)" ;; esac
check "rpc qdisc runs no other command" '[ ! -e /tmp/qdisc-injected ]'

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
check "converter error is in the error file" 'grep -q "dl_min_rate_kbps: \"fast\" is not an integer" /var/run/cake-autorate/config-error'
wait_for "program does not run" '! pidof cake-autorate-go >/dev/null'
vm 'uci set cake-autorate.wan.dl_min_rate_kbps=90000 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
check "check-config error is in the log" 'logread -e cake-autorate | grep -q "configuration check failed"'
check "check-config error is in the error file" 'grep -q "base_rate_kbps must be >= min_rate_kbps" /var/run/cake-autorate/config-error'
vm 'uci set cake-autorate.wan.dl_min_rate_kbps=5000 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "program runs again" 'pidof cake-autorate-go >/dev/null'
check "valid configuration removes the error file" '[ ! -e /var/run/cake-autorate/config-error ]'

echo "== disable and stop"
vm 'uci set cake-autorate.wan.enabled=0 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "disabled link: program does not run" '! pidof cake-autorate-go >/dev/null'
check "disabled message is in the log" 'logread -e cake-autorate | grep -q "disabled, not started"'
vm 'uci set cake-autorate.wan.enabled=1 && uci commit cake-autorate && /etc/init.d/cake-autorate restart'
wait_for "program runs again" 'pidof cake-autorate-go >/dev/null'
vm '/etc/init.d/cake-autorate stop'
wait_for "stop removes the status file and the socket" '! pidof cake-autorate-go >/dev/null && [ ! -e /var/run/cake-autorate/status.json ] && [ ! -e /var/run/cake-autorate/control.sock ]'
a=$(rpc pause '{"link":"wan"}')
case $a in *'The service does not run'*) ok "rpc with a stopped service" ;; *) fail "rpc with a stopped service (answer: $a)" ;; esac

echo "== LuCI app"
for v in cake-autorate/config cake-autorate/status; do
	check "web server sends the view $v" "wget -qO- http://127.0.0.1/luci-static/resources/view/$v.js | grep -q view.extend"
	check "menu has the view $v" "jsonfilter -i /usr/share/luci/menu.d/luci-app-cake-autorate-go.json -e '@[\"admin/network/$v\"].action.path' | grep -qx $v"
done
check "web server sends the Overview section" 'wget -qO- http://127.0.0.1/luci-static/resources/view/status/include/70_cake-autorate.js | grep -q baseclass.extend'
check "web server sends the common module" 'wget -qO- http://127.0.0.1/luci-static/resources/cake-autorate/common.js | grep -q baseclass.extend'
check "rpcd has the plugin" 'ubus list cake-autorate >/dev/null'
# root has all permissions. The user "cakeacl" has only the ACL groups of
# the app and of luci-base, thus the checks show missing permissions. The
# user "cakeread" has only the read permissions.
vm 'uci -q delete rpcd.cakeacl
	uci set rpcd.cakeacl=login
	uci set rpcd.cakeacl.username=cakeacl
	uci set rpcd.cakeacl.password="\$p\$root"
	uci add_list rpcd.cakeacl.read=luci-base
	uci add_list rpcd.cakeacl.read=luci-app-cake-autorate-go
	uci add_list rpcd.cakeacl.write=luci-app-cake-autorate-go
	uci -q delete rpcd.cakeread
	uci set rpcd.cakeread=login
	uci set rpcd.cakeread.username=cakeread
	uci set rpcd.cakeread.password="\$p\$root"
	uci add_list rpcd.cakeread.read=luci-base
	uci add_list rpcd.cakeread.read=luci-app-cake-autorate-go
	uci commit rpcd
	/etc/init.d/rpcd reload' || fail "add the rpcd users"
sleep 2
# access SCOPE OBJECT FUNCTION: checks a permission of the user cakeacl
# (or of the user in ACL_USER).
access() {
	vm "sid=\$(ubus call session login '{\"username\":\"${ACL_USER:-cakeacl}\",\"password\":\"\"}' | jsonfilter -e @.ubus_rpc_session) &&
		ubus call session access '{\"ubus_rpc_session\":\"'\$sid'\",\"scope\":\"$1\",\"object\":\"$2\",\"function\":\"$3\"}' | grep -q '\"access\": true'"
}
# rpcd loads the ACL file only if it is valid.
access access-group luci-app-cake-autorate-go write && ok "ACL: app group" || fail "ACL: app group"
access uci cake-autorate write && ok "ACL: write the UCI configuration" || fail "ACL: write the UCI configuration"
access ubus file read && ok "ACL: ubus file read" || fail "ACL: ubus file read"
access file /var/run/cake-autorate/config-error read && ok "ACL: read the error file" || fail "ACL: read the error file"
access file /var/run/cake-autorate/status.json read && ok "ACL: read the status file" || fail "ACL: read the status file"
access ubus cake-autorate pause && ok "ACL: pause" || fail "ACL: pause"
access ubus cake-autorate resume && ok "ACL: resume" || fail "ACL: resume"
access file /etc/shadow read && fail "ACL: other files are not readable" || ok "ACL: other files are not readable"
ACL_USER=cakeread access file /var/run/cake-autorate/status.json read && ok "ACL: read-only user reads the status file" || fail "ACL: read-only user reads the status file"
ACL_USER=cakeread access ubus cake-autorate history && ok "ACL: read-only user reads the history" || fail "ACL: read-only user reads the history"
ACL_USER=cakeread access ubus cake-autorate qdisc && ok "ACL: read-only user reads the qdisc" || fail "ACL: read-only user reads the qdisc"
ACL_USER=cakeread access ubus cake-autorate pause && fail "ACL: read-only user cannot pause" || ok "ACL: read-only user cannot pause"
access ubus cake-autorate calibrate_start && ok "ACL: calibrate" || fail "ACL: calibrate"
ACL_USER=cakeread access ubus cake-autorate calibrate_start && fail "ACL: read-only user cannot calibrate" || ok "ACL: read-only user cannot calibrate"
ACL_USER=cakeread access ubus cake-autorate calibrate_status && fail "ACL: read-only user cannot read the calibration" || ok "ACL: read-only user cannot read the calibration"
vm 'uci delete rpcd.cakeacl && uci delete rpcd.cakeread && uci commit rpcd && /etc/init.d/rpcd reload'

if [ "$FAILED" -ne 0 ]; then
	echo "== log"
	vm 'logread -e cake-autorate | tail -40'
	exit 1
fi
echo "all tests passed"
