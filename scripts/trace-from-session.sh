#!/usr/bin/env bash
# trace-from-session.sh: make a capacity trace from two unshaped blocks of a
# session (scripts/record-session.sh).
#
# In an unshaped block, the load fills the link. Thus, the achieved rate is
# approximately the capacity of the link. The script takes the achieved
# download rate of a dl block and the achieved upload rate of a ul block,
# during the load. It writes the mean of each second, at each rate sample
# (200 ms). The first 3 s of the load are not used (TCP slow start).
#
# Usage:
#   ./scripts/trace-from-session.sh SESSION_DIR DL_BLOCK UL_BLOCK > trace.csv
#
#   DL_BLOCK, UL_BLOCK   block numbers of unshaped blocks (column "block" in
#                        blocks.tsv) with the modes dl and ul.
#
# Output: CSV with the header "t_ms,dl_kbps,ul_kbps", the same format as
# scripts/gen-trace.sh. The simulator (scenario 5g-recorded) and
# scripts/emulate-link.sh use it. The trace is as long as the shorter block.
#
# Limit: the achieved rate is the capacity only if the load fills the link.
# If the load generator or the servers limit the rate, the trace is too low.
#
# Requirements: jq, awk, gzip.

set -euo pipefail

[ $# -eq 3 ] || { sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 1; }
DIR=$1 DL_BLOCK=$2 UL_BLOCK=$3

die() { echo "error: $*" >&2; exit 1; }
[ -f "$DIR/blocks.tsv" ] || die "$DIR/blocks.tsv does not exist"

# rates BLOCK MODE FIELD: print "t_ms rate" for each rate sample in the load
# time of the block. t_ms starts at 0.
rates() {
    local block=$1 mode=$2 field=$3 line
    line=$(awk -F'\t' -v b="$block" '$1 == b' "$DIR/blocks.tsv")
    [ -n "$line" ] || die "block $block is not in blocks.tsv"
    IFS=$'\t' read -r _ _ bmode policy start load_start load_end _ _ _ rec <<< "$line"
    [ "$bmode" = "$mode" ] || die "block $block has the mode $bmode, not $mode"
    [ "$policy" = "unshaped" ] || die "block $block has the policy $policy, not unshaped"
    # The recording time t (us) starts approximately at the block start.
    local from=$(( (load_start - start + 3) * 1000000 )) to=$(( (load_end - start) * 1000000 ))
    gzip -dc "$DIR/$rec" |
        jq -r --argjson from "$from" --argjson to "$to" --arg f "$field" \
            'select(.type == "rate" and .t >= $from and .t < $to) | "\((.t - $from) / 1000 | floor) \(.[$f])"'
}

# smooth: mean of the samples in the last second, at each sample.
smooth() {
    awk '{ t[NR] = $1; v[NR] = $2
           s = 0; n = 0
           for (i = NR; i >= 1 && t[NR] - t[i] < 1000; i--) { s += v[i]; n++ }
           printf "%d %d\n", $1, s / n }'
}

dl=$(rates "$DL_BLOCK" dl dl_kbps | smooth)
ul=$(rates "$UL_BLOCK" ul ul_kbps | smooth)

echo "t_ms,dl_kbps,ul_kbps"
# Use the time of the dl samples. For each, use the last ul sample before it.
awk 'NR == FNR { ut[++n] = $1; uv[n] = $2; next }
     { while (j < n && ut[j + 1] <= $1) j++
       if (j == 0) next
       if ($1 > ut[n]) exit
       printf "%d,%d,%d\n", $1 - t0, $2, uv[j]
     }' <(echo "$ul") <(echo "$dl") | awk -F, 'NR == 1 { t0 = $1 } { printf "%d,%d,%d\n", $1 - t0, $2, $3 }'
