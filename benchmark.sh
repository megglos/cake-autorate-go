#!/bin/sh
# benchmark.sh — Compare resource usage of cake-autorate (bash) vs cake-autorate-go
#
# Usage: ./benchmark.sh [duration_seconds]
#   duration_seconds: how long to run each version (default: 120)
#
# Requirements: install procps-ng on OpenWrt for full ps support
#   opkg install procps-ng-ps
#
# Run this on the router with both versions installed.

set -e

DURATION="${1:-120}"
SAMPLE_INTERVAL=5
GO_BIN="${GO_BIN:-/tmp/cake-autorate-go}"
GO_CONFIG="${GO_CONFIG:-/etc/cake-autorate/config.yaml}"
BASH_DIR="${BASH_DIR:-/root/cake-autorate}"
BASH_CONFIG="${BASH_CONFIG:-config.primary.sh}"
RESULTS_DIR="/tmp/cake-autorate-benchmark"

mkdir -p "$RESULTS_DIR"

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

# Collect samples for a running process tree
# Args: $1=label $2=output_file $3=match_pattern
collect_samples() {
    label="$1"
    outfile="$2"
    pattern="$3"
    samples=0
    elapsed=0

    echo "timestamp,num_procs,total_rss_kb,total_cpu_pct" > "$outfile"

    while [ "$elapsed" -lt "$DURATION" ]; do
        sleep "$SAMPLE_INTERVAL"
        elapsed=$((elapsed + SAMPLE_INTERVAL))
        samples=$((samples + 1))

        # Count processes, sum RSS and CPU
        # Use /proc directly as fallback for minimal OpenWrt
        proc_data=$(ps -eo pid,rss,pcpu,args 2>/dev/null | grep -E "$pattern" | grep -v grep || true)

        if [ -n "$proc_data" ]; then
            num_procs=$(echo "$proc_data" | wc -l)
            total_rss=$(echo "$proc_data" | awk '{sum += $2} END {print sum+0}')
            total_cpu=$(echo "$proc_data" | awk '{sum += $3} END {printf "%.1f", sum}')
        else
            num_procs=0
            total_rss=0
            total_cpu="0.0"
        fi

        timestamp=$(date '+%s')
        echo "$timestamp,$num_procs,$total_rss,$total_cpu" >> "$outfile"
        log "  [$label] sample $samples: procs=$num_procs rss=${total_rss}KB cpu=${total_cpu}%"
    done
}

# Summarize a CSV results file
summarize() {
    label="$1"
    file="$2"

    echo ""
    echo "=== $label ==="

    # Skip header, compute averages and peaks
    awk -F, 'NR > 1 {
        n++
        procs += $2; rss += $3; cpu += $4
        if ($2 > max_procs) max_procs = $2
        if ($3 > max_rss) max_rss = $3
        if ($4 > max_cpu) max_cpu = $4
    } END {
        if (n == 0) { print "  No samples collected!"; exit }
        printf "  Samples:      %d\n", n
        printf "  Avg procs:    %.1f  (peak: %d)\n", procs/n, max_procs
        printf "  Avg RSS:      %.0f KB  (peak: %d KB)\n", rss/n, max_rss
        printf "  Avg CPU:      %.2f%%  (peak: %.1f%%)\n", cpu/n, max_cpu
    }' "$file"
}

# ── Stop any running instances ──────────────────────────────────────────────

log "Stopping any running cake-autorate instances..."
killall cake-autorate-go 2>/dev/null || true
# The bash version uses cake-autorate.sh or run_cake_autorate.sh
if [ -f "$BASH_DIR/cake-autorate.sh" ]; then
    cd "$BASH_DIR" && ./cake-autorate.sh stop 2>/dev/null || true
    cd - >/dev/null
fi
killall -g cake-autorate 2>/dev/null || true
sleep 2

# ── Benchmark Go version ───────────────────────────────────────────────────

log "Starting Go version for ${DURATION}s..."
$GO_BIN --config "$GO_CONFIG" &
GO_PID=$!
sleep 2

if ! kill -0 "$GO_PID" 2>/dev/null; then
    log "ERROR: Go version failed to start"
    exit 1
fi

collect_samples "go" "$RESULTS_DIR/go_samples.csv" "cake-autorate-go"

log "Stopping Go version..."
kill "$GO_PID" 2>/dev/null || true
wait "$GO_PID" 2>/dev/null || true
sleep 3

# ── Benchmark Bash version ─────────────────────────────────────────────────

log "Starting bash version for ${DURATION}s..."
if [ -f "$BASH_DIR/cake-autorate.sh" ]; then
    cd "$BASH_DIR"
    ./cake-autorate.sh &
    BASH_PID=$!
    cd - >/dev/null
else
    log "ERROR: Bash version not found at $BASH_DIR/cake-autorate.sh"
    log "Set BASH_DIR to the correct path"
    # Still print Go results
    echo ""
    echo "============================================================"
    echo "  BENCHMARK RESULTS (Go only — bash version not found)"
    echo "  Duration: ${DURATION}s per version, sampled every ${SAMPLE_INTERVAL}s"
    echo "============================================================"
    summarize "cake-autorate-go" "$RESULTS_DIR/go_samples.csv"
    echo ""
    echo "Raw data: $RESULTS_DIR/"
    exit 0
fi
sleep 5

collect_samples "bash" "$RESULTS_DIR/bash_samples.csv" "cake.autorate|fping"

log "Stopping bash version..."
cd "$BASH_DIR"
./cake-autorate.sh stop 2>/dev/null || true
cd - >/dev/null
killall -g cake-autorate 2>/dev/null || true
sleep 2

# ── Results ─────────────────────────────────────────────────────────────────

echo ""
echo "============================================================"
echo "  BENCHMARK RESULTS"
echo "  Duration: ${DURATION}s per version, sampled every ${SAMPLE_INTERVAL}s"
echo "============================================================"

summarize "cake-autorate (bash)" "$RESULTS_DIR/bash_samples.csv"
summarize "cake-autorate-go"     "$RESULTS_DIR/go_samples.csv"

# Side-by-side comparison
echo ""
echo "=== Comparison ==="
awk -F, 'NR > 1 { n++; rss += $3; cpu += $4; procs += $2 } END {
    if (n > 0) { printf "%.0f %.2f %.1f", rss/n, cpu/n, procs/n }
}' "$RESULTS_DIR/bash_samples.csv" | {
    read bash_rss bash_cpu bash_procs 2>/dev/null || true
    awk -F, -v br="${bash_rss:-0}" -v bc="${bash_cpu:-0}" -v bp="${bash_procs:-0}" '
    NR > 1 { n++; rss += $3; cpu += $4; procs += $2 } END {
        if (n == 0 || br == 0) exit
        go_rss = rss/n; go_cpu = cpu/n; go_procs = procs/n
        printf "  Memory:  %.0fx less (bash: %.0f KB, go: %.0f KB)\n", br/go_rss, br, go_rss
        if (go_cpu > 0)
            printf "  CPU:     %.1fx less (bash: %.2f%%, go: %.2f%%)\n", bc/go_cpu, bc, go_cpu
        printf "  Procs:   %.0f vs %.0f\n", bp, go_procs
    }' "$RESULTS_DIR/go_samples.csv"
}

echo ""
echo "Raw data: $RESULTS_DIR/"
