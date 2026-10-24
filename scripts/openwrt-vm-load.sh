#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Puts load on the WAN of the OpenWrt VM, for example for the screenshots
# of the LuCI app (openwrt/luci-app-cake-autorate-go/tests/screenshots.js).
# The VM downloads and uploads in cycles: download, pause, upload, pause,
# download and upload at the same time, pause. Thus the graphs of the
# status page show the shaper rates go up and down in both directions.
#
# Usage:
#   scripts/openwrt-vm-load.sh setup            shape the WAN with SQM and
#                                               set the link "wan" of the
#                                               service to its shapers
#   scripts/openwrt-vm-load.sh start [SECONDS]  start the load in the
#                                               background (default 600 s)
#   scripts/openwrt-vm-load.sh stop             stop the load
#
# The load starts in the VM, thus only the WAN shapes it: the download
# shaper must be on the IFB of the WAN (ifb4eth1), not on br-lan. The
# browser tests set br-lan. "setup" replaces the configuration of the
# service with one link: SQM on eth1, download 10 to 150 Mbit/s (base 40),
# upload 3 to 30 Mbit/s (base 10), the rates of the screenshots.
#
# The download is a large file of the OVH test server: the speed test of
# Cloudflare refuses files of 100 MB and more, and it limits the number of
# downloads (HTTP 429). The upload goes to the speed test of Cloudflare.
# The VM must run (scripts/openwrt-vm.sh start). The script installs curl
# in the VM: uclient-fetch cannot upload to Cloudflare.

set -euo pipefail

VM="$(dirname "$0")/openwrt-vm.sh"
PIDFILE=/tmp/cav-load.pid

# The load loop, in the VM (ash). The VM expands the variables, not this
# script. Each phase stops at the end of the load time.
# shellcheck disable=SC2016
LOAD='
end=$(( $(date +%s) + $1 ))
left() { l=$(( end - $(date +%s) )); [ "$l" -gt 0 ] || return 1; [ "$l" -lt "$1" ] && echo "$l" || echo "$1"; }
pause() { t=$(left "$1") || exit 0; sleep "$t"; }
dl() { t=$(left "$1") || exit 0; curl -s -o /dev/null --max-time "$t" https://proof.ovh.net/files/1Gb.dat; }
ul() {
	t=$(left "$1") || exit 0
	u_end=$(( $(date +%s) + t ))
	while [ "$(date +%s)" -lt "$u_end" ]; do
		curl -s -o /dev/null --max-time "$(( u_end - $(date +%s) ))" \
			--data-binary @/tmp/cav-up.bin https://speed.cloudflare.com/__up
	done
}
while [ "$(date +%s)" -lt "$end" ]; do
	dl 30; pause 10
	ul 15; pause 10
	dl 20 & ul 20; wait; pause 10
done
'

# start-stop-daemon -b starts the loop in a new session, with its own
# process group. The group is not the PID of the loop (busybox forks two
# times), thus stop reads it from /proc. The kill of the group also stops
# the curl processes of the loop, and no other processes.
cmd_stop() {
	# shellcheck disable=SC2016
	"$VM" ssh '[ -f '"$PIDFILE"' ] || exit 0
		pid=$(cat '"$PIDFILE"')
		pgid=$(cut -d" " -f5 /proc/$pid/stat 2>/dev/null) && kill -TERM -$pgid
		rm -f '"$PIDFILE" || true
}

case "${1:-}" in
setup)
	# The VM expands the variables.
	# shellcheck disable=SC2016
	"$VM" ssh 'set -e
		tc qdisc del dev br-lan root 2>/dev/null || true
		uci set sqm.eth1.enabled=1
		uci set sqm.eth1.download=150000
		uci set sqm.eth1.upload=30000
		uci commit sqm
		/etc/init.d/sqm enable
		/etc/init.d/sqm restart
		for s in $(uci -q show cake-autorate | sed -n "s/^cake-autorate\.\([^.=]*\)=\(link\|global\)$/\1/p"); do
			uci delete cake-autorate.$s
		done
		uci set cake-autorate.global=global
		uci set cake-autorate.global.enabled=1
		uci set cake-autorate.global.status_history=1
		uci set cake-autorate.wan=link
		uci set cake-autorate.wan.dl_interface=ifb4eth1
		uci set cake-autorate.wan.dl_min_rate_kbps=10000
		uci set cake-autorate.wan.dl_base_rate_kbps=40000
		uci set cake-autorate.wan.dl_max_rate_kbps=150000
		uci set cake-autorate.wan.ul_interface=eth1
		uci set cake-autorate.wan.ul_min_rate_kbps=3000
		uci set cake-autorate.wan.ul_base_rate_kbps=10000
		uci set cake-autorate.wan.ul_max_rate_kbps=30000
		uci commit cake-autorate
		/etc/init.d/cake-autorate enable
		/etc/init.d/cake-autorate restart'
	;;
start)
	seconds="${2:-600}"
	cmd_stop
	"$VM" ssh "set -e
		command -v curl >/dev/null || { apk update -q && apk add -q curl; }
		dd if=/dev/urandom of=/tmp/cav-up.bin bs=1M count=10 2>/dev/null
		cat > /tmp/cav-load.sh <<'EOF'
$LOAD
EOF
		start-stop-daemon -S -b -m -p $PIDFILE -x /bin/sh -- /tmp/cav-load.sh $seconds"
	echo "load for $seconds s; stop it with: $0 stop"
	;;
stop) cmd_stop ;;
*)
	sed -n '4,16p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
