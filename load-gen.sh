#!/usr/bin/env bash
# load-gen.sh — Generate sustained internet traffic to saturate a router's WAN link
#
# Run this on a PC connected to the router's LAN. Traffic flows through the
# router's WAN interfaces into the internet, triggering cake-autorate shaping.
#
# Usage: ./load-gen.sh [options]
#   -d DURATION   Duration in seconds (default: 120)
#   -m MODE       Load mode: dl, ul, both (default: both)
#   -w WORKERS    Parallel workers per direction (default: 8)
#   -i IFACE      Show the throughput of this interface every 10 s
#   -h            Show help
#
# Requirements: curl
#
# Download workers fetch large test files from many distinct speed test servers
# so that mwan3 sticky sessions distribute connections across all WAN links.
# Upload workers POST to Cloudflare's speed test upload endpoint.
# Override URLs with DL_URLS (space-separated) and UL_URL env vars.
# UL_SIZE_MB sets the size of each upload (default: 25). Large uploads
# spend less time in TCP slow start.

set -e

DURATION=120
MODE="both"
WORKERS=8

# Speed test file servers in Europe that send at more than 500 Mbit/s on one
# connection (tested 2026-10). The files are 1 GB, so each connection spends
# little time in TCP slow start. Each new connection gets a new source port,
# which changes the mwan3 hash and spreads the connections across the WAN links.
DEFAULT_DL_URLS="https://fsn1-speed.hetzner.com/1GB.bin https://nbg1-speed.hetzner.com/1GB.bin https://hel1-speed.hetzner.com/1GB.bin http://speedtest.serverius.net/files/1000mb.bin http://fra-de-ping.vultr.com/vultr.com.1000MB.bin http://ams-nl-ping.vultr.com/vultr.com.1000MB.bin http://par-fr-ping.vultr.com/vultr.com.1000MB.bin"
DL_URLS="${DL_URLS:-$DEFAULT_DL_URLS}"
UL_URL="${UL_URL:-https://speed.cloudflare.com/__up}"
UL_SIZE_MB="${UL_SIZE_MB:-25}"
IFACE=""

usage() {
    sed -n '2,/^$/s/^# //p' "$0"
    exit 0
}

while getopts "d:m:w:i:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
        m) MODE="$OPTARG" ;;
        w) WORKERS="$OPTARG" ;;
        i) IFACE="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

# Convert DL_URLS string to an array
set -f  # disable globbing
dl_urls_arr=()
for u in $DL_URLS; do
    dl_urls_arr+=("$u")
done
set +f
dl_url_count=${#dl_urls_arr[@]}

if [ -n "$IFACE" ] && [ ! -d "/sys/class/net/$IFACE" ]; then
    echo "ERROR: interface $IFACE does not exist" >&2
    exit 1
fi

if [ "$dl_url_count" -eq 0 ]; then
    echo "ERROR: No download URLs configured" >&2
    exit 1
fi

# Track child PIDs for cleanup
PIDS=""
PAYLOAD_FILE=""
cleanup() {
    echo ""
    echo "[load-gen] Stopping all workers..."
    for pid in $PIDS; do
        pkill -P "$pid" 2>/dev/null || true
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    [ -n "$PAYLOAD_FILE" ] && rm -f "$PAYLOAD_FILE"
    echo "[load-gen] Done."
}
trap cleanup EXIT INT TERM

log() {
    echo "[load-gen] [$(date '+%H:%M:%S')] $*"
}

# Download worker: fetch large file in a loop until killed
# Each worker gets a URL assigned round-robin from dl_urls_arr.
# After a failure, wait 1 s. Without the wait, a server that is not
# available causes a fast loop of failed requests.
dl_worker() {
    url=$1
    while true; do
        curl -s -L -o /dev/null --max-time "$((DURATION + 30))" "$url" 2>/dev/null || sleep 1
    done
}

# Upload worker: POST random data in a loop until killed
ul_worker() {
    while true; do
        curl -s -o /dev/null --max-time "$((DURATION + 30))" \
            -X POST -H "Content-Type: application/octet-stream" \
            --data-binary "@${PAYLOAD_FILE}" "$UL_URL" 2>/dev/null || sleep 1
    done
}

# read_bytes prints the received and the transmitted bytes of IFACE.
read_bytes() {
    echo "$(cat "/sys/class/net/$IFACE/statistics/rx_bytes") $(cat "/sys/class/net/$IFACE/statistics/tx_bytes")"
}

log "Mode: $MODE | Duration: ${DURATION}s | Workers: $WORKERS per direction"
log "Download URLs:"
for u in "${dl_urls_arr[@]}"; do
    log "  $u"
done
log "Upload URL: $UL_URL"
echo ""

# Start download workers (round-robin across URLs)
if [ "$MODE" = "dl" ] || [ "$MODE" = "both" ]; then
    log "Starting $WORKERS download workers..."
    for i in $(seq 1 "$WORKERS"); do
        idx=$(( (i - 1) % dl_url_count ))
        url="${dl_urls_arr[$idx]}"
        dl_worker "$url" &
        PIDS="$PIDS $!"
    done
fi

# Start upload workers
if [ "$MODE" = "ul" ] || [ "$MODE" = "both" ]; then
    PAYLOAD_FILE=$(mktemp)
    dd if=/dev/urandom of="$PAYLOAD_FILE" bs=1M count="$UL_SIZE_MB" 2>/dev/null
    log "Starting $WORKERS upload workers (${UL_SIZE_MB} MB for each upload)..."
    for i in $(seq 1 "$WORKERS"); do
        ul_worker &
        PIDS="$PIDS $!"
    done
fi

log "Load generation running for ${DURATION}s..."
log "Press Ctrl+C to stop early."
echo ""

# Wait for duration, printing periodic status
elapsed=0
interval=10
[ -n "$IFACE" ] && prev_bytes=$(read_bytes)
while [ "$elapsed" -lt "$DURATION" ]; do
    remaining=$((DURATION - elapsed))
    if [ "$remaining" -lt "$interval" ]; then
        slept=$remaining
    else
        slept=$interval
    fi
    # Wait in the background, so that a signal stops the script immediately.
    sleep "$slept" &
    wait $!
    elapsed=$((elapsed + slept))
    active=$(echo "$PIDS" | tr ' ' '\n' | while read pid; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && echo "$pid"
    done | wc -l)
    if [ -n "$IFACE" ]; then
        bytes=$(read_bytes)
        # shellcheck disable=SC2086
        set -- $prev_bytes $bytes
        rate=$(awk -v rx0="$1" -v tx0="$2" -v rx1="$3" -v tx1="$4" -v s="$slept" \
            'BEGIN { printf "dl %.1f Mbit/s, ul %.1f Mbit/s", (rx1-rx0)*8/s/1e6, (tx1-tx0)*8/s/1e6 }')
        prev_bytes=$bytes
        log "${elapsed}/${DURATION}s elapsed, $active workers active, $IFACE: $rate"
    else
        log "${elapsed}/${DURATION}s elapsed, $active workers active"
    fi
done

log "Duration reached, stopping."
# cleanup runs via trap
