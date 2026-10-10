#!/bin/sh
# gen-trace.sh: make a synthetic capacity trace for scripts/emulate-link.sh.
#
# The capacity of each direction is a random walk between a minimum and a
# maximum. At each step, the capacity changes by a random factor. Sometimes,
# the capacity falls to the minimum for 1 to 3 s (a deep fade: cell edge,
# handover, blockage). The same seed gives the same trace.
#
# Usage:
#   ./scripts/gen-trace.sh [options] > trace.csv
#
# Options:
#   -d SECONDS   duration (default: 300)
#   -s MS        time between two changes (default: 200)
#   -D LO,HI     download minimum and maximum in kbit/s (default: 30000,300000)
#   -U LO,HI     upload minimum and maximum in kbit/s (default: 5000,50000)
#   -v VALUE     volatility: standard deviation of the log change for each
#                step (default: 0.08)
#   -f VALUE     probability of a deep fade for each step (default: 0.004)
#   -r SEED      random seed (default: 1)
#   -h           show this help
#
# Output: CSV with the header "t_ms,dl_kbps,ul_kbps". Each line gives the
# capacity from t_ms until the next line.

set -e

DURATION=300
STEP_MS=200
DL="30000,300000"
UL="5000,50000"
VOLATILITY=0.08
FADE=0.004
SEED=1

while getopts "d:s:D:U:v:f:r:h" opt; do
    case $opt in
        d) DURATION="$OPTARG" ;;
        s) STEP_MS="$OPTARG" ;;
        D) DL="$OPTARG" ;;
        U) UL="$OPTARG" ;;
        v) VOLATILITY="$OPTARG" ;;
        f) FADE="$OPTARG" ;;
        r) SEED="$OPTARG" ;;
        h) sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
        *) exit 1 ;;
    esac
done

awk -v dur="$DURATION" -v step="$STEP_MS" -v dl="$DL" -v ul="$UL" \
    -v vol="$VOLATILITY" -v fade="$FADE" -v seed="$SEED" '
# gauss returns a standard normal random value (Box-Muller).
function gauss() { return sqrt(-2 * log(1 - rand())) * cos(6.283185307 * rand()) }

# walk moves the capacity of one direction by one step.
function walk(d) {
    v[d] *= exp(vol * gauss())
    if (v[d] < lo[d]) v[d] = lo[d]
    if (v[d] > hi[d]) v[d] = hi[d]
    if (faded[d] == 0 && rand() < fade) faded[d] = int((1000 + 2000 * rand()) / step)
    if (faded[d] > 0) { faded[d]--; return lo[d] }
    return v[d]
}

BEGIN {
    srand(seed)
    split(dl, a, ","); lo["dl"] = a[1]; hi["dl"] = a[2]
    split(ul, a, ","); lo["ul"] = a[1]; hi["ul"] = a[2]
    for (d in lo) { v[d] = sqrt(lo[d] * hi[d]); faded[d] = 0 }
    print "t_ms,dl_kbps,ul_kbps"
    for (t = 0; t < dur * 1000; t += step) {
        printf "%d,%d,%d\n", t, walk("dl"), walk("ul")
    }
}'
