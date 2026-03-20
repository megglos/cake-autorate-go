#!/usr/bin/env bash
# load-gen.sh — Generate sustained internet traffic to saturate a router's WAN link
#
# Run this on a PC connected to the router's LAN. Traffic flows through the
# router's WAN interfaces into the internet, triggering cake-autorate shaping.
#
# Usage: ./load-gen.sh [options]
#   -d DURATION   Duration in seconds (default: 120)
#   -m MODE       Load mode: dl, ul, both (default: both)
#   -w WORKERS    Parallel workers per direction (default: 4)
#   -h            Show help
#
# Requirements: curl
#
# Download workers fetch large test files from well-known speed test servers
# (Hetzner, OVH, Tele2) that serve at full speed without throttling.
# Upload workers POST to Cloudflare's speed test upload endpoint.
# Override URLs with DL_URLS (space-separated) and UL_URL env vars.

set -e

DURATION=120
MODE="both"
WORKERS=4

# Well-known speed test file servers that serve at full line rate.
# Workers round-robin across these to spread load / avoid single-server limits.
DEFAULT_DL_URLS="https://speed.hetzner.de/1GB.bin http://speedtest.tele2.net/1GB.zip http://proof.ovh.net/files/1Gio.dat"
DL_URLS="${DL_URLS:-$DEFAULT_DL_URLS}"
UL_URL="${UL_URL:-https://speed.cloudflare.com/__up}"

usage() {
    sed -n '2,/^$/s/^# //p' "$0"
    exit 0
}

while getopts "d:m:w:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
        m) MODE="$OPTARG" ;;
        w) WORKERS="$OPTARG" ;;
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

if [ "$dl_url_count" -eq 0 ]; then
    echo "ERROR: No download URLs configured" >&2
    exit 1
fi

# Track child PIDs for cleanup
PIDS=""
cleanup() {
    echo ""
    echo "[load-gen] Stopping all workers..."
    for pid in $PIDS; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    echo "[load-gen] Done."
}
trap cleanup EXIT INT TERM

log() {
    echo "[load-gen] [$(date '+%H:%M:%S')] $*"
}

# Download worker: fetch large file in a loop until killed
# Each worker gets a URL assigned round-robin from dl_urls_arr
dl_worker() {
    url=$1
    while true; do
        curl -s -L -o /dev/null --max-time "$((DURATION + 30))" "$url" 2>/dev/null || true
    done
}

# Upload worker: POST random data in a loop until killed
ul_worker() {
    id=$1
    payload_file=$(mktemp)
    dd if=/dev/urandom of="$payload_file" bs=1M count=1 2>/dev/null
    while true; do
        curl -s -o /dev/null --max-time "$((DURATION + 30))" \
            -X POST -H "Content-Type: application/octet-stream" \
            --data-binary "@${payload_file}" "$UL_URL" 2>/dev/null || true
    done
    rm -f "$payload_file"
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
    log "Starting $WORKERS upload workers..."
    for i in $(seq 1 "$WORKERS"); do
        ul_worker "$i" &
        PIDS="$PIDS $!"
    done
fi

log "Load generation running for ${DURATION}s..."
log "Press Ctrl+C to stop early."
echo ""

# Wait for duration, printing periodic status
elapsed=0
interval=10
while [ "$elapsed" -lt "$DURATION" ]; do
    remaining=$((DURATION - elapsed))
    if [ "$remaining" -lt "$interval" ]; then
        sleep "$remaining"
        elapsed=$DURATION
    else
        sleep "$interval"
        elapsed=$((elapsed + interval))
    fi
    active=$(echo "$PIDS" | tr ' ' '\n' | while read pid; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && echo "$pid"
    done | wc -l)
    log "${elapsed}/${DURATION}s elapsed, $active workers active"
done

log "Duration reached, stopping."
# cleanup runs via trap
