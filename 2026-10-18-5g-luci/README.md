# 5G on the OpenWrt VM with LuCI, 2026-10-18

Link: 5G modem (Telekom), WAN of the OpenWrt VM. cake-autorate-go (main
36bc1cd) ran as the service of the VM, with the calibration of that day:
dl 34000/130000/245000 kbit/s, ul 19000/94500/110000 kbit/s, delay
thresholds 36 ms, `icmp-timestamp`.

`5g-autorate-20261018-1351.jsonl.gz`: 17 minutes of `--record`, with the
LuCI bufferbloat test and speed tests (Waveform) from a client. There is
no `blocks.tsv`: the load came from the tests, not from
`record-session.sh`.
