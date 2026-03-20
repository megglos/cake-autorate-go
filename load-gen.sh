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
#   -s CHUNK_MB   Download chunk size in MB (default: 100)
#   -h            Show help
#
# Requirements: curl
#
# The script downloads from / uploads to Cloudflare's speed test endpoints,
# which are fast, globally distributed, and don't require accounts.

set -e

DURATION=120
MODE="both"
WORKERS=4
CHUNK_MB=100

usage() {
    sed -n '2,/^$/s/^# //p' "$0"
    exit 0
}

while getopts "d:m:w:s:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
        m) MODE="$OPTARG" ;;
        w) WORKERS="$OPTARG" ;;
        s) CHUNK_MB="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

CHUNK_BYTES=$((CHUNK_MB * 1000000))
DL_URL="https://speed.cloudflare.com/__down?bytes=${CHUNK_BYTES}"
UL_URL="https://speed.cloudflare.com/__up"

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

# Download worker: fetch large chunks in a loop until killed
dl_worker() {
    id=$1
    while true; do
        curl -s -o /dev/null --max-time "$((DURATION + 30))" "$DL_URL" 2>/dev/null || true
    done
}

# Upload worker: POST random data in a loop until killed
ul_worker() {
    id=$1
    # Generate a reusable random payload (1 MB) and POST it repeatedly
    payload_file=$(mktemp)
    dd if=/dev/urandom of="$payload_file" bs=1M count=1 2>/dev/null
    while true; do
        curl -s -o /dev/null --max-time "$((DURATION + 30))" \
            -X POST -H "Content-Type: application/octet-stream" \
            --data-binary "@${payload_file}" "$UL_URL" 2>/dev/null || true
    done
    rm -f "$payload_file"
}

log "Mode: $MODE | Duration: ${DURATION}s | Workers: $WORKERS per direction | Chunk: ${CHUNK_MB}MB"
log "Download URL: $DL_URL"
log "Upload URL:   $UL_URL"
echo ""

# Start download workers
if [ "$MODE" = "dl" ] || [ "$MODE" = "both" ]; then
    log "Starting $WORKERS download workers..."
    for i in $(seq 1 "$WORKERS"); do
        dl_worker "$i" &
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
