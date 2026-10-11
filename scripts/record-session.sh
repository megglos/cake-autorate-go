#!/usr/bin/env bash
# record-session.sh: record a test session on a PC that connects directly to
# the internet connection (modem or router).
#
# The script measures the bufferbloat and the throughput of the link with
# different shaper policies. It records each block with cake-autorate-go
# --record. The blocks alternate between the policies, because the capacity
# of the link can change during the session.
#
# Policies:
#   unshaped   CAKE without a bandwidth limit. Shows the capacity of the link
#              (the bandwidth reference) and the bufferbloat without a shaper.
#   fixed      CAKE with a fixed bandwidth (-F percent of the capacity).
#   autorate   cake-autorate-go controls the CAKE bandwidth.
#   upstream-NAME
#              the original cake-autorate (bash) controls the CAKE bandwidth
#              (option -U). cake-autorate-go measures with --no-adjust.
#
# Each block: start cake-autorate-go, wait without load (-I), apply the load
# for -b seconds, wait 5 s, stop cake-autorate-go.
#
# Before the blocks, a saturation check applies the load with 4, 8 and 16
# workers. It finds the capacity and the number of workers that fills the link.
#
# Usage:
#   sudo ./scripts/record-session.sh -i IFACE -c CONFIG [options]
#
# Options:
#   -i IFACE    network interface of the internet connection (required)
#   -c CONFIG   cake-autorate-go config for autorate (required). Use
#               download interface ifb4IFACE and upload interface IFACE.
#   -x BINARY   cake-autorate-go binary (default: ./cake-autorate-go)
#   -o DIR      output directory (default: recordings/<date>-<IFACE>)
#   -m MODES    load modes (default: "dl ul both")
#   -b SECONDS  load time for each block (default: 150)
#   -I SECONDS  time without load at the start of each block (default: 15)
#   -r ROUNDS   number of rounds (default: 2)
#   -W SECONDS  in the mode both: apply only the download load for SECONDS
#               before the load in the two directions (default: 0). The
#               measured load time starts after it. Use it to examine
#               learn_cross_delay: Core learns the upload delay from download
#               activity only while the upload load is low.
#   -F PERCENT  add the fixed policy at PERCENT of the capacity (default: off)
#   -A NAME=CONFIG[,BINARY]
#               add the policy autorate-NAME: autorate with a different config
#               (and optionally a different binary). Use it more than one
#               time for more variants. The blocks of all variants alternate,
#               thus the variants get the same link conditions (A/B test).
#   -U NAME=CONFIG
#               add the policy upstream-NAME: the original cake-autorate with
#               the config file CONFIG (config.primary.sh format). Set
#               dl_if=ifb4IFACE and ul_if=IFACE in CONFIG. Use it more than
#               one time for more variants. The environment variable
#               UPSTREAM_DIR is the directory with cake-autorate.sh, lib.sh
#               and defaults.sh (default: ../cake-autorate).
#   -w WORKERS  load workers for each direction (default: from the saturation check)
#   -C DL,UL    capacity in kbit/s (default: from the saturation check)
#   -s          do not do the saturation check (then -w is necessary, and
#               -C is necessary with -F)
#   -h          show this help
#
# Each round has one block for each mode and policy. Example: the defaults
# give 2 rounds x 3 modes x 2 policies = 12 blocks of approximately 3 minutes.
#
# Output files in DIR:
#   session.txt            settings, saturation check and system data
#   blocks.tsv             one line for each block, with the times (Unix s)
#   NN-MODE-POLICY.jsonl.gz    recording of each block
#   NN-MODE-POLICY.log         log of cake-autorate-go and load-gen.sh
#   NN-MODE-upstream-NAME.upstream/  config and log of cake-autorate (-U)
#
# Environment:
#   BLOCK_START_HOOK   a command that the script runs at the start of each
#                      block, with the arguments BLOCK MODE POLICY.
#                      scripts/emulate-link.sh uses it to start the capacity
#                      trace again, so that each block gets the same trace.
#
# After each block, the script shows a summary (scripts/summarize-session.sh).
# This needs jq.
#
# Requirements: bash, tc, curl, awk. With -U: the pinger of the upstream
# config (for example fping). Run from the repository root.

set -euo pipefail

IFACE=""
CONFIG=""
BINARY="./cake-autorate-go"
OUTDIR=""
MODES="dl ul both"
BLOCK_S=150
IDLE_S=15
ROUNDS=2
WARMUP_S=0
FIXED_PCT=0
WORKERS=""
CAPACITY=""
SKIP_CHECK=0

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(dirname "$SCRIPT_DIR")
LOADGEN="${LOADGEN:-$REPO_DIR/load-gen.sh}"
CAKE_PC="$SCRIPT_DIR/cake-pc.sh"
UPSTREAM_DIR="${UPSTREAM_DIR:-$REPO_DIR/../cake-autorate}"

usage() {
    sed -n '2,/^$/s/^# \{0,1\}//p' "$0"
    exit "${1:-0}"
}

VARIANTS=()
UPSTREAMS=()

while getopts "i:c:x:o:m:b:I:r:W:F:w:C:A:U:sh" opt; do
    case $opt in
        i) IFACE="$OPTARG" ;;
        c) CONFIG="$OPTARG" ;;
        x) BINARY="$OPTARG" ;;
        o) OUTDIR="$OPTARG" ;;
        m) MODES="$OPTARG" ;;
        b) BLOCK_S="$OPTARG" ;;
        I) IDLE_S="$OPTARG" ;;
        r) ROUNDS="$OPTARG" ;;
        W) WARMUP_S="$OPTARG" ;;
        F) FIXED_PCT="$OPTARG" ;;
        A) VARIANTS+=("$OPTARG") ;;
        U) UPSTREAMS+=("$OPTARG") ;;
        w) WORKERS="$OPTARG" ;;
        C) CAPACITY="$OPTARG" ;;
        s) SKIP_CHECK=1 ;;
        h) usage 0 ;;
        *) usage 1 ;;
    esac
done

die() { echo "error: $*" >&2; exit 1; }
log() { echo "[session] [$(date '+%H:%M:%S')] $*"; }

[ -n "$IFACE" ] && [ -n "$CONFIG" ] || usage 1
[ "$(id -u)" -eq 0 ] || die "run this script as root"
[ -d "/sys/class/net/$IFACE" ] || die "interface $IFACE does not exist"
[ -f "$CONFIG" ] || die "config $CONFIG does not exist"
[[ "$WARMUP_S" =~ ^[0-9]+$ ]] || die "-W must be a number of seconds"
WARMUP_S=$((10#$WARMUP_S))
[ -x "$BINARY" ] || die "binary $BINARY does not exist (build it with: go build .)"
[ -x "$LOADGEN" ] || die "load generator $LOADGEN does not exist"
for cmd in tc curl awk; do
    command -v "$cmd" >/dev/null || die "$cmd is not installed"
done

IFB=$(printf 'ifb4%s' "$IFACE" | cut -c1-15)
grep -q "$IFB" "$CONFIG" || echo "warning: $CONFIG does not contain the download interface $IFB" >&2
if [ "$SKIP_CHECK" -eq 1 ]; then
    [ -n "$WORKERS" ] || die "-s needs -w"
    if [ "$FIXED_PCT" -gt 0 ] && [ -z "$CAPACITY" ]; then
        die "-s with -F needs -C"
    fi
fi

OUTDIR="${OUTDIR:-recordings/$(date '+%Y%m%d-%H%M')-$IFACE}"
mkdir -p "$OUTDIR"
SESSION="$OUTDIR/session.txt"
BLOCKS="$OUTDIR/blocks.tsv"

POLICIES="unshaped"
[ "$FIXED_PCT" -gt 0 ] && POLICIES="$POLICIES fixed"
POLICIES="$POLICIES autorate"

# Variants: autorate-NAME uses VARIANT_CONFIG[NAME] and VARIANT_BINARY[NAME].
declare -A VARIANT_CONFIG VARIANT_BINARY
for v in "${VARIANTS[@]}"; do
    vname=${v%%=*}
    vspec=${v#*=}
    [ "$vname" != "$v" ] && [ -n "$vname" ] || die "-A needs NAME=CONFIG[,BINARY]: $v"
    [[ $vname =~ ^[a-zA-Z0-9_]+$ ]] || die "-A: NAME must contain only letters, digits and _: $vname"
    VARIANT_CONFIG[$vname]=${vspec%%,*}
    VARIANT_BINARY[$vname]=$BINARY
    [ "$vspec" != "${vspec#*,}" ] && VARIANT_BINARY[$vname]=${vspec#*,}
    [ -f "${VARIANT_CONFIG[$vname]}" ] || die "config ${VARIANT_CONFIG[$vname]} does not exist"
    [ -x "${VARIANT_BINARY[$vname]}" ] || die "binary ${VARIANT_BINARY[$vname]} does not exist"
    POLICIES="$POLICIES autorate-$vname"
done

# Upstream variants: upstream-NAME uses UPSTREAM_CONFIG[NAME].
declare -A UPSTREAM_CONFIG
for u in "${UPSTREAMS[@]}"; do
    uname=${u%%=*}
    [ "$uname" != "$u" ] && [ -n "$uname" ] || die "-U needs NAME=CONFIG: $u"
    [[ $uname =~ ^[a-zA-Z0-9_]+$ ]] || die "-U: NAME must contain only letters, digits and _: $uname"
    UPSTREAM_CONFIG[$uname]=$(realpath "${u#*=}")
    cp "${UPSTREAM_CONFIG[$uname]}" "$OUTDIR/upstream-$uname.sh"
    [ -f "${UPSTREAM_CONFIG[$uname]}" ] || die "config ${UPSTREAM_CONFIG[$uname]} does not exist"
    grep -q "^dl_if=$IFB\b" "${UPSTREAM_CONFIG[$uname]}" || die "${UPSTREAM_CONFIG[$uname]}: set dl_if=$IFB"
    grep -q "^ul_if=$IFACE\b" "${UPSTREAM_CONFIG[$uname]}" || die "${UPSTREAM_CONFIG[$uname]}: set ul_if=$IFACE"
    POLICIES="$POLICIES upstream-$uname"
done
if [ "${#UPSTREAMS[@]}" -gt 0 ]; then
    for f in cake-autorate.sh lib.sh defaults.sh; do
        [ -f "$UPSTREAM_DIR/$f" ] || die "$UPSTREAM_DIR/$f does not exist (set UPSTREAM_DIR)"
    done
fi

# --- Cleanup ---------------------------------------------------------------

CAR_PID=""
LOAD_PID=""
UP_PID=""
cleanup() {
    [ -n "$LOAD_PID" ] && kill "$LOAD_PID" 2>/dev/null && wait "$LOAD_PID" 2>/dev/null
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null && wait "$UP_PID" 2>/dev/null
    [ -n "$CAR_PID" ] && kill -INT "$CAR_PID" 2>/dev/null && wait "$CAR_PID" 2>/dev/null
    "$CAKE_PC" down "$IFACE" >/dev/null 2>&1 || true
    # Give the files to the user who started sudo.
    if [ -n "${SUDO_USER:-}" ] && [ -d "$OUTDIR" ]; then
        chown -R "$SUDO_USER": "$OUTDIR" 2>/dev/null || true
    fi
    log "CAKE is removed. Output: $OUTDIR"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# --- Helpers ---------------------------------------------------------------

# read_bytes prints the received and the transmitted bytes of IFACE.
read_bytes() {
    echo "$(cat "/sys/class/net/$IFACE/statistics/rx_bytes") $(cat "/sys/class/net/$IFACE/statistics/tx_bytes")"
}

# rate_kbps prints the download and the upload rate in kbit/s between two
# read_bytes results.
rate_kbps() {
    # shellcheck disable=SC2086
    set -- $1 $2 "$3"
    awk -v rx0="$1" -v tx0="$2" -v rx1="$3" -v tx1="$4" -v s="$5" \
        'BEGIN { printf "%d %d", (rx1-rx0)*8/s/1000, (tx1-tx0)*8/s/1000 }'
}

# set_cake sets the CAKE bandwidth. Use "unlimited" or a rate in kbit/s.
set_cake() {
    local dl=$1 ul=$2
    if [ "$dl" = "unlimited" ]; then
        tc qdisc change dev "$IFB" root cake unlimited
        tc qdisc change dev "$IFACE" root cake unlimited
    else
        tc qdisc change dev "$IFB" root cake bandwidth "${dl}kbit"
        tc qdisc change dev "$IFACE" root cake bandwidth "${ul}kbit"
    fi
}

# measure_load applies the load for 20 s. It sets MEASURED_DL and MEASURED_UL
# to the rates (kbit/s) in the last 10 s.
measure_load() {
    local mode=$1 workers=$2 b0 b1
    "$LOADGEN" -m "$mode" -w "$workers" -d 20 >/dev/null 2>&1 &
    LOAD_PID=$!
    sleep 10
    b0=$(read_bytes)
    sleep 10
    b1=$(read_bytes)
    wait "$LOAD_PID" 2>/dev/null || true
    LOAD_PID=""
    read -r MEASURED_DL MEASURED_UL <<< "$(rate_kbps "$b0" "$b1" 10)"
}

# --- Session ---------------------------------------------------------------

log "Output directory: $OUTDIR"
"$CAKE_PC" up "$IFACE" >/dev/null
cp "$CONFIG" "$OUTDIR/config.yaml"
for vname in "${!VARIANT_CONFIG[@]}"; do
    cp "${VARIANT_CONFIG[$vname]}" "$OUTDIR/config-$vname.yaml"
done

{
    echo "date: $(date -Iseconds)"
    echo "interface: $IFACE"
    echo "ifb: $IFB"
    echo "config: $CONFIG"
    echo "binary: $("$BINARY" --version 2>&1)"
    echo "git: $(git -C "$REPO_DIR" describe --always --dirty 2>/dev/null || echo unknown)"
    echo "kernel: $(uname -r)"
    echo "link: $(cat "/sys/class/net/$IFACE/speed" 2>/dev/null || echo unknown) Mbit/s, mtu $(cat "/sys/class/net/$IFACE/mtu")"
    echo "route: $(ip route get 1.1.1.1 2>/dev/null | head -n 1)"
    echo "modes: $MODES"
    echo "policies: $POLICIES"
    for vname in "${!VARIANT_CONFIG[@]}"; do
        echo "variant $vname: config ${VARIANT_CONFIG[$vname]}, binary ${VARIANT_BINARY[$vname]} ($("${VARIANT_BINARY[$vname]}" --version 2>&1))"
    done
    echo "rounds: $ROUNDS"
    echo "block: idle ${IDLE_S}s, load ${BLOCK_S}s, download warm-up in the mode both ${WARMUP_S}s"
} > "$SESSION"

ip route get 1.1.1.1 | grep -q "dev $IFACE" ||
    echo "warning: the route to the internet does not use $IFACE" >&2

# Saturation check
if [ "$SKIP_CHECK" -eq 0 ]; then
    log "Saturation check (CAKE unlimited, approximately 2 minutes)"
    set_cake unlimited unlimited
    echo "saturation check (kbit/s, last 10 s of 20 s):" >> "$SESSION"
    cap_dl=0; cap_ul=0; workers_needed=4
    for dir in dl ul; do
        prev=0; best=0; gain=0
        for w in 4 8 16; do
            measure_load "$dir" "$w"
            if [ "$dir" = "dl" ]; then rate=$MEASURED_DL; else rate=$MEASURED_UL; fi
            log "  $dir, $w workers: $((rate / 1000)) Mbit/s"
            echo "  $dir workers=$w rate=$rate" >> "$SESSION"
            [ "$rate" -gt "$best" ] && best=$rate
            # More than 10 % more throughput than with fewer workers: the
            # link was not full with fewer workers.
            gain=0
            if [ "$prev" -gt 0 ] && [ $((rate * 10)) -gt $((prev * 11)) ]; then
                gain=1
                [ "$w" -gt "$workers_needed" ] && workers_needed=$w
            fi
            prev=$rate
        done
        if [ "$gain" -eq 1 ]; then
            echo "warning: $dir throughput still increased from 8 to 16 workers. The link is possibly not full." | tee -a "$SESSION" >&2
        fi
        if [ "$dir" = "dl" ]; then cap_dl=$best; else cap_ul=$best; fi
    done
    [ -n "$WORKERS" ] || WORKERS=$workers_needed
    [ -n "$CAPACITY" ] || CAPACITY="$cap_dl,$cap_ul"
fi
echo "workers: $WORKERS" >> "$SESSION"
echo "capacity_kbps: ${CAPACITY:-unknown}" >> "$SESSION"
log "Workers: $WORKERS, capacity (dl,ul kbit/s): ${CAPACITY:-unknown}"

FIXED_DL=""; FIXED_UL=""
if [ "$FIXED_PCT" -gt 0 ]; then
    FIXED_DL=$(( ${CAPACITY%,*} * FIXED_PCT / 100 ))
    FIXED_UL=$(( ${CAPACITY#*,} * FIXED_PCT / 100 ))
    echo "fixed_kbps: $FIXED_DL,$FIXED_UL ($FIXED_PCT %)" >> "$SESSION"
fi

printf 'block\tround\tmode\tpolicy\tstart\tload_start\tload_end\tend\tcake_dl_kbps\tcake_ul_kbps\trecording\n' > "$BLOCKS"

n_modes=$(wc -w <<< "$MODES")
n_policies=$(wc -w <<< "$POLICIES")
total=$((ROUNDS * n_modes * n_policies))
n_both=0
grep -qw both <<< "$MODES" && n_both=$((ROUNDS * n_policies))
log "Start of $total blocks, approximately $(( (total * (IDLE_S + BLOCK_S + 10) + n_both * WARMUP_S) / 60 )) minutes"

block=0
for round in $(seq 1 "$ROUNDS"); do
    for mode in $MODES; do
        for policy in $POLICIES; do
            block=$((block + 1))
            name=$(printf '%02d-%s-%s' "$block" "$mode" "$policy")
            rec="$OUTDIR/$name.jsonl.gz"
            blog="$OUTDIR/$name.log"
            args=(--config "$CONFIG" --record "$rec")
            bin=$BINARY
            case $policy in
                unshaped)
                    set_cake unlimited unlimited
                    cake_dl=unlimited; cake_ul=unlimited
                    args+=(--no-adjust) ;;
                fixed)
                    set_cake "$FIXED_DL" "$FIXED_UL"
                    cake_dl=$FIXED_DL; cake_ul=$FIXED_UL
                    args+=(--no-adjust) ;;
                autorate)
                    cake_dl=autorate; cake_ul=autorate ;;
                autorate-*)
                    vname=${policy#autorate-}
                    args=(--config "${VARIANT_CONFIG[$vname]}" --record "$rec")
                    bin=${VARIANT_BINARY[$vname]}
                    cake_dl=autorate; cake_ul=autorate ;;
                upstream-*)
                    args+=(--no-adjust)
                    cake_dl=upstream; cake_ul=upstream ;;
            esac

            log "Block $block/$total: round $round, $mode, $policy"
            if [ -n "${BLOCK_START_HOOK:-}" ]; then
                $BLOCK_START_HOOK "$block" "$mode" "$policy"
            fi
            start=$(date +%s)
            if [[ $policy == upstream-* ]]; then
                # cake-autorate gets its instance name from config.NAME.sh
                # and writes its log to log_file_path_override.
                uname=${policy#upstream-}
                updir=$(realpath "$OUTDIR")/$name.upstream
                mkdir -p "$updir"
                { cat "${UPSTREAM_CONFIG[$uname]}"; echo; echo "log_file_path_override=\"$updir\""; } \
                    > "$updir/config.$uname.sh"
                CAKE_AUTORATE_SCRIPT_PREFIX="$UPSTREAM_DIR" \
                    bash "$UPSTREAM_DIR/cake-autorate.sh" "$updir/config.$uname.sh" \
                    > "$updir/stdout.log" 2>&1 &
                UP_PID=$!
            fi
            "$bin" "${args[@]}" > "$blog" 2>&1 &
            CAR_PID=$!
            sleep "$IDLE_S"
            kill -0 "$CAR_PID" 2>/dev/null || die "cake-autorate-go stopped. See $blog"
            if [ -n "$UP_PID" ]; then
                kill -0 "$UP_PID" 2>/dev/null || die "cake-autorate stopped. See $OUTDIR/$name.upstream/"
            fi

            if [ "$mode" = both ] && [ "$WARMUP_S" -gt 0 ]; then
                "$LOADGEN" -m dl -w "$WORKERS" -d "$WARMUP_S" -i "$IFACE" >> "$blog" 2>&1 &
                LOAD_PID=$!
                wait "$LOAD_PID" 2>/dev/null || true
                LOAD_PID=""
                kill -0 "$CAR_PID" 2>/dev/null || die "cake-autorate-go stopped during the warm-up. See $blog"
            fi
            load_start=$(date +%s)
            "$LOADGEN" -m "$mode" -w "$WORKERS" -d "$BLOCK_S" -i "$IFACE" >> "$blog" 2>&1 &
            LOAD_PID=$!
            wait "$LOAD_PID" 2>/dev/null || true
            LOAD_PID=""
            load_end=$(date +%s)
            grep -E "elapsed" "$blog" | tail -n 1 | sed 's/^/    /'

            sleep 5
            kill -0 "$CAR_PID" 2>/dev/null || die "cake-autorate-go stopped during the load. See $blog"
            kill -INT "$CAR_PID"
            wait "$CAR_PID" 2>/dev/null || true
            CAR_PID=""
            if [ -n "$UP_PID" ]; then
                kill "$UP_PID" 2>/dev/null || true
                wait "$UP_PID" 2>/dev/null || true
                UP_PID=""
                # cake-autorate leaves its last rate. The next block sets
                # the rate that it needs.
            fi
            end=$(date +%s)

            printf '%d\t%d\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n' \
                "$block" "$round" "$mode" "$policy" "$start" "$load_start" "$load_end" "$end" \
                "$cake_dl" "$cake_ul" "$(basename "$rec")" >> "$BLOCKS"
            if command -v jq >/dev/null; then
                "$SCRIPT_DIR/summarize-session.sh" "$OUTDIR" "$block" | tail -n 1 | sed 's/^/    /' || true
            fi
        done
    done
done

log "All $total blocks are complete."
if command -v jq >/dev/null; then
    echo
    "$SCRIPT_DIR/summarize-session.sh" "$OUTDIR" || true
fi
