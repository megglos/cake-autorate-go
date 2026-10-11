#!/usr/bin/env bash
# ci-emulation.sh: test cake-autorate-go on an emulated link, without an
# internet connection.
#
# The script makes three network namespaces. It uses the real binary, the
# real CAKE qdisc, netlink and the pingers. The simulator does not test
# these parts.
#
#   cie-router                 cie-modem                    cie-server
#   CAKE, cake-autorate-go,    bottleneck: tbf + netem      reflectors
#   iperf3 clients, ping       on each egress interface     (10.210.2.1-6),
#   cie-wan ──────────── veth ── cie-lan  cie-up ── veth ── iperf3 servers
#
# The modem has a large queue (QUEUE packets) and a rate that changes (a
# square wave: the high rate and half of it, PERIOD_S each). The kernel of
# the server namespace replies to ICMP echo and ICMP timestamp requests.
#
# The test has two phases. Each phase has IDLE_S without load, then LOAD_S
# with load in the two directions (iperf3):
#   1. unshaped: CAKE without a rate limit. The modem queue fills. This
#      phase makes sure that the emulation has bufferbloat.
#   2. autorate: cake-autorate-go controls CAKE.
# Then the script compares the results with the limits. The exit code is 1
# if a limit is not met.
#
# Usage:
#   sudo ./scripts/ci-emulation.sh [-b BINARY] [-o DIR]
#   sudo ./scripts/ci-emulation.sh -k
#
# Options:
#   -b BINARY  cake-autorate-go binary (default: ./cake-autorate-go)
#   -o DIR     output directory (default: recordings/<date>-ci-emu)
#   -k         remove an old setup (after a crash), then stop
#   -h         show this help
#
# Environment (defaults in brackets):
#   DL_KBPS [40000], UL_KBPS [10000]  high rate of the modem
#   PERIOD_S [15]       time of each rate step
#   DELAY_MS [20]       base RTT of the emulated link
#   QUEUE [1000]        queue of each bottleneck in packets
#   IDLE_S [10], LOAD_S [60]
#   MAX_P95_MS [60]     autorate: maximum p95 of the excess RTT under load
#   MIN_UNSHAPED_P95_MS [100]  unshaped: minimum p95 of the excess RTT
#   MIN_DL_PCT [50], MIN_UL_PCT [40]  autorate: minimum throughput in
#                       percent of the mean capacity
#   MIN_LOAD_REPLY_PCT [80] minimum replies of the RTT probe under load, in
#                       percent (else the p95 is nan and the check fails)
#   MIN_SHAPER_EVENTS [10]  autorate: minimum number of shaper rate changes
#   MIN_OWD_PINGS [100]     autorate: minimum number of pings with one-way
#                       delays
#
# Requirements: bash, iproute2, iperf3, iputils ping, awk, jq, ethtool, kernel modules
# sch_cake, sch_netem, sch_tbf, ifb and act_mirred.

set -euo pipefail
export LC_ALL=C

NS_R=cie-router
NS_M=cie-modem
NS_S=cie-server
WAN=cie-wan
IFB=ifb4cie-wan
LAN=cie-lan
UP=cie-up
SRV=cie-srv
REFLECTORS="10.210.2.1 10.210.2.2 10.210.2.3 10.210.2.4 10.210.2.5 10.210.2.6"
PROBE=10.210.2.10 # target of the RTT probe (not a reflector)

DL_KBPS=${DL_KBPS:-40000}
UL_KBPS=${UL_KBPS:-10000}
PERIOD_S=${PERIOD_S:-15}
DELAY_MS=${DELAY_MS:-20}
QUEUE=${QUEUE:-1000}
IDLE_S=${IDLE_S:-10}
LOAD_S=${LOAD_S:-60}
MAX_P95_MS=${MAX_P95_MS:-60}
MIN_UNSHAPED_P95_MS=${MIN_UNSHAPED_P95_MS:-100}
MIN_DL_PCT=${MIN_DL_PCT:-50}
MIN_SHAPER_EVENTS=${MIN_SHAPER_EVENTS:-10}
MIN_LOAD_REPLY_PCT=${MIN_LOAD_REPLY_PCT:-80}
MIN_OWD_PINGS=${MIN_OWD_PINGS:-100}
MIN_UL_PCT=${MIN_UL_PCT:-40}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
BINARY=./cake-autorate-go
OUTDIR=""
CLEANUP_ONLY=0

usage() {
    sed -n '2,/^$/s/^# \{0,1\}//p' "$0"
    exit "${1:-0}"
}

while getopts "b:o:kh" opt; do
    case $opt in
        b) BINARY="$OPTARG" ;;
        o) OUTDIR="$OPTARG" ;;
        k) CLEANUP_ONLY=1 ;;
        h) usage 0 ;;
        *) usage 1 ;;
    esac
done

die() { echo "error: $*" >&2; exit 1; }
log() { echo "[ci-emu] [$(date '+%H:%M:%S')] $*"; }

[ "$(id -u)" -eq 0 ] || die "run this script as root"

# The background jobs that run now. A job leaves the list after its wait:
# a later kill could hit a new process with the same PID.
WAVE_PID=""
AUTORATE_PID=""

teardown() {
    for pid in "$WAVE_PID" "$AUTORATE_PID"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null
    done
    # Stop all processes in the namespaces (iperf3 servers and clients,
    # ping, cake-autorate-go) before the wait: else the wait blocks until
    # the end of the phase. Deleting a namespace does not stop them.
    for ns in "$NS_R" "$NS_M" "$NS_S"; do
        for pid in $(ip netns pids "$ns" 2>/dev/null); do
            kill "$pid" 2>/dev/null || true
        done
    done
    wait 2>/dev/null || true
    for ns in "$NS_R" "$NS_M" "$NS_S"; do
        ip netns del "$ns" 2>/dev/null || true
    done
    if [ -n "$OUTDIR" ] && [ -d "$OUTDIR" ] && [ -n "${SUDO_USER:-}" ]; then
        chown -R "$SUDO_USER": "$OUTDIR" 2>/dev/null || true
    fi
}

if [ "$CLEANUP_ONLY" -eq 1 ]; then
    teardown
    log "The setup is removed."
    exit 0
fi

for cmd in tc iperf3 ping awk jq ethtool; do
    command -v "$cmd" >/dev/null || die "$cmd is not installed"
done
[ -x "$BINARY" ] || die "binary $BINARY does not exist (build it with: go build -o cake-autorate-go .)"
BINARY=$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")
for ns in "$NS_R" "$NS_M" "$NS_S"; do
    ip netns list | grep -qw "$ns" && die "namespace $ns exists. Remove the old setup with: sudo $0 -k"
done
for m in sch_cake sch_netem sch_tbf ifb act_mirred; do
    modprobe "$m" 2>/dev/null || true
done

OUTDIR="${OUTDIR:-recordings/$(date '+%Y%m%d-%H%M')-ci-emu}"
mkdir -p "$OUTDIR"
OUTDIR=$(cd "$OUTDIR" && pwd)
trap teardown EXIT
trap 'exit 130' INT TERM

r() { ip netns exec "$NS_R" "$@"; }
m() { ip netns exec "$NS_M" "$@"; }
s() { ip netns exec "$NS_S" "$@"; }

# --- Setup -----------------------------------------------------------------

for ns in "$NS_R" "$NS_M" "$NS_S"; do
    ip netns add "$ns"
    ip netns exec "$ns" ip link set lo up
done
ip link add "$WAN" netns "$NS_R" type veth peer name "$LAN" netns "$NS_M"
ip link add "$UP" netns "$NS_M" type veth peer name "$SRV" netns "$NS_S"

r ip addr add 10.210.0.2/30 dev "$WAN"
r ip link set "$WAN" up
r ip route add default via 10.210.0.1
m ip addr add 10.210.0.1/30 dev "$LAN"
m ip addr add 10.210.1.1/30 dev "$UP"
m ip link set "$LAN" up
m ip link set "$UP" up
m ip route add 10.210.2.0/24 via 10.210.1.2
m sysctl -qw net.ipv4.ip_forward=1
s ip addr add 10.210.1.2/30 dev "$SRV"
s ip link set "$SRV" up
s ip route add default via 10.210.1.1
for a in $REFLECTORS $PROBE; do
    s ip addr add "$a/32" dev lo
done

# A real link sends single packets: turn off the offloads, so that the
# bottleneck and CAKE see packets of the MTU size.
r ethtool -K "$WAN" gro off gso off tso off || die "cannot turn off the offloads of $WAN"
m ethtool -K "$LAN" gro off gso off tso off || die "cannot turn off the offloads of $LAN"
m ethtool -K "$UP" gro off gso off tso off || die "cannot turn off the offloads of $UP"
s ethtool -K "$SRV" gro off gso off tso off || die "cannot turn off the offloads of $SRV"

# burst_bytes prints the tbf burst for a rate: 1 ms of data, minimum 3000 bytes.
burst_bytes() { echo $(( $1 / 8 > 3000 ? $1 / 8 : 3000 )); }

# Bottlenecks in the modem: download on the egress to the router, upload on
# the egress to the server. The netem child holds the queue and adds half
# of the base RTT in each direction.
half_delay=$(awk -v d="$DELAY_MS" 'BEGIN { printf "%.1fms", d / 2 }')
for spec in "$LAN $DL_KBPS" "$UP $UL_KBPS"; do
    read -r dev rate <<< "$spec"
    m tc qdisc add dev "$dev" root handle 1: tbf rate "${rate}kbit" burst "$(burst_bytes "$rate")" limit 1000000
    m tc qdisc add dev "$dev" parent 1:1 handle 10: netem delay "$half_delay" limit "$QUEUE"
done

# CAKE in the router, without a rate limit.
r "$SCRIPT_DIR/cake-pc.sh" up "$WAN" >/dev/null

r ping -c 2 -W 2 "$PROBE" >/dev/null || die "no connection from $NS_R to $NS_S"
s iperf3 -s -p 5201 -D
s iperf3 -s -p 5202 -D
sleep 1
log "Emulated link: dl ${DL_KBPS} and $((DL_KBPS / 2)) kbit/s, ul ${UL_KBPS} and $((UL_KBPS / 2)) kbit/s, ${PERIOD_S} s each; base RTT ${DELAY_MS} ms; queue ${QUEUE} packets"

# --- Rate changes ----------------------------------------------------------

# rate_wave changes the modem rates: the high rate for PERIOD_S, then half
# of it for PERIOD_S. It starts with the high rate.
rate_wave() {
    trap - EXIT
    # The sleep runs in the background with an interruptible wait: else
    # bash runs the trap only after the sleep, and the phase takes up to
    # PERIOD_S more.
    local sleep_pid=""
    trap '[ -n "$sleep_pid" ] && kill "$sleep_pid" 2>/dev/null; exit 0' INT TERM
    local f=1
    while true; do
        for spec in "$LAN $DL_KBPS" "$UP $UL_KBPS"; do
            read -r dev rate <<< "$spec"
            rate=$(( rate / f ))
            m tc qdisc change dev "$dev" root handle 1: tbf rate "${rate}kbit" burst "$(burst_bytes "$rate")" limit 1000000
        done
        sleep "$PERIOD_S" &
        sleep_pid=$!
        wait "$sleep_pid"
        f=$(( 3 - f ))
    done
}

# --- Phases ----------------------------------------------------------------

# phase NAME runs IDLE_S without load and LOAD_S with load. The RTT probe
# runs during the two parts.
phase() {
    local name=$1 dir="$OUTDIR/$1"
    mkdir -p "$dir"
    log "Phase $name: ${IDLE_S} s idle, then ${LOAD_S} s load in the two directions"
    r ping -n -i 0.1 -w $(( IDLE_S + LOAD_S )) "$PROBE" > "$dir/ping.txt" 2>&1 &
    local ping_pid=$!
    sleep "$IDLE_S"
    rate_wave &
    WAVE_PID=$!
    r iperf3 -c 10.210.1.2 -p 5201 -R -P 4 -t "$LOAD_S" -J > "$dir/iperf-dl.json" 2>&1 &
    local dl_pid=$!
    r iperf3 -c 10.210.1.2 -p 5202 -P 4 -t "$LOAD_S" -J > "$dir/iperf-ul.json" 2>&1 &
    local ul_pid=$!
    wait "$dl_pid" "$ul_pid" || log "warning: iperf3 failed (see $dir/iperf-*.json)"
    # The wave runs until this kill. If it stopped before (a tc error), the
    # phase did not have the square wave.
    kill -0 "$WAVE_PID" 2>/dev/null || die "phase $name: the rate changes stopped (tc error)"
    kill "$WAVE_PID" 2>/dev/null || true
    wait "$WAVE_PID" 2>/dev/null || true
    WAVE_PID=""
    wait "$ping_pid" || true
    sleep 2
}

# The mean capacity of the load time. The wave starts with the high rate.
# If LOAD_S is not a number of full wave cycles, the high rate has more
# time.
high_s=$(( LOAD_S / (2 * PERIOD_S) * PERIOD_S ))
rest=$(( LOAD_S % (2 * PERIOD_S) ))
high_s=$(( high_s + (rest < PERIOD_S ? rest : PERIOD_S) ))
mean_dl=$(( DL_KBPS * (LOAD_S + high_s) / (2 * LOAD_S) ))
mean_ul=$(( UL_KBPS * (LOAD_S + high_s) / (2 * LOAD_S) ))

phase unshaped

CONFIG="$OUTDIR/config.yaml"
cat > "$CONFIG" <<EOF
# Made by ci-emulation.sh.
links:
  - name: emu
    download:
      interface: $IFB
      adjust: true
      min_rate_kbps: $(( DL_KBPS / 10 ))
      base_rate_kbps: $(( DL_KBPS / 2 ))
      max_rate_kbps: $DL_KBPS
    upload:
      interface: $WAN
      adjust: true
      min_rate_kbps: $(( UL_KBPS / 10 ))
      base_rate_kbps: $(( UL_KBPS / 2 ))
      max_rate_kbps: $UL_KBPS
    ping_interface: $WAN
    reflectors: [$(echo "$REFLECTORS" | sed 's/ /, /g')]
ping_method: icmp-timestamp
pinger_count: 6
log_to_file: false
EOF
r "$BINARY" -config "$CONFIG" -record "$OUTDIR/autorate.jsonl.gz" > "$OUTDIR/autorate.log" 2>&1 &
AUTORATE_PID=$!
sleep 5
kill -0 "$AUTORATE_PID" 2>/dev/null || { cat "$OUTDIR/autorate.log" >&2; die "cake-autorate-go stopped"; }
phase autorate
kill -0 "$AUTORATE_PID" 2>/dev/null || { cat "$OUTDIR/autorate.log" >&2; die "cake-autorate-go stopped during the load"; }
kill "$AUTORATE_PID"
wait "$AUTORATE_PID" 2>/dev/null || true
AUTORATE_PID=""

# --- Results ---------------------------------------------------------------

# excess_p95 prints the p95 of the excess RTT under load (ms): the RTT minus
# the minimum RTT of the idle time. ping prints one line for each reply.
# It sends at 0.1 s: icmp_seq N is at N/10 s.
# The first awk prints "idle RTT" or "load RTT"; sort puts the idle lines
# first and sorts the RTTs (portable: no GNU awk asort). With fewer than
# MIN_LOAD_REPLY_PCT of the load probes answered, it prints nan (the check
# fails): lost probes are not in the p95. The p95 is the sample
# floor(0.95 * n) + 1, as in the simulator and summarize-session.sh.
excess_p95() {
    awk -v idle="$IDLE_S" '
        /time=/ {
            seq = $0; sub(/.*icmp_seq=/, "", seq); sub(/ .*/, "", seq)
            rtt = $0; sub(/.*time=/, "", rtt); sub(/ ms.*/, "", rtt)
            print (seq + 0 <= idle * 10 ? "idle" : "load"), rtt + 0
        }' "$1" | sort -k1,1 -k2,2n | awk -v want=$(( LOAD_S * 10 * MIN_LOAD_REPLY_PCT / 100 )) '
        $1 == "idle" { if (min == "") min = $2; next }
        { load[++k] = $2 }
        END {
            if (k == 0 || k < want || min == "") { print "nan"; exit }
            i = int(0.95 * k) + 1; if (i > k) i = k
            printf "%.1f\n", load[i] - min
        }'
}

# kbps prints the received throughput of an iperf3 JSON result (kbit/s).
kbps() {
    jq -r '.end.sum_received.bits_per_second / 1000 | floor' "$1" 2>/dev/null || echo 0
}

fail=0
check() { # check DESCRIPTION OK
    if [ "$2" -eq 1 ]; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi
}
ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a != "nan" && a >= b) }' && echo 1 || echo 0; }
le() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a != "nan" && a <= b) }' && echo 1 || echo 0; }

u_p95=$(excess_p95 "$OUTDIR/unshaped/ping.txt")
a_p95=$(excess_p95 "$OUTDIR/autorate/ping.txt")
u_dl=$(kbps "$OUTDIR/unshaped/iperf-dl.json"); u_ul=$(kbps "$OUTDIR/unshaped/iperf-ul.json")
a_dl=$(kbps "$OUTDIR/autorate/iperf-dl.json"); a_ul=$(kbps "$OUTDIR/autorate/iperf-ul.json")
a_dl_pct=$(( ${a_dl:-0} * 100 / mean_dl ))
a_ul_pct=$(( ${a_ul:-0} * 100 / mean_ul ))
shaper_events=$(gzip -dc "$OUTDIR/autorate.jsonl.gz" 2>/dev/null | grep -c '"type":"shaper"' || true)
# The recording has the shaper events also if netlink fails: then the
# shaper uses tc and logs it. The test must use netlink.
tc_fallbacks=$(grep -c "tc exec" "$OUTDIR/autorate.log" || true)
owd_pings=$(gzip -dc "$OUTDIR/autorate.jsonl.gz" 2>/dev/null | grep '"type":"ping"' | grep -c '"dl_owd_us"' || true)

{
    echo "phase     excess RTT p95   dl kbit/s   ul kbit/s"
    printf '%-9s %8s ms   %9s   %9s\n' unshaped "$u_p95" "${u_dl:-?}" "${u_ul:-?}"
    printf '%-9s %8s ms   %9s   %9s   (%d %% / %d %% of the mean capacity %d / %d)\n' \
        autorate "$a_p95" "${a_dl:-?}" "${a_ul:-?}" "$a_dl_pct" "$a_ul_pct" "$mean_dl" "$mean_ul"
    echo "shaper rate changes: $shaper_events, pings with one-way delays: $owd_pings"
    echo
    check "unshaped: excess RTT p95 $u_p95 ms >= $MIN_UNSHAPED_P95_MS ms (the emulation has bufferbloat)" "$(ge "$u_p95" "$MIN_UNSHAPED_P95_MS")"
    check "autorate: excess RTT p95 $a_p95 ms <= $MAX_P95_MS ms" "$(le "$a_p95" "$MAX_P95_MS")"
    check "autorate: download $a_dl_pct % >= $MIN_DL_PCT % of the mean capacity" "$(ge "$a_dl_pct" "$MIN_DL_PCT")"
    check "autorate: upload $a_ul_pct % >= $MIN_UL_PCT % of the mean capacity" "$(ge "$a_ul_pct" "$MIN_UL_PCT")"
    check "autorate: the shaper rate changed ($shaper_events times)" "$(ge "$shaper_events" "$MIN_SHAPER_EVENTS")"
    check "autorate: netlink, no tc fallback ($tc_fallbacks log lines)" "$(le "$tc_fallbacks" 0)"
    check "autorate: ICMP timestamp replies ($owd_pings)" "$(ge "$owd_pings" "$MIN_OWD_PINGS")"
} > "$OUTDIR/result.txt"
cat "$OUTDIR/result.txt"
log "Output: $OUTDIR"
exit "$fail"
