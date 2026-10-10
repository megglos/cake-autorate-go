#!/bin/sh
# cake-pc.sh: set up CAKE on a PC that connects directly to a modem.
#
# Use this script to run cake-autorate-go on a Linux PC without a router.
# The script adds CAKE on the modem interface (upload) and on an IFB
# interface (download).
#
# Usage:
#   sudo ./cake-pc.sh up WAN [DL_RATE] [UL_RATE]
#   sudo ./cake-pc.sh down WAN
#
#   WAN       the network interface of the modem (see: ip -br link)
#   DL_RATE   the CAKE download bandwidth, for example 200mbit (default: unlimited)
#   UL_RATE   the CAKE upload bandwidth, for example 30mbit (default: unlimited)
#
# Examples:
#   sudo ./cake-pc.sh up wwan0                  # for link recordings without autorate
#   sudo ./cake-pc.sh up wwan0 200mbit 30mbit   # for autorate
#   sudo ./cake-pc.sh down wwan0
#
# Requirements: iproute2 (tc), kernel modules sch_cake, ifb and act_mirred.

set -e

ACTION="$1"
WAN="$2"
DL_RATE="${3:-unlimited}"
UL_RATE="${4:-unlimited}"

usage() {
    sed -n '2,/^$/s/^# \{0,1\}//p' "$0"
    exit 1
}

[ -n "$ACTION" ] && [ -n "$WAN" ] || usage
[ "$(id -u)" -eq 0 ] || { echo "error: run this script as root" >&2; exit 1; }
ip link show "$WAN" >/dev/null 2>&1 || { echo "error: interface $WAN does not exist" >&2; exit 1; }

# The IFB interface name. Linux limits interface names to 15 characters.
IFB=$(printf 'ifb4%s' "$WAN" | cut -c1-15)

rate_arg() {
    if [ "$1" = "unlimited" ]; then echo "unlimited"; else echo "bandwidth $1"; fi
}

case "$ACTION" in
up)
    modprobe sch_cake
    modprobe ifb
    modprobe act_mirred

    # Upload: CAKE on the egress of the modem interface.
    # shellcheck disable=SC2046
    tc qdisc replace dev "$WAN" root cake $(rate_arg "$UL_RATE") besteffort

    # Download: send the ingress traffic of the modem interface to the IFB
    # interface, and put CAKE on the egress of the IFB interface.
    ip link show "$IFB" >/dev/null 2>&1 || ip link add "$IFB" type ifb
    ip link set "$IFB" up
    tc qdisc del dev "$WAN" ingress 2>/dev/null || true
    tc qdisc add dev "$WAN" handle ffff: ingress
    tc filter add dev "$WAN" parent ffff: matchall action mirred egress redirect dev "$IFB"
    # shellcheck disable=SC2046
    tc qdisc replace dev "$IFB" root cake $(rate_arg "$DL_RATE") besteffort ingress

    echo "CAKE is active. Use these interfaces in the cake-autorate-go config:"
    echo
    echo "  download:"
    echo "    interface: $IFB"
    echo "  upload:"
    echo "    interface: $WAN"
    echo "  ping_interface: $WAN"
    echo
    tc -s qdisc show dev "$WAN" | head -n 1
    tc -s qdisc show dev "$IFB" | head -n 1
    ;;
down)
    tc qdisc del dev "$WAN" root 2>/dev/null || true
    tc qdisc del dev "$WAN" ingress 2>/dev/null || true
    ip link del "$IFB" 2>/dev/null || true
    echo "CAKE is removed from $WAN and $IFB."
    ;;
*)
    usage
    ;;
esac
