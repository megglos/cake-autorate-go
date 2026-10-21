#!/usr/bin/env bash
# shellcheck disable=SC2034
# Original cake-autorate for the 5G comparison in the OpenWrt VM. Same
# rates, thresholds and reflectors as 5g.yaml (calibration of 2026-10-18).
dl_if=ifb4eth1
ul_if=eth1
adjust_dl_shaper_rate=1
adjust_ul_shaper_rate=1
min_dl_shaper_rate_kbps=34000
base_dl_shaper_rate_kbps=130000
max_dl_shaper_rate_kbps=245000
min_ul_shaper_rate_kbps=19000
base_ul_shaper_rate_kbps=94500
max_ul_shaper_rate_kbps=110000
dl_owd_delta_delay_thr_ms=36.0
ul_owd_delta_delay_thr_ms=36.0
connection_active_thr_kbps=2000
pinger_method=fping-ts
reflectors=("lon-gb-ping.vultr.com" "94.140.15.15" "ams-nl-ping.vultr.com" "94.140.14.14" "waw-pl-ping.vultr.com" "sto-se-ping.vultr.com" "fra-de-ping.vultr.com" "ping.online.net")
no_pingers=6
debug=0
output_summary_stats=1

log_file_path_override=/root/cav/out/dp4/12-both-upstream-fts.upstream
