# Starlink test plan

> Goal: find the best settings of cake-autorate-go for a Starlink link, with the same method as the 5G day pass ([RESULTS.md](RESULTS.md#5g-day-pass-2026-10-21)). Starlink access is available from 2026-10-28.

The questions:

1. Does the delay floor of Starlink change every 15 s (at the seconds 12, 27, 42 and 57)? How large are the steps? Are they the same in the two directions?
2. Do ICMP timestamps go through the carrier NAT of Starlink? Which reflectors have the lowest jitter (`100.64.0.1`, Frankfurt, other)?
3. Which link mode is the best on Starlink (`latency`, `balanced`, `bandwidth`), and how do they compare with the original cake-autorate?
4. Does `starlink_reconfiguration` (branch `feat/starlink-reconfig`) prevent incorrect rate decreases at the floor steps?
5. Which thresholds, rates and `learn_cross_delay` setting does the calibration propose? Does it detect a Starlink link?
6. What does the controller do during a short outage (an obstruction or a satellite handover)?

## A. Before the access (until 2026-10-27)

- [ ] Merge PR #112: the calibration probe tests `100.64.0.1`.
- [ ] Rebase `feat/starlink-reconfig` on main. The branch is from 2026-10-11. Since then, main has link modes, `auto_max_rate`, stall recovery and the calibration. Run the simulator again: starlink-like, 8 seeds, with and without the option. No PR before the live data.
- [ ] Analysis script for the 15 s schedule: from a recording, the OWD (DL and UL) per second of the minute (p10, p50, p90), and the size of the step at each boundary. Test it with the `starlink-like` simulator trace.
- [ ] Session configs in `recordings/starlink/`:
  - `latency.yaml`, `balanced.yaml`, `bandwidth.yaml`: the same link and thresholds, only `mode` is different.
  - `reconfig.yaml`: `balanced` plus `starlink_reconfiguration` (binary from the rebased branch).
  - `upstream-starlink.sh`: the original cake-autorate with `fping` and ICMP timestamps, with its Starlink settings.
- [ ] Host setup script for Starlink on `enp8s0`, as for 5G: table 55 with the default route through the Starlink gateway, NAT on `enp8s0`, ufw route rules for `cavtap0`. Starlink gives the address with DHCP. Thus, read the gateway from the lease.
- Setup (user, 2026-10-22): the Starlink router runs in bypass mode, and the dish has a clear view of the sky.

## B. Day 1: first look (approximately 2 hours)

1. Connect the Starlink router (bypass mode) to `enp8s0`. Run the host setup. Check from cav1: the public address, the RTT to `100.64.0.1` and to `fra-de-ping.vultr.com`.
2. Idle recording, 30 minutes, without load, `ping_method: icmp-timestamp`, all candidate reflectors. Use the analysis script: answers question 1 (the floor steps without a queue).
3. Live calibration from LuCI (normal mode). Record the proposal: ping method, reflectors, rates, thresholds, `learn_cross_delay`. Answers question 2 and a part of question 5.
4. One unshaped block per direction (dl, ul, both, 120 s): the capacity and the bufferbloat without a shaper.

Stop and look at the data before the A/B sessions. If the floor steps are not at 12/27/42/57: make the period and the offset config keys of `starlink_reconfiguration` before the sessions.

## C. A/B sessions (2 or 3 days, at different times of the day)

The Starlink plan has no data limit (user, 2026-10-22). Thus, the sessions need no `-L`, and the session can have more rounds than on 5G.

The method of the 5G day pass: `openwrt-vm-session.sh` on cav1, blocks of 120 s, the policies alternate in each round, so they get the same link conditions. Use paired differences per round for the comparison.

| Session | Policies | Load | Rounds | When |
|---|---|---|---|---|
| sl1 | latency, balanced, bandwidth, original | dl, ul, both | 3 | day 2, morning |
| sl2 | latency, balanced, bandwidth, original | dl, both | 5 | day 2, evening (busy cell) |
| sl3 | balanced, reconfig, bandwidth, bandwidth + reconfig | dl, both | 5 | day 3 |
| sl4 | the calibrated config, the best mode, original | dl, ul, both | 3 | day 3, night |
| sl5 (optional) | best mode with `learn_cross_delay` on and off | both, with `-W 30` | 4 | after sl1 to sl4 |

For each block, record: p50, p95 and p99 of the excess delay, DL and UL throughput, the number of rate decreases at the 15 s boundaries, and the outages (STALL state).

Question 6: when a block has an outage, examine the recording. Does the rate come back after the outage, and how fast? Compare with the 5G stall recovery results (PR #106).

## D. Analysis and results

- [ ] RESULTS.md: section "Starlink" with the tables of B and C, the same format as the 5G day pass.
- [ ] Simulator: import the traces (`trace-from-session.sh -r`) as the scenario `starlink-recorded`. Calibrate `starlink-like` against the live data (as for 5G, step 2c of PLAN.md).
- [ ] Decisions (the user decides):
  - `starlink_reconfiguration`: PR or reject.
  - The default mode on Starlink. If it is not `balanced`, the calibration proposes a mode for a Starlink link.
  - The calibration detects a Starlink link (the floor steps), and proposes the Starlink settings.
  - The default reflectors: add `100.64.0.1` or not.
- [ ] Docs: the Starlink part of comparison.md, a Starlink example in configuration.md, README summary. Comment on the related issues.
- [ ] Recordings to the branch `recordings` (raw sessions), with a README note for each session.

## Risks

- **Outages.** The dish has a clear view, but satellite handovers and weather can still cause short outages. They make blocks not comparable. Mark the blocks with an outage, and leave them out of the paired comparison.
- **ICMP timestamps blocked.** Then use `icmp` with `latency` (`balanced` is equal to `latency` with `icmp`). `starlink_reconfiguration` needs one-way delays, thus it cannot be used then.
