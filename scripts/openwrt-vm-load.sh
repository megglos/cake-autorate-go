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
#   scripts/openwrt-vm-load.sh start [SECONDS]  start the load in the
#                                               background (default 600 s)
#   scripts/openwrt-vm-load.sh stop             stop the load
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
# script.
# shellcheck disable=SC2016
LOAD='
end=$(( $(date +%s) + $1 ))
dl() { curl -s -o /dev/null --max-time "$1" https://proof.ovh.net/files/1Gb.dat; }
ul() {
	u_end=$(( $(date +%s) + $1 ))
	while [ "$(date +%s)" -lt "$u_end" ]; do
		curl -s -o /dev/null --max-time 10 --data-binary @/tmp/cav-up.bin https://speed.cloudflare.com/__up
	done
}
while [ "$(date +%s)" -lt "$end" ]; do
	dl 30; sleep 10
	ul 15; sleep 10
	dl 20 & ul 20; wait; sleep 10
done
'

cmd_stop() {
	"$VM" ssh "start-stop-daemon -K -q -p $PIDFILE -s TERM; rm -f $PIDFILE; killall -q curl" || true
}

case "${1:-}" in
start)
	seconds="${2:-600}"
	cmd_stop
	"$VM" ssh "command -v curl >/dev/null || { apk update -q && apk add -q curl; }
		dd if=/dev/urandom of=/tmp/cav-up.bin bs=1M count=10 2>/dev/null
		cat > /tmp/cav-load.sh <<'EOF'
$LOAD
EOF
		start-stop-daemon -S -b -m -p $PIDFILE -x /bin/sh -- /tmp/cav-load.sh $seconds"
	echo "load for $seconds s; stop it with: $0 stop"
	;;
stop) cmd_stop ;;
*)
	sed -n '4,13p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
