#!/usr/bin/env bash
# emulate-link.sh: emulate a variable link (for example 5G) on a fast
# connection, and record a test session through it.
#
# The script makes a network namespace "emu-router". The namespace operates
# as the router: it runs CAKE, cake-autorate-go and the load generator. The
# root namespace operates as the modem: it has a bottleneck for each
# direction, and the bottleneck rate follows a capacity trace. The
# bottleneck has a large queue, as a 5G modem has. Thus, the load causes
# bufferbloat, and autorate must prevent it.
#
#   emu-router namespace                root namespace ("modem")
#   load-gen, cake-autorate-go,         download bottleneck: tbf + netem on
#   CAKE on emu-wan            ──veth── emu-modem egress
#                                       upload bottleneck: tbf + netem on
#                                       ifb-emu-ul (emu-modem ingress)
#                                       NAT ──► UPLINK ──► internet
#
# The real connection must be much faster than the trace. Keep the maximum of
# the trace at 70 % or less of the capacity of the real connection. If not,
# the real connection is also a bottleneck.
#
# At the start of each block, the trace starts again from t=0. Thus, each
# policy gets the same capacity changes. The script writes the applied
# capacity to DIR/capacity.tsv. scripts/summarize-session.sh uses it as the
# reference for the throughput.
#
# Usage:
#   sudo ./scripts/emulate-link.sh -u UPLINK -t TRACE [options] [-- SESSION-OPTIONS]
#   sudo ./scripts/emulate-link.sh -k
#
# Options:
#   -u UPLINK   the interface of the real internet connection (required)
#   -t TRACE    capacity trace, CSV "t_ms,dl_kbps,ul_kbps" (required).
#               Make one with scripts/gen-trace.sh.
#   -d MS       extra base RTT of the emulated link (default: 20)
#   -q PACKETS  queue size of each bottleneck in packets (default: 1000)
#   -c CONFIG   cake-autorate-go config (default: made from the trace, with
#               the interfaces ifb4emu-wan and emu-wan)
#   -o DIR      output directory (default: recordings/<date>-emu)
#   -k          remove an old emulation setup (after a crash), then stop
#   -h          show this help
#
# SESSION-OPTIONS go to scripts/record-session.sh. The script already gives
# -i, -c, -o, -s (no saturation check), -w 8 and -C (the median capacity of
# the trace). Examples: -r 2 -m "dl ul both" -b 120 -F 85 -w 16.
#
# Example:
#   ./scripts/gen-trace.sh -d 200 -r 1 > recordings/trace-5g.csv
#   sudo ./scripts/emulate-link.sh -u enp7s0 -t recordings/trace-5g.csv -- -r 1 -m "dl ul" -F 85
#
# Requirements: bash, iproute2, iptables, curl, awk, jq (for the summary).

set -euo pipefail
export LC_ALL=C

NS=emu-router
VETH_NS=emu-wan
VETH_ROOT=emu-modem
IFB_UL=ifb-emu-ul
NET_ROOT=10.200.0.1
NET_NS=10.200.0.2
SUBNET=10.200.0.0/30
TAG=cake-autorate-emu

UPLINK=""
TRACE=""
DELAY_MS=20
QUEUE=1000
CONFIG=""
OUTDIR=""
CLEANUP_ONLY=0

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

usage() {
    sed -n '2,/^$/s/^# \{0,1\}//p' "$0"
    exit "${1:-0}"
}

while getopts "u:t:d:q:c:o:kh" opt; do
    case $opt in
        u) UPLINK="$OPTARG" ;;
        t) TRACE="$OPTARG" ;;
        d) DELAY_MS="$OPTARG" ;;
        q) QUEUE="$OPTARG" ;;
        c) CONFIG="$OPTARG" ;;
        o) OUTDIR="$OPTARG" ;;
        k) CLEANUP_ONLY=1 ;;
        h) usage 0 ;;
        *) usage 1 ;;
    esac
done
shift $((OPTIND - 1))
SESSION_ARGS=("$@")

die() { echo "error: $*" >&2; exit 1; }
log() { echo "[emulate] [$(date '+%H:%M:%S')] $*"; }

[ "$(id -u)" -eq 0 ] || die "run this script as root"

# --- Setup and cleanup -----------------------------------------------------

RUN=""
PLAYER_PID=""
FORWARD_BEFORE=""

# iptables_rule adds (-I/-A) or deletes (-D) the forward and NAT rules.
iptables_rules() {
    local op=$1
    local fwd_op=$op
    [ "$op" = "-A" ] && fwd_op="-I"
    iptables $fwd_op FORWARD -i "$VETH_ROOT" -o "$UPLINK" -m comment --comment "$TAG" -j ACCEPT
    iptables $fwd_op FORWARD -i "$UPLINK" -o "$VETH_ROOT" -m conntrack --ctstate RELATED,ESTABLISHED \
        -m comment --comment "$TAG" -j ACCEPT
    iptables -t nat "$op" POSTROUTING -s "$SUBNET" -o "$UPLINK" -m comment --comment "$TAG" -j MASQUERADE
}

teardown() {
    [ -n "$PLAYER_PID" ] && kill "$PLAYER_PID" 2>/dev/null && wait "$PLAYER_PID" 2>/dev/null
    ip netns del "$NS" 2>/dev/null || true
    ip link del "$IFB_UL" 2>/dev/null || true
    ip link del "$VETH_ROOT" 2>/dev/null || true
    # Delete all rules with the tag, also rules of an old setup.
    for table in filter nat; do
        iptables -t "$table" -S 2>/dev/null | grep -- "--comment $TAG" | sed 's/^-A /-D /' |
            while read -r rule; do
                # shellcheck disable=SC2086
                iptables -t "$table" $rule 2>/dev/null || true
            done
    done
    [ -n "$FORWARD_BEFORE" ] && sysctl -qw net.ipv4.ip_forward="$FORWARD_BEFORE"
    rm -rf "/etc/netns/$NS"
    [ -n "$RUN" ] && rm -rf "$RUN"
    if [ -n "$OUTDIR" ] && [ -d "$OUTDIR" ] && [ -n "${SUDO_USER:-}" ]; then
        chown -R "$SUDO_USER": "$OUTDIR" 2>/dev/null || true
    fi
}

if [ "$CLEANUP_ONLY" -eq 1 ]; then
    UPLINK=${UPLINK:-none}
    teardown
    log "The emulation setup is removed."
    exit 0
fi

[ -n "$UPLINK" ] && [ -n "$TRACE" ] || usage 1
ip link show "$UPLINK" >/dev/null 2>&1 || die "interface $UPLINK does not exist"
[ -f "$TRACE" ] || die "trace $TRACE does not exist"
ip netns list | grep -qw "$NS" && die "namespace $NS exists. Remove the old setup with: sudo $0 -k"
for cmd in tc iptables curl awk; do
    command -v "$cmd" >/dev/null || die "$cmd is not installed"
done

# Trace statistics: min, p5, median and max for each direction (kbit/s).
read -r DL_MIN DL_P5 DL_MED DL_MAX UL_MIN UL_P5 UL_MED UL_MAX <<< "$(awk -F, '
    NR > 1 { n++; d[n] = $2; u[n] = $3 }
    function q(a, p,   i) { i = int(n * p) + 1; return a[i > n ? n : i] }
    END {
        if (n == 0) exit 1
        asort(d); asort(u)
        print d[1], q(d, 0.05), q(d, 0.5), d[n], u[1], q(u, 0.05), q(u, 0.5), u[n]
    }' "$TRACE")" || die "the trace $TRACE has no data lines"

OUTDIR="${OUTDIR:-recordings/$(date '+%Y%m%d-%H%M')-emu}"
mkdir -p "$OUTDIR"
cp "$TRACE" "$OUTDIR/trace.csv"
RUN=$(mktemp -d)
trap teardown EXIT
trap 'exit 130' INT TERM

if [ -z "$CONFIG" ]; then
    CONFIG="$OUTDIR/emu-config.yaml"
    # The rates are what a user can know about the link: min_rate below the
    # usual minimum, base_rate at the median, max_rate at the maximum.
    cat > "$CONFIG" <<EOF
# Made by emulate-link.sh from $(basename "$TRACE").
links:
  - name: emu
    download:
      interface: ifb4$VETH_NS
      adjust: true
      min_rate_kbps: $(( DL_P5 * 8 / 10 > 1000 ? DL_P5 * 8 / 10 : 1000 ))
      base_rate_kbps: $DL_MED
      max_rate_kbps: $DL_MAX
    upload:
      interface: $VETH_NS
      adjust: true
      min_rate_kbps: $(( UL_P5 * 8 / 10 > 1000 ? UL_P5 * 8 / 10 : 1000 ))
      base_rate_kbps: $UL_MED
      max_rate_kbps: $UL_MAX
    ping_interface: $VETH_NS
log_to_file: false
EOF
fi

log "Trace: dl $((DL_MIN / 1000))-$((DL_MAX / 1000)) Mbit/s (median $((DL_MED / 1000))), ul $((UL_MIN / 1000))-$((UL_MAX / 1000)) Mbit/s (median $((UL_MED / 1000)))"
log "Output directory: $OUTDIR"

# Namespace and veth pair
ip netns add "$NS"
ip link add "$VETH_ROOT" type veth peer name "$VETH_NS"
ip link set "$VETH_NS" netns "$NS"
ip addr add "$NET_ROOT/30" dev "$VETH_ROOT"
ip link set "$VETH_ROOT" up
ip netns exec "$NS" ip addr add "$NET_NS/30" dev "$VETH_NS"
ip netns exec "$NS" ip link set "$VETH_NS" up
ip netns exec "$NS" ip link set lo up
ip netns exec "$NS" ip route add default via "$NET_ROOT"

# A real link sends single packets. Turn off the offloads of the veth pair,
# so that the bottleneck and CAKE see packets of the MTU size.
ethtool -K "$VETH_ROOT" gro off gso off tso off >/dev/null 2>&1 || true
ip netns exec "$NS" ethtool -K "$VETH_NS" gro off gso off tso off >/dev/null 2>&1 || true

# DNS in the namespace (systemd-resolved at 127.0.0.53 is not available there)
mkdir -p "/etc/netns/$NS"
printf 'nameserver 1.1.1.1\nnameserver 9.9.9.9\n' > "/etc/netns/$NS/resolv.conf"

# Routing and NAT from the namespace to the internet
FORWARD_BEFORE=$(sysctl -n net.ipv4.ip_forward)
sysctl -qw net.ipv4.ip_forward=1
iptables_rules -A

# burst_bytes prints the tbf burst for a rate: 1 ms of data, minimum 3000 bytes.
burst_bytes() { echo $(( $1 * 1000 / 8 / 1000 > 3000 ? $1 * 1000 / 8 / 1000 : 3000 )); }

# Bottlenecks. The tbf qdisc limits the rate. The netem child holds the queue
# (QUEUE packets) and adds half of the extra base RTT in each direction.
read -r T0_DL T0_UL <<< "$(awk -F, 'NR == 2 { print $2, $3 }' "$TRACE")"
half_delay="$(awk -v d="$DELAY_MS" 'BEGIN { printf "%.1fms", d / 2 }')"
setup_bottleneck() {
    local dev=$1 rate=$2
    tc qdisc replace dev "$dev" root handle 1: tbf rate "${rate}kbit" burst "$(burst_bytes "$rate")" limit 1000000
    tc qdisc add dev "$dev" parent 1:1 handle 10: netem delay "$half_delay" limit "$QUEUE"
}
setup_bottleneck "$VETH_ROOT" "$T0_DL"
modprobe ifb 2>/dev/null || true
ip link add "$IFB_UL" type ifb
ip link set "$IFB_UL" up
tc qdisc add dev "$VETH_ROOT" handle ffff: ingress
tc filter add dev "$VETH_ROOT" parent ffff: matchall action mirred egress redirect dev "$IFB_UL"
setup_bottleneck "$IFB_UL" "$T0_UL"

ip netns exec "$NS" ping -c 2 -W 2 1.1.1.1 >/dev/null || die "no internet connection from the namespace"
log "Emulated link is active (extra base RTT ${DELAY_MS} ms, queue ${QUEUE} packets)"

# --- Trace player ----------------------------------------------------------

# play_trace applies the trace to the bottlenecks. When $RUN/restart exists,
# it starts the trace again from t=0. At the end of the trace, it starts
# again from t=0. It writes each change to capacity.tsv.
play_trace() {
    # This function runs in a subshell. Do not run the traps of the main
    # script (teardown) here.
    trap - EXIT
    trap 'exit 0' INT TERM
    local -a ts dls uls
    local n=0 i t0 now target
    while IFS=, read -r t dl ul; do
        [[ "$t" =~ ^[0-9]+$ ]] || continue
        ts[n]=$t; dls[n]=$dl; uls[n]=$ul; n=$((n + 1))
    done < "$TRACE"
    local step=$(( n > 1 ? ts[1] - ts[0] : 1000 ))
    local period=$(( ts[n - 1] + step ))
    local cap="$OUTDIR/capacity.tsv"
    printf 'unix_ms\tdl_kbps\tul_kbps\n' > "$cap"

    # now_ms prints the time in ms (bash 5 EPOCHREALTIME, without a fork).
    now_ms() { local e=${EPOCHREALTIME/[.,]/}; echo $(( e / 1000 )); }

    t0=$(now_ms); i=0
    while true; do
        target=$(( t0 + ts[i] ))
        while now=$(now_ms); [ "$now" -lt "$target" ]; do
            [ -e "$RUN/restart" ] && break
            sleep 0.02
        done
        if [ -e "$RUN/restart" ]; then
            rm -f "$RUN/restart"
            t0=$(now_ms); i=0
        fi
        tc qdisc change dev "$VETH_ROOT" root handle 1: tbf rate "${dls[i]}kbit" \
            burst "$(burst_bytes "${dls[i]}")" limit 1000000 2>/dev/null || true
        tc qdisc change dev "$IFB_UL" root handle 1: tbf rate "${uls[i]}kbit" \
            burst "$(burst_bytes "${uls[i]}")" limit 1000000 2>/dev/null || true
        printf '%d\t%d\t%d\n' "$(now_ms)" "${dls[i]}" "${uls[i]}" >> "$cap"
        i=$((i + 1))
        if [ "$i" -ge "$n" ]; then
            t0=$(( t0 + period )); i=0
        fi
    done
}
play_trace &
PLAYER_PID=$!

# --- Session ---------------------------------------------------------------

# The hook gets the arguments BLOCK MODE POLICY. It does not use them.
printf '#!/bin/sh\ntouch "%s/restart"\n' "$RUN" > "$RUN/block-start"
chmod +x "$RUN/block-start"
ip netns exec "$NS" env BLOCK_START_HOOK="$RUN/block-start" \
    "$SCRIPT_DIR/record-session.sh" -i "$VETH_NS" -c "$CONFIG" -o "$OUTDIR" \
    -s -w 8 -C "$DL_MED,$UL_MED" "${SESSION_ARGS[@]}"

{
    echo "emulation: uplink $UPLINK, extra base RTT ${DELAY_MS} ms, queue ${QUEUE} packets"
    echo "trace: $(basename "$TRACE"), dl ${DL_MIN}-${DL_MAX} kbit/s (median ${DL_MED}), ul ${UL_MIN}-${UL_MAX} kbit/s (median ${UL_MED})"
} >> "$OUTDIR/session.txt"
