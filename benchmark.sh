#!/bin/sh
# benchmark.sh — Compare resource usage and responsiveness of
#                cake-autorate (bash) vs cake-autorate-go
#
# Usage: ./benchmark.sh [options]
#   -d DURATION     Seconds to run each version (default: 120)
#   -i DL_IFACE     Download CAKE interface (default: ifb4eth1)
#   -u UL_IFACE     Upload CAKE interface (default: eth1)
#   -h              Show help
#
# Requirements: install procps-ng on OpenWrt for full ps support
#   opkg install procps-ng-ps
#
# Run this on the router. Start load-gen.sh on the PC ~10s after
# this script begins each version's benchmark phase.
#
# What it measures:
#   - CPU and memory usage (sampled every 5s)
#   - CAKE shaper rate timeline (polled every 50ms via tc)
#   - Decision latency from debug logs (when each version decides to adjust)
#   - Responsiveness: time to first rate change, rate of adjustment

set -e

DURATION=120
SAMPLE_INTERVAL_MS=500
RATE_POLL_MS=50
GO_SERVICE="${GO_SERVICE:-cake-autorate-go}"
BASH_SERVICE="${BASH_SERVICE:-cake-autorate}"
BASH_LOG="${BASH_LOG:-/var/log/cake-autorate.log}"
GO_LOG="${GO_LOG:-/var/log/cake-autorate.log}"
DL_IFACE="${DL_IFACE:-ifb4eth1}"
UL_IFACE="${UL_IFACE:-eth1}"
RESULTS_DIR="/tmp/cake-autorate-benchmark"

usage() {
    sed -n '2,/^$/s/^# //p' "$0"
    exit 0
}

while getopts "d:i:u:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
        i) DL_IFACE="$OPTARG" ;;
        u) UL_IFACE="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

mkdir -p "$RESULTS_DIR"

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

# ── Read current CAKE rate from tc ──────────────────────────────────────────
# Parses "bandwidth NNNNKbit" from tc qdisc show output.
# Returns rate in kbit/s, or 0 on failure.
read_cake_rate() {
    iface="$1"
    line=$(tc qdisc show dev "$iface" root 2>/dev/null) || { echo 0; return; }
    rate=$(echo "$line" | sed -n 's/.*bandwidth \([0-9]*\)[Kk]bit.*/\1/p')
    if [ -z "$rate" ]; then
        # Try Mbit
        rate=$(echo "$line" | sed -n 's/.*bandwidth \([0-9.]*\)[Mm]bit.*/\1/p')
        if [ -n "$rate" ]; then
            # Convert Mbit to Kbit (integer math, good enough)
            rate=$(echo "$rate" | awk '{printf "%d", $1 * 1000}')
        else
            rate=0
        fi
    fi
    echo "$rate"
}

# ── Collect resource usage samples ──────────────────────────────────────────
# Args: $1=label $2=output_file $3=match_pattern
collect_samples() {
    label="$1"
    outfile="$2"
    pattern="$3"
    samples=0
    elapsed_ms=0
    duration_ms=$((DURATION * 1000))
    sleep_sec=$(awk "BEGIN {printf \"%.3f\", $SAMPLE_INTERVAL_MS / 1000.0}")
    # Print a status line every ~5s to avoid flooding the terminal
    status_every=$(( (5000 + SAMPLE_INTERVAL_MS - 1) / SAMPLE_INTERVAL_MS ))

    echo "timestamp_ms,num_procs,total_rss_kb,total_cpu_pct" > "$outfile"

    while [ "$elapsed_ms" -lt "$duration_ms" ]; do
        sleep "$sleep_sec"
        elapsed_ms=$((elapsed_ms + SAMPLE_INTERVAL_MS))
        samples=$((samples + 1))

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

        ts=$(date '+%s%N' 2>/dev/null | cut -c1-13)
        if [ ${#ts} -lt 13 ]; then
            ts=$(date '+%s')000
        fi
        echo "${ts},$num_procs,$total_rss,$total_cpu" >> "$outfile"

        # Periodic status line
        if [ $((samples % status_every)) -eq 0 ]; then
            log "  [$label] ${elapsed_ms}ms/${duration_ms}ms: procs=$num_procs rss=${total_rss}KB cpu=${total_cpu}%"
        fi
    done
}

# ── Poll CAKE rates at high frequency ───────────────────────────────────────
# Runs in background, records dl/ul rates every RATE_POLL_MS.
# Args: $1=output_file
poll_rates() {
    outfile="$1"
    echo "timestamp_ms,dl_rate_kbps,ul_rate_kbps" > "$outfile"

    elapsed_ms=0
    duration_ms=$((DURATION * 1000))

    while [ "$elapsed_ms" -lt "$duration_ms" ]; do
        ts=$(date '+%s%N' 2>/dev/null | cut -c1-13)
        # Fallback for systems without %N (busybox)
        if [ ${#ts} -lt 13 ]; then
            ts=$(date '+%s')000
        fi

        dl_rate=$(read_cake_rate "$DL_IFACE")
        ul_rate=$(read_cake_rate "$UL_IFACE")

        echo "${ts},${dl_rate},${ul_rate}" >> "$outfile"

        # Sleep in milliseconds using awk for fractional sleep
        sleep_sec=$(awk "BEGIN {printf \"%.3f\", $RATE_POLL_MS / 1000.0}")
        sleep "$sleep_sec"
        elapsed_ms=$((elapsed_ms + RATE_POLL_MS))
    done
}

# ── Summarize resource usage CSV ────────────────────────────────────────────
summarize() {
    label="$1"
    file="$2"

    echo ""
    echo "=== $label ==="

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

# ── Analyze rate timeline for responsiveness ────────────────────────────────
analyze_rates() {
    label="$1"
    file="$2"

    echo ""
    echo "--- $label: Rate Responsiveness ---"

    awk -F, '
    NR == 1 { next }
    NR == 2 {
        start_ts = $1
        initial_dl = $2
        initial_ul = $3
        min_dl = $2; max_dl = $2
        min_ul = $3; max_ul = $3
        first_dl_change_ts = 0
        first_ul_change_ts = 0
        dl_changes = 0; ul_changes = 0
        prev_dl = $2; prev_ul = $3
        next
    }
    {
        if ($2 != prev_dl) {
            dl_changes++
            if (first_dl_change_ts == 0 && $2 != initial_dl) first_dl_change_ts = $1
        }
        if ($3 != prev_ul) {
            ul_changes++
            if (first_ul_change_ts == 0 && $3 != initial_ul) first_ul_change_ts = $1
        }
        if ($2 < min_dl) min_dl = $2
        if ($2 > max_dl) max_dl = $2
        if ($3 < min_ul) min_ul = $3
        if ($3 > max_ul) max_ul = $3
        prev_dl = $2; prev_ul = $3
        last_ts = $1
    }
    END {
        if (NR < 3) { print "  Not enough rate samples"; exit }
        duration_s = (last_ts - start_ts) / 1000.0

        printf "  Duration:          %.1fs (%d rate samples)\n", duration_s, NR - 1

        printf "  DL rate range:     %d - %d kbps (initial: %d)\n", min_dl, max_dl, initial_dl
        printf "  UL rate range:     %d - %d kbps (initial: %d)\n", min_ul, max_ul, initial_ul

        printf "  DL rate changes:   %d", dl_changes
        if (first_dl_change_ts > 0)
            printf "  (first change: %.1fs after start)", (first_dl_change_ts - start_ts) / 1000.0
        printf "\n"

        printf "  UL rate changes:   %d", ul_changes
        if (first_ul_change_ts > 0)
            printf "  (first change: %.1fs after start)", (first_ul_change_ts - start_ts) / 1000.0
        printf "\n"

        if (dl_changes > 0)
            printf "  DL adjustments/s:  %.1f\n", dl_changes / duration_s
        if (ul_changes > 0)
            printf "  UL adjustments/s:  %.1f\n", ul_changes / duration_s
    }' "$file"
}

# ── Analyze Go debug logs for decision latency ──────────────────────────────
# Go log format: "2024/01/15 10:30:45.123456 [DEBUG] [link] [DL] bufferbloat: rate 200000 -> 150000 kbps ..."
# Also matches: "high load: rate ... -> ... kbps" and "decay: rate ... -> ... kbps"
analyze_go_log() {
    logfile="$1"
    if [ ! -s "$logfile" ]; then
        echo "  (no Go debug log available)"
        return
    fi

    echo ""
    echo "--- cake-autorate-go: Decision Latency (from debug log) ---"

    grep -E "rate [0-9]+ -> [0-9]+ kbps" "$logfile" | head -1 | {
        read first_line 2>/dev/null || true
        if [ -z "$first_line" ]; then
            echo "  No rate changes found in Go log"
            return
        fi
    }

    # Extract rate change events with timestamps
    grep -E "rate [0-9]+ -> [0-9]+ kbps" "$logfile" | awk '
    {
        # Parse timestamp: "2024/01/15 10:30:45.123456"
        split($2, t, ":")
        split(t[3], s, ".")
        ts = t[1]*3600 + t[2]*60 + s[1] + (length(s) > 1 ? s[2] / 1000000.0 : 0)

        if (NR == 1) first_ts = ts

        # Parse direction [DL] or [UL]
        dir = ""
        for (i = 3; i <= NF; i++) {
            if ($i == "[DL]") dir = "DL"
            if ($i == "[UL]") dir = "UL"
        }

        # Parse type: bufferbloat/high load/decay
        type = "unknown"
        for (i = 3; i <= NF; i++) {
            if ($i == "bufferbloat:") type = "bb"
            if ($i == "high") type = "up"
            if ($i == "decay:") type = "decay"
        }

        total++
        if (type == "bb") bb_count++
        else if (type == "up") up_count++
        else if (type == "decay") decay_count++

        if (dir == "DL") dl_count++
        if (dir == "UL") ul_count++

        last_ts = ts
    }
    END {
        if (total == 0) { print "  No rate changes found in Go log"; exit }
        duration = last_ts - first_ts
        printf "  Total rate decisions:   %d over %.1fs\n", total, duration
        printf "  Breakdown:              %d bufferbloat, %d high-load, %d decay\n", bb_count+0, up_count+0, decay_count+0
        printf "  By direction:           %d DL, %d UL\n", dl_count+0, ul_count+0
        if (duration > 0)
            printf "  Decisions/s:            %.1f\n", total / duration
    }'
}

# ── Analyze bash debug logs for decision latency ────────────────────────────
# Bash log format: "SHAPER; datetime; timestamp; tc qdisc change root dev IFACE cake bandwidth NNNKbit"
analyze_bash_log() {
    logfile="$1"
    if [ ! -s "$logfile" ]; then
        echo "  (no bash debug log available — ensure output_cake_changes=1 in bash config)"
        return
    fi

    echo ""
    echo "--- cake-autorate (bash): Decision Latency (from SHAPER log) ---"

    grep "^SHAPER" "$logfile" | awk -F'; ' '
    {
        total++
        # Extract timestamp (field 3) and rate
        ts = $3 + 0
        if (NR == 1) first_ts = ts

        # Extract interface and rate from tc command
        n = split($4, parts, " ")
        for (i = 1; i <= n; i++) {
            if (parts[i] == "dev") iface = parts[i+1]
            if (parts[i] == "bandwidth") rate = parts[i+1]
        }

        last_ts = ts
    }
    END {
        if (total == 0) { print "  No SHAPER entries found in bash log"; exit }
        duration = last_ts - first_ts
        printf "  Total rate decisions:   %d over %.1fs\n", total, duration
        if (duration > 0)
            printf "  Decisions/s:            %.1f\n", total / duration
    }'
}

# ── Compare rate responsiveness between versions ────────────────────────────
compare_rates() {
    go_file="$1"
    bash_file="$2"

    echo ""
    echo "--- Rate Responsiveness Comparison ---"

    # Extract metrics from each file for comparison
    go_metrics=$(awk -F, '
    NR == 1 { next }
    NR == 2 { start_ts = $1; initial_dl = $2; initial_ul = $3; first_dl = 0; first_ul = 0; dlc = 0; ulc = 0; prev_dl = $2; prev_ul = $3; next }
    {
        if ($2 != prev_dl) { dlc++; if (first_dl == 0 && $2 != initial_dl) first_dl = $1 - start_ts }
        if ($3 != prev_ul) { ulc++; if (first_ul == 0 && $3 != initial_ul) first_ul = $1 - start_ts }
        prev_dl = $2; prev_ul = $3; last_ts = $1
    }
    END {
        dur = (last_ts - start_ts) / 1000.0
        printf "%.0f %.0f %d %d %.1f", first_dl, first_ul, dlc, ulc, dur
    }' "$go_file")

    bash_metrics=$(awk -F, '
    NR == 1 { next }
    NR == 2 { start_ts = $1; initial_dl = $2; initial_ul = $3; first_dl = 0; first_ul = 0; dlc = 0; ulc = 0; prev_dl = $2; prev_ul = $3; next }
    {
        if ($2 != prev_dl) { dlc++; if (first_dl == 0 && $2 != initial_dl) first_dl = $1 - start_ts }
        if ($3 != prev_ul) { ulc++; if (first_ul == 0 && $3 != initial_ul) first_ul = $1 - start_ts }
        prev_dl = $2; prev_ul = $3; last_ts = $1
    }
    END {
        dur = (last_ts - start_ts) / 1000.0
        printf "%.0f %.0f %d %d %.1f", first_dl, first_ul, dlc, ulc, dur
    }' "$bash_file")

    echo "$go_metrics" | {
        read go_first_dl go_first_ul go_dlc go_ulc go_dur 2>/dev/null || true
        echo "$bash_metrics" | {
            read bash_first_dl bash_first_ul bash_dlc bash_ulc bash_dur 2>/dev/null || true

            if [ "${go_first_dl:-0}" -gt 0 ] && [ "${bash_first_dl:-0}" -gt 0 ]; then
                ratio=$(awk "BEGIN {printf \"%.1f\", $bash_first_dl / $go_first_dl}")
                printf "  DL first reaction:  go=%dms  bash=%dms  (go %.1fx faster)\n" \
                    "${go_first_dl}" "${bash_first_dl}" "$ratio"
            fi
            if [ "${go_first_ul:-0}" -gt 0 ] && [ "${bash_first_ul:-0}" -gt 0 ]; then
                ratio=$(awk "BEGIN {printf \"%.1f\", $bash_first_ul / $go_first_ul}")
                printf "  UL first reaction:  go=%dms  bash=%dms  (go %.1fx faster)\n" \
                    "${go_first_ul}" "${bash_first_ul}" "$ratio"
            fi
            if [ "${go_dur:-0}" != "0" ] && [ "${bash_dur:-0}" != "0" ]; then
                go_rate=$(awk "BEGIN {if ($go_dur > 0) printf \"%.1f\", $go_dlc / $go_dur; else print 0}")
                bash_rate=$(awk "BEGIN {if ($bash_dur > 0) printf \"%.1f\", $bash_dlc / $bash_dur; else print 0}")
                printf "  DL adjustments/s:   go=%s  bash=%s\n" "$go_rate" "$bash_rate"
                go_rate=$(awk "BEGIN {if ($go_dur > 0) printf \"%.1f\", $go_ulc / $go_dur; else print 0}")
                bash_rate=$(awk "BEGIN {if ($bash_dur > 0) printf \"%.1f\", $bash_ulc / $bash_dur; else print 0}")
                printf "  UL adjustments/s:   go=%s  bash=%s\n" "$go_rate" "$bash_rate"
            fi
        }
    }
}

# ── Main ────────────────────────────────────────────────────────────────────

log "Benchmark configuration:"
log "  Duration:     ${DURATION}s per version"
log "  DL interface: $DL_IFACE"
log "  UL interface: $UL_IFACE"
log "  Rate polling: every ${RATE_POLL_MS}ms"
log "  Results dir:  $RESULTS_DIR"
echo ""

# Verify tc can read the interfaces
dl_test=$(read_cake_rate "$DL_IFACE")
ul_test=$(read_cake_rate "$UL_IFACE")
if [ "$dl_test" = "0" ] && [ "$ul_test" = "0" ]; then
    log "WARNING: Could not read CAKE rates from $DL_IFACE / $UL_IFACE"
    log "Make sure CAKE qdisc is configured on these interfaces."
    log "Continuing anyway (rate tracking may show all zeros)."
    echo ""
fi

# ── Stop any running instances ──────────────────────────────────────────────

log "Stopping any running cake-autorate instances..."
service "$GO_SERVICE" stop 2>/dev/null || true
service "$BASH_SERVICE" stop 2>/dev/null || true
sleep 2

# ── Benchmark Go version ───────────────────────────────────────────────────

log "=== Phase 1: Go version (${DURATION}s) ==="

# Truncate Go log to isolate this benchmark's output
: > "$GO_LOG" 2>/dev/null || true

log "Starting Go version (service $GO_SERVICE)..."
if ! service "$GO_SERVICE" start 2>/dev/null; then
    log "ERROR: 'service $GO_SERVICE start' failed"
    log "Ensure the $GO_SERVICE init script is installed"
    exit 1
fi
sleep 2

log "Go version running"
log ">>> Start load-gen.sh on the PC now <<<"
echo ""

# Run resource sampling and rate polling in parallel
poll_rates "$RESULTS_DIR/go_rates.csv" &
RATE_PID=$!
collect_samples "go" "$RESULTS_DIR/go_samples.csv" "cake-autorate-go"
kill "$RATE_PID" 2>/dev/null || true
wait "$RATE_PID" 2>/dev/null || true

log "Stopping Go version..."
service "$GO_SERVICE" stop 2>/dev/null || true
cp "$GO_LOG" "$RESULTS_DIR/go_debug.log" 2>/dev/null || true
sleep 3

# ── Benchmark Bash version ─────────────────────────────────────────────────

log "=== Phase 2: Bash version (${DURATION}s) ==="
log "Starting bash version (service $BASH_SERVICE)..."
# Truncate bash log to isolate this benchmark's output
: > "$BASH_LOG" 2>/dev/null || true
if ! service "$BASH_SERVICE" start 2>/dev/null; then
    log "ERROR: 'service $BASH_SERVICE start' failed"
    log "Ensure the $BASH_SERVICE init script is installed"
    echo ""
    echo "============================================================"
    echo "  BENCHMARK RESULTS (Go only — bash service not available)"
    echo "  Duration: ${DURATION}s, sampled every ${SAMPLE_INTERVAL}s"
    echo "============================================================"
    summarize "cake-autorate-go" "$RESULTS_DIR/go_samples.csv"
    analyze_rates "cake-autorate-go" "$RESULTS_DIR/go_rates.csv"
    analyze_go_log "$RESULTS_DIR/go_debug.log"
    echo ""
    echo "Raw data: $RESULTS_DIR/"
    exit 0
fi
sleep 5

log "Bash version running"
log ">>> Start load-gen.sh on the PC now <<<"
echo ""

poll_rates "$RESULTS_DIR/bash_rates.csv" &
RATE_PID=$!
collect_samples "bash" "$RESULTS_DIR/bash_samples.csv" "cake-autorate|fping"
kill "$RATE_PID" 2>/dev/null || true
wait "$RATE_PID" 2>/dev/null || true

log "Stopping bash version..."
service "$BASH_SERVICE" stop 2>/dev/null || true
cp "$BASH_LOG" "$RESULTS_DIR/bash_debug.log" 2>/dev/null || true
sleep 2

# ── Results ─────────────────────────────────────────────────────────────────

echo ""
echo "============================================================"
echo "  BENCHMARK RESULTS"
echo "  Duration: ${DURATION}s per version, sampled every ${SAMPLE_INTERVAL_MS}ms"
echo "  Interfaces: DL=$DL_IFACE  UL=$UL_IFACE"
echo "============================================================"

# Resource usage
summarize "cake-autorate (bash)" "$RESULTS_DIR/bash_samples.csv"
summarize "cake-autorate-go"     "$RESULTS_DIR/go_samples.csv"

# Side-by-side resource comparison
echo ""
echo "=== Resource Comparison ==="
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

# Rate responsiveness
analyze_rates "cake-autorate-go"     "$RESULTS_DIR/go_rates.csv"
analyze_rates "cake-autorate (bash)" "$RESULTS_DIR/bash_rates.csv"
compare_rates "$RESULTS_DIR/go_rates.csv" "$RESULTS_DIR/bash_rates.csv"

# Decision latency from debug logs
analyze_go_log "$RESULTS_DIR/go_debug.log"
analyze_bash_log "$RESULTS_DIR/bash_debug.log"

echo ""
echo "Raw data: $RESULTS_DIR/"
echo "  go_samples.csv   — Go resource usage over time"
echo "  bash_samples.csv — Bash resource usage over time"
echo "  go_rates.csv     — Go CAKE rate timeline (${RATE_POLL_MS}ms resolution)"
echo "  bash_rates.csv   — Bash CAKE rate timeline (${RATE_POLL_MS}ms resolution)"
echo "  go_debug.log     — Go debug log (rate decisions with microsecond timestamps)"
echo "  bash_debug.log   — Bash log (SHAPER entries)"
