#!/bin/sh
# benchmark.sh — Compare resource usage and responsiveness of
#                cake-autorate (bash) vs cake-autorate-go
#
# Usage: ./benchmark.sh [options]
#   -d DURATION     Seconds to run each version (default: 120)
#   -h              Show help
#
# Dual-WAN interface defaults (override via environment variables):
#   PRI_DL_IFACE=ifb4eth1  PRI_UL_IFACE=eth1
#   SEC_DL_IFACE=ifb4eth2  SEC_UL_IFACE=eth2
#
# Requirements: install procps-ng on OpenWrt for full ps support
#   opkg install procps-ng-ps
#
# Run this on the router. Start load-gen.sh on the PC ~10s after
# this script begins each version's benchmark phase.
#
# What it measures:
#   - CPU and memory usage (sampled every 500ms)
#   - CAKE shaper rate timeline per link (polled every 50ms via tc)
#   - Decision latency from debug logs (when each version decides to adjust)
#   - Responsiveness: time to first rate change, rate of adjustment

set -e

DURATION=120
SAMPLE_INTERVAL_MS=500
RATE_POLL_MS=50

# BusyBox sleep only accepts integers. Use usleep (microseconds) if available,
# otherwise fall back to sleep 1.
msleep() {
    ms="$1"
    if command -v usleep >/dev/null 2>&1; then
        usleep $((ms * 1000))
    elif [ "$ms" -ge 1000 ]; then
        sleep $((ms / 1000))
    else
        sleep 1
    fi
}

# ── Service and log configuration ─────────────────────────────────────────
GO_SERVICE="${GO_SERVICE:-cake-autorate-go}"
BASH_SERVICE="${BASH_SERVICE:-cake-autorate}"

# Go uses a single log file for all links
GO_LOG="${GO_LOG:-/var/log/cake-autorate.log}"

# Bash uses per-link log files
BASH_LOG_PRIMARY="${BASH_LOG_PRIMARY:-/var/log/cake-autorate.primary.log}"
BASH_LOG_SECONDARY="${BASH_LOG_SECONDARY:-/var/log/cake-autorate.secondary.log}"

# ── Dual-WAN interface configuration ──────────────────────────────────────
# Primary link
PRI_DL_IFACE="${PRI_DL_IFACE:-ifb4eth1}"
PRI_UL_IFACE="${PRI_UL_IFACE:-eth1}"
# Secondary link
SEC_DL_IFACE="${SEC_DL_IFACE:-ifb4lan1}"
SEC_UL_IFACE="${SEC_UL_IFACE:-lan1}"

SETTLE_TIMEOUT=120
SETTLE_TOLERANCE_PCT=5

# Config file paths (override via environment)
GO_CONFIG="${GO_CONFIG:-/etc/cake-autorate/config.yaml}"
BASH_CONFIG_PRIMARY="${BASH_CONFIG_PRIMARY:-/root/cake-autorate/config.primary.sh}"
BASH_CONFIG_SECONDARY="${BASH_CONFIG_SECONDARY:-/root/cake-autorate/config.secondary.sh}"

RESULTS_DIR="/tmp/cake-autorate-benchmark"

usage() {
    sed -n '2,/^$/s/^# //p' "$0"
    exit 0
}

while getopts "d:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
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
            rate=$(echo "$rate" | awk '{printf "%d", $1 * 1000}')
        else
            rate=0
        fi
    fi
    echo "$rate"
}

# ── Extract base rates from config files ───────────────────────────────────
# Go config: YAML with links[].download.base_rate_kbps / upload.base_rate_kbps
# Returns: "pri_dl pri_ul sec_dl sec_ul" base rates in kbps
get_go_base_rates() {
    config="$1"
    if [ ! -f "$config" ]; then
        log "WARNING: Go config not found at $config"
        echo "0 0 0 0"
        return
    fi
    # Parse YAML — extract base_rate_kbps values per link/direction.
    # Expects links with download/upload sections. Uses simple line-based parsing.
    awk '
    /^[[:space:]]*links:/ { in_links = 1; next }
    in_links && /^[[:space:]]*- name:/ {
        link_idx++
        next
    }
    in_links && /^[[:space:]]*download:/ { section = "dl"; next }
    in_links && /^[[:space:]]*upload:/ { section = "ul"; next }
    in_links && /base_rate_kbps:/ {
        gsub(/[^0-9]/, "", $NF)
        if (link_idx == 1 && section == "dl") pri_dl = $NF
        if (link_idx == 1 && section == "ul") pri_ul = $NF
        if (link_idx == 2 && section == "dl") sec_dl = $NF
        if (link_idx == 2 && section == "ul") sec_ul = $NF
    }
    # Also handle top-level download/upload (single-link config)
    !in_links && /^download:/ { section = "dl"; single = 1; next }
    !in_links && /^upload:/ { section = "ul"; single = 1; next }
    single && /base_rate_kbps:/ {
        gsub(/[^0-9]/, "", $NF)
        if (section == "dl") pri_dl = $NF
        if (section == "ul") pri_ul = $NF
    }
    END {
        printf "%s %s %s %s", pri_dl+0, pri_ul+0, sec_dl+0, sec_ul+0
    }' "$config"
}

# Bash config: shell variables base_dl_shaper_rate_kbps / base_ul_shaper_rate_kbps
# Args: $1=primary_config $2=secondary_config
# Returns: "pri_dl pri_ul sec_dl sec_ul" base rates in kbps
get_bash_base_rates() {
    pri_config="$1"
    sec_config="$2"
    pri_dl=0; pri_ul=0; sec_dl=0; sec_ul=0

    if [ -f "$pri_config" ]; then
        pri_dl=$(sed -n 's/^base_dl_shaper_rate_kbps=\([0-9]*\).*/\1/p' "$pri_config")
        pri_ul=$(sed -n 's/^base_ul_shaper_rate_kbps=\([0-9]*\).*/\1/p' "$pri_config")
    else
        log "WARNING: Bash primary config not found at $pri_config"
    fi

    if [ -f "$sec_config" ]; then
        sec_dl=$(sed -n 's/^base_dl_shaper_rate_kbps=\([0-9]*\).*/\1/p' "$sec_config")
        sec_ul=$(sed -n 's/^base_ul_shaper_rate_kbps=\([0-9]*\).*/\1/p' "$sec_config")
    else
        log "WARNING: Bash secondary config not found at $sec_config"
    fi

    printf "%s %s %s %s" "${pri_dl:-0}" "${pri_ul:-0}" "${sec_dl:-0}" "${sec_ul:-0}"
}

# ── Wait for autorate to settle at base rates ─────────────────────────────
# Polls tc until all interfaces show rates within SETTLE_TOLERANCE_PCT of base.
# Args: $1=pri_dl_base $2=pri_ul_base $3=sec_dl_base $4=sec_ul_base
# Times out after SETTLE_TIMEOUT seconds.
wait_for_settle() {
    base_pri_dl="$1"; base_pri_ul="$2"
    base_sec_dl="$3"; base_sec_ul="$4"
    elapsed=0

    # Skip if we have no base rates to compare against
    if [ "$base_pri_dl" = "0" ] && [ "$base_sec_dl" = "0" ]; then
        log "  No base rates available — skipping settle detection"
        return 0
    fi

    log "  Waiting for rates to settle at base values (timeout: ${SETTLE_TIMEOUT}s, tolerance: ${SETTLE_TOLERANCE_PCT}%)..."
    log "  Target base rates: pri_dl=${base_pri_dl} pri_ul=${base_pri_ul} sec_dl=${base_sec_dl} sec_ul=${base_sec_ul}"

    while [ "$elapsed" -lt "$SETTLE_TIMEOUT" ]; do
        cur_pri_dl=$(read_cake_rate "$PRI_DL_IFACE")
        cur_pri_ul=$(read_cake_rate "$PRI_UL_IFACE")
        cur_sec_dl=$(read_cake_rate "$SEC_DL_IFACE")
        cur_sec_ul=$(read_cake_rate "$SEC_UL_IFACE")

        settled=1
        for pair in \
            "$cur_pri_dl $base_pri_dl" \
            "$cur_pri_ul $base_pri_ul" \
            "$cur_sec_dl $base_sec_dl" \
            "$cur_sec_ul $base_sec_ul"
        do
            set -- $pair
            current="$1"; base="$2"
            [ "$base" = "0" ] && continue  # skip unconfigured links
            if ! awk "BEGIN { diff = ($current - $base); if (diff < 0) diff = -diff; exit (diff <= $base * $SETTLE_TOLERANCE_PCT / 100) ? 0 : 1 }"; then
                settled=0
                break
            fi
        done

        if [ "$settled" = "1" ]; then
            log "  Settled at base rates after ${elapsed}s (pri_dl=${cur_pri_dl} pri_ul=${cur_pri_ul} sec_dl=${cur_sec_dl} sec_ul=${cur_sec_ul})"
            return 0
        fi

        sleep 2
        elapsed=$((elapsed + 2))

        if [ $((elapsed % 10)) -eq 0 ]; then
            log "  Still settling... ${elapsed}s (pri_dl=${cur_pri_dl}/${base_pri_dl} pri_ul=${cur_pri_ul}/${base_pri_ul} sec_dl=${cur_sec_dl}/${base_sec_dl} sec_ul=${cur_sec_ul}/${base_sec_ul})"
        fi
    done

    log "  WARNING: Settle timeout (${SETTLE_TIMEOUT}s) — proceeding with current rates"
    return 1
}

# ── Collect resource usage samples ──────────────────────────────────────────
# Args: $1=label $2=output_file $3=match_pattern
collect_samples() {
    label="$1"
    outfile="$2"
    pattern="$3"
    samples=0
    start_epoch=$(date '+%s')
    last_status_s=0

    echo "timestamp_ms,num_procs,total_rss_kb,total_cpu_pct" > "$outfile"

    while true; do
        msleep "$SAMPLE_INTERVAL_MS"

        now_epoch=$(date '+%s')
        elapsed_s=$((now_epoch - start_epoch))
        [ "$elapsed_s" -ge "$DURATION" ] && break

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

        if [ $((elapsed_s - last_status_s)) -ge 5 ]; then
            last_status_s=$elapsed_s
            log "  [$label] ${elapsed_s}s/${DURATION}s: procs=$num_procs rss=${total_rss}KB cpu=${total_cpu}%"
        fi
    done
}

# ── Poll CAKE rates at high frequency (dual-WAN) ──────────────────────────
# Records primary + secondary DL/UL rates every RATE_POLL_MS.
# Args: $1=output_file
poll_rates() {
    outfile="$1"
    echo "timestamp_ms,pri_dl_kbps,pri_ul_kbps,sec_dl_kbps,sec_ul_kbps" > "$outfile"

    start_epoch=$(date '+%s')

    while true; do
        now_epoch=$(date '+%s')
        [ $((now_epoch - start_epoch)) -ge "$DURATION" ] && break

        ts=$(date '+%s%N' 2>/dev/null | cut -c1-13)
        if [ ${#ts} -lt 13 ]; then
            ts=$(date '+%s')000
        fi

        pri_dl=$(read_cake_rate "$PRI_DL_IFACE")
        pri_ul=$(read_cake_rate "$PRI_UL_IFACE")
        sec_dl=$(read_cake_rate "$SEC_DL_IFACE")
        sec_ul=$(read_cake_rate "$SEC_UL_IFACE")

        echo "${ts},${pri_dl},${pri_ul},${sec_dl},${sec_ul}" >> "$outfile"

        msleep "$RATE_POLL_MS"
    done
}

# ── Summarize resource usage CSV ──────────────────────────────────────────
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

# ── Analyze rate timeline for one link ────────────────────────────────────
# Args: $1=label $2=csv_file $3=dl_column $4=ul_column
analyze_rates_link() {
    label="$1"
    file="$2"
    dl_col="$3"
    ul_col="$4"

    echo ""
    echo "--- $label: Rate Responsiveness ---"

    awk -F, -v dl_col="$dl_col" -v ul_col="$ul_col" '
    NR == 1 { next }
    NR == 2 {
        start_ts = $1
        initial_dl = $dl_col; initial_ul = $ul_col
        min_dl = $dl_col; max_dl = $dl_col
        min_ul = $ul_col; max_ul = $ul_col
        first_dl_change_ts = 0; first_ul_change_ts = 0
        dl_changes = 0; ul_changes = 0
        prev_dl = $dl_col; prev_ul = $ul_col
        next
    }
    {
        if ($dl_col != prev_dl) {
            dl_changes++
            if (first_dl_change_ts == 0 && $dl_col != initial_dl) first_dl_change_ts = $1
        }
        if ($ul_col != prev_ul) {
            ul_changes++
            if (first_ul_change_ts == 0 && $ul_col != initial_ul) first_ul_change_ts = $1
        }
        if ($dl_col < min_dl) min_dl = $dl_col
        if ($dl_col > max_dl) max_dl = $dl_col
        if ($ul_col < min_ul) min_ul = $ul_col
        if ($ul_col > max_ul) max_ul = $ul_col
        prev_dl = $dl_col; prev_ul = $ul_col
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
# Go log format: "2026/03/21 13:30:49.409860 [DEBUG] [link] [dl] high load: rate 389505 -> 393400 kbps ..."
analyze_go_log() {
    logfile="$1"
    link_filter="$2"  # e.g. "primary" or "secondary"
    if [ ! -s "$logfile" ]; then
        echo "  (no Go debug log available)"
        return
    fi

    echo ""
    echo "--- cake-autorate-go [$link_filter]: Decision Latency ---"

    grep -E "\\[$link_filter\\].*rate [0-9]+ -> [0-9]+ kbps" "$logfile" | awk '
    {
        split($2, t, ":")
        split(t[3], s, ".")
        ts = t[1]*3600 + t[2]*60 + s[1] + (length(s) > 1 ? s[2] / 1000000.0 : 0)

        if (NR == 1) first_ts = ts

        dir = ""
        for (i = 3; i <= NF; i++) {
            if ($i == "[dl]") dir = "DL"
            if ($i == "[ul]") dir = "UL"
        }

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
    link_name="$2"
    if [ ! -s "$logfile" ]; then
        echo "  (no bash $link_name log — ensure output_cake_changes=1 in bash config)"
        return
    fi

    echo ""
    echo "--- cake-autorate (bash) [$link_name]: Decision Latency ---"

    grep "^SHAPER" "$logfile" | awk -F'; ' '
    {
        total++
        ts = $3 + 0
        if (NR == 1) first_ts = ts

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

# ── Compare rate responsiveness between versions (per link) ──────────────
compare_rates_link() {
    go_file="$1"
    bash_file="$2"
    dl_col="$3"
    ul_col="$4"
    link_name="$5"

    echo ""
    echo "--- Rate Responsiveness Comparison [$link_name] ---"

    go_metrics=$(awk -F, -v dc="$dl_col" -v uc="$ul_col" '
    NR == 1 { next }
    NR == 2 { start_ts = $1; initial_dl = $dc; initial_ul = $uc; first_dl = 0; first_ul = 0; dlc = 0; ulc = 0; prev_dl = $dc; prev_ul = $uc; next }
    {
        if ($dc != prev_dl) { dlc++; if (first_dl == 0 && $dc != initial_dl) first_dl = $1 - start_ts }
        if ($uc != prev_ul) { ulc++; if (first_ul == 0 && $uc != initial_ul) first_ul = $1 - start_ts }
        prev_dl = $dc; prev_ul = $uc; last_ts = $1
    }
    END {
        dur = (last_ts - start_ts) / 1000.0
        printf "%.0f %.0f %d %d %.1f", first_dl, first_ul, dlc, ulc, dur
    }' "$go_file")

    bash_metrics=$(awk -F, -v dc="$dl_col" -v uc="$ul_col" '
    NR == 1 { next }
    NR == 2 { start_ts = $1; initial_dl = $dc; initial_ul = $uc; first_dl = 0; first_ul = 0; dlc = 0; ulc = 0; prev_dl = $dc; prev_ul = $uc; next }
    {
        if ($dc != prev_dl) { dlc++; if (first_dl == 0 && $dc != initial_dl) first_dl = $1 - start_ts }
        if ($uc != prev_ul) { ulc++; if (first_ul == 0 && $uc != initial_ul) first_ul = $1 - start_ts }
        prev_dl = $dc; prev_ul = $uc; last_ts = $1
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
                awk "BEGIN {
                    g = $go_first_dl; b = $bash_first_dl
                    if (g < b) printf \"  DL first reaction:  go=%dms  bash=%dms  (go %.1fx faster)\n\", g, b, b/g
                    else if (b < g) printf \"  DL first reaction:  go=%dms  bash=%dms  (bash %.1fx faster)\n\", g, b, g/b
                    else printf \"  DL first reaction:  go=%dms  bash=%dms  (same)\n\", g, b
                }"
            fi
            if [ "${go_first_ul:-0}" -gt 0 ] && [ "${bash_first_ul:-0}" -gt 0 ]; then
                awk "BEGIN {
                    g = $go_first_ul; b = $bash_first_ul
                    if (g < b) printf \"  UL first reaction:  go=%dms  bash=%dms  (go %.1fx faster)\n\", g, b, b/g
                    else if (b < g) printf \"  UL first reaction:  go=%dms  bash=%dms  (bash %.1fx faster)\n\", g, b, g/b
                    else printf \"  UL first reaction:  go=%dms  bash=%dms  (same)\n\", g, b
                }"
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
log "  Duration:       ${DURATION}s per version"
log "  Primary link:   DL=$PRI_DL_IFACE  UL=$PRI_UL_IFACE"
log "  Secondary link: DL=$SEC_DL_IFACE  UL=$SEC_UL_IFACE"
log "  Rate polling:   every ${RATE_POLL_MS}ms"
log "  Results dir:    $RESULTS_DIR"
echo ""

# Verify tc can read the interfaces
pri_dl_test=$(read_cake_rate "$PRI_DL_IFACE")
pri_ul_test=$(read_cake_rate "$PRI_UL_IFACE")
sec_dl_test=$(read_cake_rate "$SEC_DL_IFACE")
sec_ul_test=$(read_cake_rate "$SEC_UL_IFACE")

if [ "$pri_dl_test" = "0" ] && [ "$pri_ul_test" = "0" ]; then
    log "WARNING: Could not read CAKE rates from primary ($PRI_DL_IFACE / $PRI_UL_IFACE)"
fi
if [ "$sec_dl_test" = "0" ] && [ "$sec_ul_test" = "0" ]; then
    log "WARNING: Could not read CAKE rates from secondary ($SEC_DL_IFACE / $SEC_UL_IFACE)"
fi
if [ "$pri_dl_test" = "0" ] && [ "$sec_dl_test" = "0" ]; then
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

log "Go version running — waiting for settle..."
go_base_rates=$(get_go_base_rates "$GO_CONFIG")
set -- $go_base_rates
wait_for_settle "$1" "$2" "$3" "$4"

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

# Truncate bash logs to isolate this benchmark's output
: > "$BASH_LOG_PRIMARY" 2>/dev/null || true
: > "$BASH_LOG_SECONDARY" 2>/dev/null || true

if ! service "$BASH_SERVICE" start 2>/dev/null; then
    log "ERROR: 'service $BASH_SERVICE start' failed"
    log "Ensure the $BASH_SERVICE init script is installed"
    echo ""
    echo "============================================================"
    echo "  BENCHMARK RESULTS (Go only — bash service not available)"
    echo "  Duration: ${DURATION}s, sampled every ${SAMPLE_INTERVAL_MS}ms"
    echo "  Primary:   DL=$PRI_DL_IFACE  UL=$PRI_UL_IFACE"
    echo "  Secondary: DL=$SEC_DL_IFACE  UL=$SEC_UL_IFACE"
    echo "============================================================"
    summarize "cake-autorate-go" "$RESULTS_DIR/go_samples.csv"
    analyze_rates_link "cake-autorate-go [primary]" "$RESULTS_DIR/go_rates.csv" 2 3
    analyze_rates_link "cake-autorate-go [secondary]" "$RESULTS_DIR/go_rates.csv" 4 5
    analyze_go_log "$RESULTS_DIR/go_debug.log" "primary"
    analyze_go_log "$RESULTS_DIR/go_debug.log" "secondary"
    echo ""
    echo "Raw data: $RESULTS_DIR/"
    exit 0
fi
sleep 5

log "Bash version running — waiting for settle..."
bash_base_rates=$(get_bash_base_rates "$BASH_CONFIG_PRIMARY" "$BASH_CONFIG_SECONDARY")
set -- $bash_base_rates
wait_for_settle "$1" "$2" "$3" "$4"

log ">>> Start load-gen.sh on the PC now <<<"
echo ""

poll_rates "$RESULTS_DIR/bash_rates.csv" &
RATE_PID=$!
collect_samples "bash" "$RESULTS_DIR/bash_samples.csv" "cake-autorate|fping"
kill "$RATE_PID" 2>/dev/null || true
wait "$RATE_PID" 2>/dev/null || true

log "Stopping bash version..."
service "$BASH_SERVICE" stop 2>/dev/null || true
cp "$BASH_LOG_PRIMARY" "$RESULTS_DIR/bash_primary_debug.log" 2>/dev/null || true
cp "$BASH_LOG_SECONDARY" "$RESULTS_DIR/bash_secondary_debug.log" 2>/dev/null || true
sleep 2

# ── Results ─────────────────────────────────────────────────────────────────

echo ""
echo "============================================================"
echo "  BENCHMARK RESULTS"
echo "  Duration: ${DURATION}s per version, sampled every ${SAMPLE_INTERVAL_MS}ms"
echo "  Primary:   DL=$PRI_DL_IFACE  UL=$PRI_UL_IFACE"
echo "  Secondary: DL=$SEC_DL_IFACE  UL=$SEC_UL_IFACE"
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

# Rate responsiveness — per link
analyze_rates_link "cake-autorate-go [primary]"     "$RESULTS_DIR/go_rates.csv" 2 3
analyze_rates_link "cake-autorate-go [secondary]"   "$RESULTS_DIR/go_rates.csv" 4 5
analyze_rates_link "cake-autorate (bash) [primary]"   "$RESULTS_DIR/bash_rates.csv" 2 3
analyze_rates_link "cake-autorate (bash) [secondary]" "$RESULTS_DIR/bash_rates.csv" 4 5

# Compare go vs bash per link
compare_rates_link "$RESULTS_DIR/go_rates.csv" "$RESULTS_DIR/bash_rates.csv" 2 3 "primary"
compare_rates_link "$RESULTS_DIR/go_rates.csv" "$RESULTS_DIR/bash_rates.csv" 4 5 "secondary"

# Decision latency from debug logs — per link
analyze_go_log "$RESULTS_DIR/go_debug.log" "primary"
analyze_go_log "$RESULTS_DIR/go_debug.log" "secondary"
analyze_bash_log "$RESULTS_DIR/bash_primary_debug.log" "primary"
analyze_bash_log "$RESULTS_DIR/bash_secondary_debug.log" "secondary"

echo ""
echo "Raw data: $RESULTS_DIR/"
echo "  go_samples.csv            — Go resource usage over time"
echo "  bash_samples.csv          — Bash resource usage over time"
echo "  go_rates.csv              — Go CAKE rate timeline, both links (${RATE_POLL_MS}ms resolution)"
echo "  bash_rates.csv            — Bash CAKE rate timeline, both links (${RATE_POLL_MS}ms resolution)"
echo "  go_debug.log              — Go debug log (rate decisions with microsecond timestamps)"
echo "  bash_primary_debug.log    — Bash primary link log (SHAPER entries)"
echo "  bash_secondary_debug.log  — Bash secondary link log (SHAPER entries)"
