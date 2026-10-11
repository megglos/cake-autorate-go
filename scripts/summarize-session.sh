#!/usr/bin/env bash
# summarize-session.sh: show the bufferbloat and the throughput of each block
# of a session from scripts/record-session.sh.
#
# Usage:
#   ./scripts/summarize-session.sh DIR [BLOCK]
#
#   DIR     the output directory of record-session.sh
#   BLOCK   show only this block (default: all blocks)
#
# For each block, the script uses the loaded time of the block, without the
# first 10 s (TCP ramp-up):
#
#   excess RTT   the RTT of each ping minus the minimum RTT of the same
#                reflector in the block. Each reflector has a different base
#                RTT. Thus, the script does not compare pings of different
#                reflectors. The table shows p50, p95 and p99 in ms.
#   dl, ul       the mean throughput in Mbit/s of the loaded directions.
#   % ref        the throughput as a percentage of the reference:
#                - If DIR/capacity.tsv exists (emulated link): the mean
#                  capacity of the emulated link in the same time.
#                - If not: the mean throughput of the "unshaped" block of the
#                  same round and mode.
#
# Requirements: bash, jq, awk, gzip, date (GNU).

set -euo pipefail

# Use "." as the decimal separator in all locales.
export LC_ALL=C

DIR="${1:-}"
ONLY="${2:-}"
[ -n "$DIR" ] || { sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 1; }
[ -f "$DIR/blocks.tsv" ] || { echo "error: $DIR/blocks.tsv does not exist" >&2; exit 1; }
command -v jq >/dev/null || { echo "error: jq is not installed" >&2; exit 1; }

RAMP_S=10
CAPACITY="$DIR/capacity.tsv"
[ -f "$CAPACITY" ] || CAPACITY=/dev/null

# block_stats prints "p50 p95 p99 dl ul cap_dl cap_ul" for one block. The
# rates are in kbit/s, the RTTs in ms. cap_dl and cap_ul are 0 without a
# capacity file.
block_stats() {
    local rec=$1 load_start=$2 load_end=$3 start_wall
    start_wall=$(date -d "$(zcat -f "$rec" | head -n 1 | jq -r .start)" +%s.%N)
    zcat -f "$rec" | jq -r --argjson s0 "$start_wall" --argjson ls "$load_start" \
        --argjson le "$load_end" --argjson ramp "$RAMP_S" '
        select(.t != null) | ($s0 + .t / 1e6) as $w |
        if .type == "ping" and (.timeout | not) then
            "ping \(.reflector) \(.rtt_us) \(if $w > $ls + $ramp and $w < $le then 1 else 0 end)"
        elif .type == "rate" and $w > $ls + $ramp and $w < $le then
            "rate \($w) \(.dl_kbps) \(.ul_kbps)"
        else empty end' |
    awk -v capfile="$CAPACITY" '
        BEGIN {
            # capacity.tsv: unix_ms dl_kbps ul_kbps, one line for each change
            while ((getline line < capfile) > 0) {
                split(line, f, "\t")
                if (f[1] !~ /^[0-9]+$/) continue
                nc++; ct[nc] = f[1] / 1000; cdl[nc] = f[2]; cul[nc] = f[3]
            }
            ci = 1
        }
        $1 == "ping" {
            if (!($2 in mn) || $3 < mn[$2]) mn[$2] = $3
            if ($4 == 1) { np++; pr[np] = $2; pv[np] = $3 }
        }
        $1 == "rate" {
            nr++; dl += $3; ul += $4
            if (nc > 0) {
                while (ci < nc && ct[ci + 1] <= $2) ci++
                capdl += cdl[ci]; capul += cul[ci]
            }
        }
        # q returns the p-quantile of the sorted array x with n values.
        function q(p,   i) {
            if (n == 0) return 0
            i = int(n * p) + 1
            return x[i > n ? n : i]
        }
        END {
            for (i = 1; i <= np; i++) x[i] = (pv[i] - mn[pr[i]]) / 1000
            n = asort(x)
            if (nr == 0) nr = 1
            printf "%.1f %.1f %.1f %.0f %.0f %.0f %.0f\n",
                q(0.50), q(0.95), q(0.99), dl / nr, ul / nr, capdl / nr, capul / nr
        }'
}

# pct prints a as a percentage of b, or "-" if b is 0.
pct() { awk -v a="$1" -v b="$2" 'BEGIN { if (b > 0) printf "%.0f%%", 100 * a / b; else printf "-" }'; }

printf '%-5s %-4s %-14s | %-18s | %-17s | %-17s\n' "block" "mode" "policy" "excess RTT p50/95/99" "dl Mbit/s (% ref)" "ul Mbit/s (% ref)"
declare -A ref_dl ref_ul
tail -n +2 "$DIR/blocks.tsv" | while IFS=$'\t' read -r block round mode policy _start load_start load_end _end _cdl _cul rec; do
    read -r p50 p95 p99 dl ul cap_dl cap_ul <<< "$(block_stats "$DIR/$rec" "$load_start" "$load_end")"
    if [ "$policy" = "unshaped" ]; then
        ref_dl["$round-$mode"]=$dl; ref_ul["$round-$mode"]=$ul
    fi
    if [ "$CAPACITY" != /dev/null ]; then
        rdl=$cap_dl; rul=$cap_ul
    else
        rdl=${ref_dl["$round-$mode"]:-0}; rul=${ref_ul["$round-$mode"]:-0}
    fi
    [ -n "$ONLY" ] && [ "$block" != "$ONLY" ] && continue
    dl_col="-"; ul_col="-"
    case $mode in dl|both) dl_col=$(printf '%.0f (%s)' "$(awk -v v="$dl" 'BEGIN{print v/1000}')" "$(pct "$dl" "$rdl")") ;; esac
    case $mode in ul|both) ul_col=$(printf '%.0f (%s)' "$(awk -v v="$ul" 'BEGIN{print v/1000}')" "$(pct "$ul" "$rul")") ;; esac
    printf '%-5s %-4s %-14s | %5s %5s %5s ms  | %-17s | %-17s\n' "$block" "$mode" "$policy" "$p50" "$p95" "$p99" "$dl_col" "$ul_col"
done
