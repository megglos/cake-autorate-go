#!/usr/bin/env bash
# shellcheck disable=SC2034
# Original cake-autorate for the Starlink comparison in the OpenWrt VM. Same
# rates, thresholds and reflectors as starlink-*.yaml. Change the rates
# after the calibration of day 1, as in the yaml files.
dl_if=ifb4eth1
ul_if=eth1
adjust_dl_shaper_rate=1
adjust_ul_shaper_rate=1
min_dl_shaper_rate_kbps=20000
base_dl_shaper_rate_kbps=100000
max_dl_shaper_rate_kbps=300000
min_ul_shaper_rate_kbps=5000
base_ul_shaper_rate_kbps=20000
max_ul_shaper_rate_kbps=40000
dl_owd_delta_delay_thr_ms=30.0
ul_owd_delta_delay_thr_ms=30.0
connection_active_thr_kbps=2000
pinger_method=fping-ts
reflectors=("100.64.0.1" "fra-de-ping.vultr.com" "nbg1-speed.hetzner.com" "fsn1-speed.hetzner.com" "94.140.14.14" "94.140.15.15" "ams-nl-ping.vultr.com" "par-fr-ping.vultr.com")
no_pingers=6
debug=0
output_summary_stats=1
