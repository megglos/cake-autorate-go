#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Makes a client behind the OpenWrt VM: a network namespace on this host
# with a cable to the LAN of the VM. A browser in the namespace uses the
# VM as its router, like a PC behind a router. Use it for live tests of
# the shaping (for example a bufferbloat test of a website).
#
# Usage:
#   sudo scripts/openwrt-vm-client.sh up USER   make the tap device (owner
#                                               USER), the bridge and the
#                                               namespace
#   scripts/openwrt-vm-client.sh firefox        start Firefox in the
#                                               namespace (asks for sudo)
#   sudo scripts/openwrt-vm-client.sh down      remove all of it
#
# Then start the VM with the tap device on its LAN:
#   VM_LAN_TAP=cavlan0 scripts/openwrt-vm.sh start
#
# The namespace has the address 192.168.1.50/24. The VM (192.168.1.1) is
# the gateway and the DNS server. LuCI in the namespace: http://192.168.1.1.
#
# Host:  cavlan0 (tap, QEMU) --- cavbr (bridge) --- cavveth
# Space: cavns: cavveth-ns 192.168.1.50/24

set -euo pipefail

NS=cavns
TAP=cavlan0
BR=cavbr
VETH=cavveth
ADDR=192.168.1.50/24
GW=192.168.1.1

cmd_up() {
	local user="${1:?usage: $0 up USER}"
	ip netns add "$NS"
	ip tuntap add dev "$TAP" mode tap user "$user"
	ip link add "$BR" type bridge
	ip link add "$VETH" type veth peer name "$VETH-ns"
	ip link set "$TAP" master "$BR"
	ip link set "$VETH" master "$BR"
	ip link set "$VETH-ns" netns "$NS"
	ip link set "$TAP" up
	ip link set "$VETH" up
	ip link set "$BR" up
	ip -n "$NS" link set lo up
	ip -n "$NS" addr add "$ADDR" dev "$VETH-ns"
	ip -n "$NS" link set "$VETH-ns" up
	ip -n "$NS" route add default via "$GW"
	mkdir -p "/etc/netns/$NS"
	echo "nameserver $GW" >"/etc/netns/$NS/resolv.conf"
	echo "client is ready: start the VM with VM_LAN_TAP=$TAP"
}

cmd_down() {
	ip netns del "$NS" 2>/dev/null || true
	ip link del "$VETH" 2>/dev/null || true
	ip link del "$BR" 2>/dev/null || true
	ip link del "$TAP" 2>/dev/null || true
	rm -rf "/etc/netns/$NS"
}

# cmd_firefox: starts Firefox as this user in the namespace, with its own
# profile. The Wayland and the X11 sockets are files: the namespace does
# not change them.
cmd_firefox() {
	local profile="${XDG_CACHE_HOME:-$HOME/.cache}/cake-autorate-vm-client/firefox"
	mkdir -p "$profile"
	sudo ip netns exec "$NS" sudo -u "$(id -un)" \
		env HOME="$HOME" XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-}" \
		WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-}" DISPLAY="${DISPLAY:-}" \
		firefox --no-remote --profile "$profile" "http://$GW" "$@"
}

case "${1:-}" in
up)
	shift
	cmd_up "$@"
	;;
down) cmd_down ;;
firefox)
	shift
	cmd_firefox "$@"
	;;
*)
	sed -n '4,25p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
