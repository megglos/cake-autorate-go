#!/usr/bin/env bash
# shellcheck disable=SC2034
# Upstream cake-autorate config for the 5G A/B comparison. Same rates,
# interfaces and reflectors as recordings/5g-owd.yaml. Other values are the
# upstream defaults (the same as the cake-autorate-go defaults).
dl_if=ifb4enp7s0
ul_if=enp7s0
adjust_dl_shaper_rate=1
adjust_ul_shaper_rate=1
min_dl_shaper_rate_kbps=30000
base_dl_shaper_rate_kbps=150000
max_dl_shaper_rate_kbps=400000
min_ul_shaper_rate_kbps=4000
base_ul_shaper_rate_kbps=18000
max_ul_shaper_rate_kbps=45000
connection_active_thr_kbps=2000
pinger_method=fping
reflectors=("1.1.1.1" "1.0.0.1" "8.8.8.8" "8.8.4.4" "9.9.9.9" "149.112.112.112" "208.67.222.222" "208.67.220.220")
no_pingers=6
debug=0
output_summary_stats=1 # shaper rates in the log, to count the rate changes
