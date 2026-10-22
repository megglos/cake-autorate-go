# Starlink sessions

Prepared before the access (2026-10-22). The sessions follow PLAN-STARLINK.md
on the `plans` branch. Rename this directory to `<date>-starlink` at the
first session.

## Configs

All configs use `ping_method: icmp-timestamp` and the same link, rates,
thresholds and reflectors. Only the options of the variants are different.

| File | Variant |
|---|---|
| `starlink-latency.yaml` | `mode: latency` |
| `starlink-balanced.yaml` | `mode: balanced` (the default mode) |
| `starlink-bandwidth.yaml` | `mode: bandwidth` |
| `starlink-reconfig.yaml` | `balanced` + `starlink_reconfiguration` |
| `starlink-bandwidth-reconfig.yaml` | `bandwidth` + `starlink_reconfiguration` |
| `starlink-balanced-lcd.yaml` | `balanced` + `learn_cross_delay` |
| `upstream-starlink-fts.sh` | the original cake-autorate (`fping-ts`), same rates and reflectors |

`starlink_reconfiguration` is only on the branch
`feat/starlink-reconfig-v2`. A binary of `main` does not accept the
`reconfig` configs. Use the variant binary with `-A NAME=CONFIG,BINARY`.

Before the A/B sessions, change in all files:

- the rates and the thresholds: from the calibration of day 1. The
  values now are typical Starlink values (download 20 / 100 / 300 Mbit/s,
  upload 5 / 20 / 40 Mbit/s, min / base / max; thresholds 30 ms).
- the reflectors: from the proposal of the calibration. The list now has
  `100.64.0.1` (the Starlink gateway at the point of presence) and
  reflectors near Frankfurt first.

## Example

    scripts/openwrt-vm-session.sh start sl1 -c starlink-balanced.yaml \
        -A latency=starlink-latency.yaml -A bandwidth=starlink-bandwidth.yaml \
        -U fts=upstream-starlink-fts.sh -m "dl ul both" -b 120 -r 3 -s

## Host setup

`host-setup.sh up` sends the traffic of the cav1 VM through the Starlink
router on enp8s0 (run as root, after each reboot of the host and after
the start of the VM taps). `host-setup.sh down` removes it.

## Delay floor

To examine the 15 s floor changes in a recording, use
`scripts/starlink-floor.py` (PR #114 on `main`).
