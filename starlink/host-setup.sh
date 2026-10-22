#!/bin/bash
# host-setup.sh: send the traffic of the cav1 VM through Starlink (run as root).
#
# The Starlink router (bypass mode) connects to NIC (default enp8s0). It
# gives the address with DHCP (carrier NAT, gateway usually 100.64.0.1).
# The host itself stays on the other connection (fiber on wlan0): the
# route of NIC gets a high metric.
#
# The traffic of the VM taps (cavtap0 10.77.0.0/24, and cavtap1/cavtap2
# 10.78.0.0/24 and 10.79.0.0/24 if they exist) goes through table 55 with
# the default route of Starlink, and NAT on NIC. If the Starlink route is
# not there, an "unreachable" rule stops the traffic of the VM: then a test
# cannot go through the fiber by mistake.
#
# Usage:
#   sudo starlink/host-setup.sh up [NIC]
#   sudo starlink/host-setup.sh down [NIC]   remove the rules (the VM then
#        uses the main table of the host)
#   sudo starlink/host-setup.sh status [NIC]
set -euo pipefail

CMD=${1:-status}
NIC=${2:-enp8s0}
TABLE=55
NETS=(10.77.0.0/24 10.78.0.0/24 10.79.0.0/24)
TAPS=(cavtap0 cavtap1 cavtap2)

gateway() {
	# The DHCP gateway of NIC: from NetworkManager, else from the routes.
	local gw=""
	command -v nmcli >/dev/null && gw=$(nmcli -g IP4.GATEWAY dev show "$NIC" 2>/dev/null || true)
	[ -n "$gw" ] || gw=$(ip -4 route show default dev "$NIC" | awk '{ print $3; exit }')
	echo "$gw"
}

down() {
	for i in "${!NETS[@]}"; do
		while ip rule del pref $((1000 + i)) 2>/dev/null; do :; done
		while ip rule del pref $((1010 + i)) 2>/dev/null; do :; done
	done
	ip route flush table $TABLE 2>/dev/null || true
	nft delete table ip cavstarlink 2>/dev/null || true
}

case $CMD in
up)
	gw=$(gateway)
	[ -n "$gw" ] || { echo "error: $NIC has no IPv4 gateway (is the Starlink router connected and in bypass mode?)" >&2; exit 1; }
	# The host stays on the other connection. Starlink has no IPv6 on
	# the host: else the host sends IPv6 through Starlink.
	if command -v nmcli >/dev/null; then
		con=$(nmcli -g GENERAL.CONNECTION dev show "$NIC")
		nmcli con mod "$con" ipv4.route-metric 900 ipv6.method disabled
		nmcli dev reapply "$NIC" >/dev/null
		sleep 2
		gw=$(gateway)
	fi
	down
	ip route replace default via "$gw" dev "$NIC" table $TABLE
	for i in "${!NETS[@]}"; do
		ip link show dev "${TAPS[$i]}" >/dev/null 2>&1 || continue
		ip route replace "${NETS[$i]}" dev "${TAPS[$i]}" table $TABLE
		ip rule add pref $((1000 + i)) from "${NETS[$i]}" lookup $TABLE
		ip rule add pref $((1010 + i)) from "${NETS[$i]}" unreachable
		command -v ufw >/dev/null && ufw route allow in on "${TAPS[$i]}" out on "$NIC" >/dev/null
	done
	nft add table ip cavstarlink
	nft add chain ip cavstarlink post '{ type nat hook postrouting priority 100; }'
	nft add rule ip cavstarlink post ip saddr '{ 10.77.0.0/24, 10.78.0.0/24, 10.79.0.0/24 }' oifname "$NIC" masquerade
	sysctl -qw net.ipv4.ip_forward=1
	;&
status)
	echo "gateway of $NIC: $(gateway)"
	ip rule | grep -E "^10[01][0-9]:" || echo "no rules for the VM"
	ip route show table $TABLE
	gw=$(gateway)
	[ -z "$gw" ] || ping -c 3 -W 2 -I "$NIC" "$gw" | tail -1 || true
	ping -c 3 -W 2 -I "$NIC" 100.64.0.1 | tail -1 || true
	# The host needs its own connection (for example Wi-Fi on the fiber).
	ip -4 route show default | grep -qv "dev $NIC" ||
		echo "warning: the host has no default route on another interface: the host also uses Starlink (turn the Wi-Fi on)"
	;;
down)
	down
	echo "removed: the VM uses the main table of the host now"
	;;
*)
	sed -n '2,/^set /s/^# \{0,1\}//p' "$0" | sed '$d'
	exit 1
	;;
esac
