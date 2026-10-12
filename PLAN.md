# cake-autorate-go: improvement plan

> Working plan. It is on the branch `plans` (worktree ../cake-autorate-go-plans, symlinked into the main worktree). Commit and push each change there. Never merge this branch into main.
> Goal: the best possible CAKE autorate for **5G** and **Starlink** links.
> Language: ASD-STE100.
> Last review: 2026-10-11.

## Decision

Keep the current code. Do not write it again from the start.

- Keep the shaper (netlink with a `tc` fallback), the monitor, the config, the logger and the multi-link support.
- Change the control algorithm. It decides the performance on 5G and Starlink links.
- The control algorithm is now in `Core` (`core.go`). `Core` is deterministic and we can test it.

## Status

| Item | Status |
|---|---|
| PR #4: deterministic `Core` | Merged |
| PR #6: CI runs on all pull requests | Merged |
| PR #5: link simulator, scorecard, known-issue tests, baseline check | Merged |
| Branches `refactor/pure-core`, `sim/link-simulator`, `ci/run-on-all-prs` | Deleted |
| PR #7: `--record`, `RecordingCore`, recording procedure, session tools | Merged |
| PR #8: default delay thresholds for links | Merged |
| Branch `emu/5g-on-fiber`: 5G emulation, trace generator, session summary | Merged (PR #9) |
| PR #10: ICMP timestamps (`ping_method: icmp-timestamp`, F4) | Merged |
| PR #11: decrease from the achieved rate (F2), `refractory_on_decrease` (F1), `record-session.sh -A` | Merged |
| Branch `feat/ul-dl-coupling`: `rebaseline_at_min_rate_ms` | Rejected (no effect on 5G, A/B session 2). Deleted |
| Branch `fix/increase-to-threshold` (worktree ../cake-autorate-go-f7, bb9b26f): F8 | PR #18 **draft, recommend close**: 8 seeds show 5g-variable p95 53 → 68 ms; `learn_cross_delay` alone gives the same UL gain |
| Branch `feat/learn-cross-delay-v2` (worktree ../cake-autorate-go-cross, 6e403f2, on main, no F8): simulator coupling model + 5g-coupled, `learn_cross_delay` with 2 s low-load gate | PR #20 (2 Copilot cycles done, ready for user review). PR #19 (old, on F8) replaced; user closes it. Backup of old branch: `backup/cross-on-f8` |
| PR #20 (`learn_cross_delay`), PR #21 (baseline with autorate-owd and 4 seeds) | Merged. PR #18 (F8) and #19 closed |
| Branch `sim/ul-grant-delay` (worktree ../cake-autorate-go-grant, 52ea8db): UL grant delay model in 5g-coupled, test for the UL threshold floor, drop test that learns first | PR #22 (Copilot: 0 findings, ready for user review) |
| PR #12 unknown config keys, PR #13 netlink retry, PR #14 development docs, PR #15 simulator seeds, PR #16 ping loss and outages, PR #17 recorded 5G traces | Merged |
| Worktree ../cake-autorate-go-tune (not committed, detached) | Obsolete (session 3 done, results in #20/#22). Remove |
| Branch `feat/upstream-compare` (worktree ../cake-autorate-go-up, 4f3ed41): `record-session.sh -U` | Rebased on main 2026-10-11 19:10 (with `-W`), not pushed. For step 8 |
| PR #22 UL grant delay model, #23 `-W` warm-up, #24 remove delta EWMA (F3 rejected), #25 sim calibration with 5G | Merged |
| PR #28: README "5G Links" (5G profile: learn_cross_delay, UL threshold 20) | Merged |
| PR #26: `calibrate` command (offline, from a session) | Open: Copilot rounds done (3 + 1 user request), ready for user review/merge |
| PR #27: latency target per direction | Open: Copilot rounds done (3 + 1 user request), ready for user review/merge. Changed after session 8: ref gate 20 % load, stale windows, RTT mode uses RTT increase |
| PR #29: CI emulation (`scripts/ci-emulation.sh`, CI job `emulation`) | Open: first CI run passed (autorate p95 42 ms, DL 87 %, UL 78 %; unshaped p95 1024 ms). Copilot round 2 pending |
| Old worktrees of merged branches (-bl, -cal, -cross, -docs, -f3, -f7, -grant, -keys, -loss, -nl, -seeds, -trace, -warm, -tune) | Removed 2026-10-12 (except -tune: 9 uncommitted files, user decides) |

## Findings

The code review on 2026-10-10 found F1 to F7. The comparison with upstream on 2026-10-11 found F8. The IDs are also in the tests and commits.

Status: F1 option merged (#11), F2 merged (#11), F3 rejected (PR #24 merged: window average worse in the simulator, 8 seeds; unused EWMA removed), F4 merged (#10), F5 open (Starlink), F6 fixed (#4), F7 closed, F8 rejected (PR #18 draft: costs latency; `learn_cross_delay` solves the coupling better).

| ID | Issue | Location | Effect | Simulator result |
|---|---|---|---|---|
| F1 | No refractory period for decreases. Each ping cuts the rate again while the detection window is full (approximately 13 pings/s with 4 reflectors at 300 ms). | `core.go:297` | Too many cuts after one spike | A 300 ms spike cuts DL to 42 % of the capacity |
| F2 | The cut uses the shaper rate, not `min(achieved, shaper)`. | `core.go:335` | Slow convergence when the capacity decreases quickly (frequent on 5G) | After 200 to 40 Mbit/s, the rate goes to the minimum (10 Mbit/s) |
| F3 | `deltaEWMA` is calculated but not used. The `avg_owd_delta_*` thresholds apply to the delta of one sample. | `core.go:224`, `core.go:323`, `core.go:353` | Noise of one sample sets the size of the adjustment | No isolated test yet |
| F4 | RTT/2 is the OWD for the two directions. | `core.go:216` | UL bloat (the usual bottleneck on 5G and Starlink) also cuts DL | With saturated UL, DL uses 51 % of its capacity |
| F5 | Starlink changes the RTT floor every 15 s (at seconds 12, 27, 42 and 57). The baseline increases slowly (`alpha_baseline_increase`). Thus, the new floor looks like bloat. | `core.go:249` | Incorrect cuts every 15 s | A +40 ms floor step decreases the mean DL rate by 34 % |
| F6 | `time.Now()` in the control logic. | — | Not deterministic | **Fixed in PR #4** |
| F7 | (Minor) The baseline update uses the current sample before the delta calculation. | `core.go:212` | The delta is a little too small | — (closed: upstream does the same) |
| F8 | (Found 2026-10-11 in the comparison with upstream.) At high load, the rate increase stops completely when the delta is above `avg_owd_delta_max_adjust_up_thr_ms` (10 ms). Upstream increases with a smaller step up to the delay threshold (30 ms). | `core.go` `adjustRateHighLoad` | On 5G, the UL delay from DL activity (p50 21 ms) stops all UL increases: UL stays near the minimum | 5g-coupled UL 25 → 72 % (with OWD). With RTT/2 the upstream factor is worse (bidir DL 51 → 16 %), thus the fix is for OWD only |

Open item: compare F1 and F2 with the upstream cake-autorate v3 source (lynxthecat). Do this before step 3.

## Emulation on fiber (2026-10-10)

Session `recordings/20261010-2023-emu`: trace `trace-5g.csv` (seed 1, dl 30-400 Mbit/s median 238, ul 5-80 median 21), queue 1000 packets, extra RTT 20 ms, 150 s blocks, internet servers.

| Block | Excess RTT p50/p95/p99 | Throughput (% of capacity) |
|---|---|---|
| dl unshaped | 137 / 442 / 570 ms | 99 % |
| dl fixed (85 % of median) | 0.7 / 83 / 650 ms | 63 % |
| dl autorate | 31 / 78 / 108 ms | 92 % |
| ul unshaped | 272 / 584 / 806 ms | 101 % |
| ul fixed | 0.5 / 63 / 117 ms | 46 % |
| ul autorate | 30 / 79 / 568 ms | 89 % |

Analysis:
- Autorate gives the best trade-off, but it does not meet the proposed criteria (p95 ≤ 30 ms).
- p50 is approximately 30 ms in the two directions. This is the operating point of the default thresholds (OWD delta 30 ms, with RTT/2 as the OWD). It is a tuning item (step 6), and F4 changes it.
- UL p99 568 ms: two deep fades (49→5 Mbit/s at t=81 s, 72→5 Mbit/s at t=102 s). The controller cuts to the minimum in 0.3 to 0.5 s, but the queue already contains more than 1 s of data at 5 Mbit/s. Faster detection and F2 can help (step 5).
- Recovery after a fade is slow: from 4 to 25 Mbit/s in approximately 2 s, while the capacity is back at 72 Mbit/s. This costs throughput.

## First 5G session (2026-10-10, 21:38)

Session `recordings/20261010-2138-enp7s0`: Vodafone 5G, modem in passthrough on enp7s0, Berlin, evening. Full session (2 rounds x dl/ul/both x 3 policies), `-F 85`. Config `recordings/5g.yaml` (dl 30/150/400, ul 4/18/45 Mbit/s). Code: main at 5cce1b5 (no fixes).

- Capacity changes very much: saturation check dl 72 / 490 / 69 Mbit/s (4/8/16 workers), dl 454 to 221 Mbit/s in 10 s. UL is stable (34-37 Mbit/s).
- Thus, "% ref" (unshaped block of the same round) has a large error (block 8: 172 %). The fixed dl rate (417 Mbit/s) was too high and acts as unshaped.

| Mode | unshaped p50/p95 | autorate p50/p95 | autorate throughput (% unshaped) | fixed |
|---|---|---|---|---|
| dl | 620-990 / 1140-1250 ms | 28-33 / 77-79 ms | 40-48 % | (too high, bloat) |
| ul | 410-420 / 660-690 ms | 19-20 / 83-85 ms | 68 % | 14 / 54 ms, 75-78 % |
| both | 720-900 / 1260-1360 ms | 21-22 / 92-93 ms | dl 34-35 %, ul 87-94 % | (dl too high) |

Analysis:
- Autorate decreases the bufferbloat by approximately 15x. But it loses too much throughput. In UL, the fixed shaper is better than autorate for the two values.
- Sawtooth: the dl rate increases from approximately 105 to 180-220 Mbit/s in 5 to 10 s, then the rate falls back to approximately 105. Approximately one cut episode each 4 s.
- **F1 is confirmed on the real link.** Each cut episode has 6 to 10 cuts in 300 to 600 ms (one for each ping while the detection window has 3 or more delayed pings). Mean depth: 22-27 % (dl, ul modes), 50-54 % (both mode). With one cut for each refractory period, the depth is approximately 5-25 %.
- **F4 is confirmed.** In "both" mode, the two directions are cut together and dl gets only 34 % of the reference. In ul mode, also the idle dl rate is cut (8 % for each episode).
- The excess RTT stays at approximately 23-27 ms also when the shaper is far below the achieved capacity. Possible causes: 5G scheduling delay under load, or the UL grant delay for the ACKs and the pings. OWD measurement (step 4) can show which direction.
- Idle RTT is different for each reflector (11 ms to 34 ms). Idle jitter is approximately 2-5 ms.

## ICMP timestamps on 5G (2026-10-10)

Test with a raw socket (type 13/14), 5 requests for each host, Vodafone 5G:
- Timestamps go through the carrier network. The carrier gateway (CGNAT address) replies.
- No reply: Cloudflare, Google, Quad9, OpenDNS, Level3 (4.2.2.x), Yandex, Cloudflare CDN, serverius.
- Reply: AdGuard DNS 94.140.14.14, Vultr (fra/ams/par), Hetzner (fsn1/nbg1/hel1).
- Thus, step 4 is possible on this link. But the reflector list must change: the usual public DNS reflectors do not reply. A good list must have 6 or more hosts that reply and that permit a constant low rate of probes.
- The out/back values contain the clock offset. The controller must use only the change of each OWD against its baseline, as for the RTT.

## Second 5G session: weaker signal (2026-10-10, 22:41)

Session `recordings/20261010-2241-enp7s0`: modem inside, next to the window. `-r 1`. Saturation check dl 72/237/70, ul 15/15/17 Mbit/s (16 workers selected; warning: UL possibly not full).

| Mode | unshaped p50/p95/p99 | autorate p50/p95/p99 | autorate throughput |
|---|---|---|---|
| dl | 108 / 874 / 1142 ms | 29 / 86 / 124 ms | 45 % |
| ul | 25 / 65 / 501 ms | 24 / 55 / 194 ms | 94 % |
| both | 816 / 1343 / 1443 ms | 32 / 80 / 108 ms | dl 53 %, ul 110 % |

- Same pattern as session 1: autorate removes the dl bloat, but gets only approximately half of the dl throughput.
- At approximately 16 Mbit/s, the UL bloat is small without a shaper (p50 25 ms). Only the tail is large.

## F1/F2 experiment (branch fix/bufferbloat-refractory, worktree ../cake-autorate-go-f1, WIP commit, not pushed)

Simulator, autorate (main → F1 + F2 with the achieved rate only at high load):

| Scenario | p50 ms | p95 ms | p99 ms | dl % | ul % |
|---|---|---|---|---|---|
| 5g-variable | 24 → 38 | 82 → 115 | 538 → 592 | 82 → 95 | 56 → 63 |
| starlink-like | 15 → 23 | 61 → 69 | 83 → 140 | 73 → 90 | 70 → 69 |
| capacity-drop | 37 → 40 | 65 → 65 | 150 → 336 | 97 → 98 | - |
| bidir-saturated | 40 → 53 | 70 → 78 | 72 → 82 | 51 → 56 | 97 → 100 |

- F1 alone: the spike test passes (98 % instead of 42 %), but bloat increases and 5g-variable UL falls to 19 %. The many cuts of F1 compensated for the missing F2.
- F2 without a gate: bidir-saturated DL falls to 15 %. Cause: F4. The UL bloat also gives "bufferbloat" for DL. DL does not fill its shaper, thus each decrease from the achieved DL rate goes lower (ratchet).
- F2 with a gate (achieved rate only at high load): no ratchet, but after a capacity drop the load is low (shaper far above the capacity). Thus, the gate also blocks F2 where it is necessary (capacity-drop p99 336 ms).
- Conclusion: F1 and F2 depend on F4. With one delay signal for the two directions, Core cannot know which direction has the queue. Proposal: do F4 (OWD with ICMP timestamps, step 4) before F1/F2. ICMP timestamps operate on the 5G link.

## F4 fix: one-way delays with ICMP timestamps (branch feat/icmp-timestamps, b7b4741, not pushed)

- `ping_method: icmp-timestamp`. Per-reflector, per-direction EWMA baseline (it removes the clock offset). The default reflector list changes to hosts that reply (AdGuard, Vultr, Hetzner).
- Simulator (autorate → autorate-owd): bidir-saturated p95 70 → 43 ms, dl 51 → 89 %. 5g-variable p95 82 → 53 ms, ul 56 → 84 %. starlink-like p95 61 → 35 ms.

## F1/F2 with OWD (branch fix/bufferbloat-refractory, b7b4741 + 79935db, not pushed)

- F2 (cut from min(achieved, shaper)) only with OWD. It is better in all simulator scenarios: capacity-drop p99 45 → 35 ms, 5g-variable p95 53 → 47 ms, dl 81 → 85 %, ul 84 → 87 %.
- F1 is the option `refractory_on_decrease` (default false). In the simulator it adds 5-10 % throughput but approximately two times the p95 bloat. The 5G A/B session decides.
- `record-session.sh -A NAME=CONFIG[,BINARY]` adds variant policies (A/B in one session).

## Third 5G session: OWD (2026-10-10, 23:26)

Session `recordings/20261010-2326-enp7s0`: binary from feat/icmp-timestamps, `recordings/5g-owd.yaml`, `-F 85`, 2 rounds.

| Mode | session 1 (RTT/2) autorate | OWD autorate |
|---|---|---|
| both | dl 34-35 %, ul 87-94 %, p95 92 ms | **dl 61-79 %**, ul 32-49 %, p95 77-78 ms |
| dl | 40-48 %, p95 77-79 ms | 37-45 %, p95 128-138 ms |
| ul | 68 %, p95 83-85 ms | 54-68 %, p95 41-44 ms |

- The F4 fix operates: in "both" mode, the UL delay does not cut DL.
- **New issue: download activity delays the upload direction (5G scheduler).** In dl blocks, the UL OWD increase is +29 ms p50, +120 ms p95. The DL OWD increase is only +13 / +41 ms. UL traffic is only ACKs (2-4 Mbit/s). The UL shaper stays at its minimum (4 Mbit/s) all the time.
- The same UL delay occurs in the unshaped dl blocks. The 5G radio causes it, not a queue in our shaper.
- Binned by DL rate (dl blocks of this session): the UL delay is +25 to +40 ms p50 for all DL rates from 25 to 300 Mbit/s. It is +11 ms only when DL is idle. Thus, **the delay depends on DL activity, not on the DL rate**. A DL decrease does not remove it.
- With RTT/2, DL got half of this UL delay. This is why dl-only RTT p95 was lower with RTT/2 (77 ms) than with OWD (128 ms), at the same throughput.
- AdGuard (not a load server) shows the same per-direction delays as the Vultr/Hetzner reflectors (load servers). No bias from the load servers.
- Saturation checks are noisy (4 workers: 68 or 360 Mbit/s in different checks). Use the fixed blocks for comparison, not "% ref".
- **Consequence for the success criteria:** with DL active, the 5G radio adds approximately 25-40 ms RTT (p50) that no shaper can remove. Thus, p95 ≤ 30 ms excess RTT is not possible on this link under DL load. Measure the criteria against this floor.

## Coupling fix: rebaseline_at_min_rate_ms (branch feat/ul-dl-coupling, worktree ../cake-autorate-go-coupling, bdc478e on 79935db, not pushed)

- If a direction stays at its minimum rate with bufferbloat for the hold time, Core adds the delay to the OWD baseline of that direction. Off by default.
- Simulator: new coupling model (`ulCouplingMs` = 40 ms, full value at 20 % DL use, as in the 5G data) and scenarios 5g-coupled, 5g-coupled-dl. 5g-coupled: ul 7 → 85 %, dl 99 → 93 %, p95 69 → 77 ms. Other scenarios do not change.
- Hold time is necessary: without it, a UL capacity a little above the minimum gives p95 57 → 514 ms (the queue at the minimum rate empties slowly). With 2 s: no change.
- Rejected alternative: give the UL delay to DL when UL is at the minimum. Simulator: p95 88 → 20 ms, but dl 98 → 34 % and ul stays at 7 %. The 5G data shows that a DL decrease does not remove the delay. Thus, do not do this.
- autorate-owd 5g-variable p99 414 ms (p95 47 ms): one event. UL load starts at t=30 s during a UL fade to 5 Mbit/s, with the shaper at the base rate (20 Mbit/s). The 800 ms buffer fills before the first cut. Low priority.

## A/B session 1 (2026-10-11, 05:04)

Session `recordings/20261011-0504-enp7s0`: policies autorate (OWD, ./cake-autorate-go), autorate-f2 (F2), autorate-f1f2 (F2 + `refractory_on_decrease`). 2 rounds. Throughput in Mbit/s (the % ref values are noisy).

| Mode | owd | f2 | f1f2 |
|---|---|---|---|
| dl: dl Mbit/s, p95 | 100 / 106, 124 / 132 ms | 104 / 95, 143 / 117 ms | **124 / 117**, 146 / 130 ms |
| ul: ul Mbit/s, p95 | 14 / 18, 39 / 40 ms | 16 / 15, 38 / 37 ms | **23 / 20**, 43 / 44 ms |
| both: dl Mbit/s, p95 | 87 / 84, 63 / 67 ms | 93 / 78, 60 / 65 ms | **113 / 125**, 74 / 85 ms |
| both: ul Mbit/s | 6 / 5 | 9 / 5 | 5 / 5 |

- F2 alone: no difference from owd that is larger than the noise.
- F1 + F2: +15 to +45 % throughput in all modes. p95 +0 to +18 ms. The same trade-off as in the simulator. Decision: use f1f2 as the base for the next tests (goal: bufferbloat low AND throughput high). The default stays off until more data.
- In "both" mode, UL stays at approximately 5 Mbit/s with all variants (the UL shaper is at the minimum, median 5.1 Mbit/s). This is the coupling issue. The next A/B tests the coupling fix.
- ul mode with autorate gets only 41-66 % of unshaped and less than fixed (24 Mbit/s, p95 55 ms). f1f2 is the best autorate variant here.

## A/B session 2 (prepared)

f1f2 vs f1f2 + `rebaseline_at_min_rate_ms: 2000`. Binary `../cake-autorate-go-coupling/cake-autorate-go` for both, configs `recordings/5g-owd-f1.yaml` and `recordings/5g-owd-f1-rb.yaml`. 24 blocks, approximately 72 minutes.

## Simulator sweep (2026-10-11, base: OWD + F1 + F2 + rebaseline 2 s)

One parameter at a time, 7 scenarios. Mean p95 / mean utilization: base 52.9 ms / 94.0 %.
- The delay threshold is the largest lever. It sets the operating point of the queue. thr 15 ms: 33.5 / 93.7. thr 20: 37.3 / 94.1. thr 40: 62.2 / 91.3.
- Max decrease 0.6 (default 0.75): 45.5 / 94.3. Refractory 150 ms: 47.7 / 94.1. Other parameters (detection 2 or 4 of 6, high_load_thr, max increase, ping interval): small effect or worse.
- Combinations: thr 15 + down 0.6: 28.4 / 92.0. thr 20 + down 0.6: 34.1 / 93.9. With 8 ms jitter: 25.6 / 90.0 and 31.4 / 91.9.
- Without F1, thr 15: 26.5 / 85.2. F1 keeps the throughput at low thresholds.
- Risk: the simulator jitter model is simple. On the real link, a low threshold can give false bufferbloat. Test on 5G.
- Configs: `recordings/5g-owd-t20.yaml`, `recordings/5g-owd-t15.yaml` (f1f2 + rb + thr + `shaper_rate_max_adjust_down_bufferbloat: 0.6`).

## A/B session 2: rebaseline (2026-10-11, 06:56)

Session `recordings/20261011-0656-enp7s0`: f1f2 vs f1f2 + `rebaseline_at_min_rate_ms: 2000`. Result (2 rounds): no difference larger than the noise. "both" UL: 6 / 6 (f1f2) vs 6 / 7 Mbit/s (rb). Rebaseline is not useful on this link. Do not open a PR for it.
- Cause: the UL delay from DL activity is noisy. Per ping, under load: p10 10, p50 21, p90 38, p95 46 ms. 21 % of the pings are above 30 ms. Thus, the UL shaper goes up and down between 4 and 7 Mbit/s. It does not stay at the minimum rate, and the rebaseline does not occur.
- Simulator: with this noise (`ulCouplingJitterMs`), the simulator shows the same result (rebaseline has no effect, UL 51 %). The coupling model is now 10 ms + exponential noise with a mean of 12 ms.

## New design: learn_cross_delay (worktree ../cake-autorate-go-tune, not committed)

- Core learns the delay of a direction when the other direction is active and this direction has a low load (achieved < 50 % of the shaper rate). Then the shaper of this direction has no queue. EWMA alpha 0.05 for each ping. The learned delay is not more than the delay threshold. Core removes it only while the other direction is active.
- Simulator (mean of 7 scenarios, p95 / utilization): thr 30: 48.2 / 91.2. thr 15 + down 0.6: 24.5 / 85.2 (5g-coupled UL 9 %). thr 15 + down 0.6 + cross: 28.7 / 91.8 (5g-coupled UL 79 %).
- Cost: starlink-like p95 19 → 32 ms. Cause: Core learns the UL delay from jitter and floor steps before the UL load starts. Then the learned value stays while UL is loaded. Examine this before Starlink.
- F8 (new finding, upstream parity): upstream increases the rate up to the delay threshold (factor (thr − avg) / (thr − up_thr)). Ours stops all increases above `avg_owd_delta_max_adjust_up_thr_ms` (10 ms). On 5G, the UL delay from DL activity (p50 21 ms) then stops all UL increases. Simulator 5g-coupled UL: 50 → 93 %. Test key `x_increase_to_threshold`.
- F3 (window average for the adjustment size): simulator mean p95 48.6 → 47.2 ms, utilization 91.9 → 91.5 %. With thr 15: worse. On hold.
- F7: upstream also updates the baseline before the delta. Not a parity issue. Closed.
- Session 3: f1f2 vs f8 vs x vs t15x vs t15xf8, `-F 0`. Binary `../cake-autorate-go-tune/cake-autorate-go`. 5 policies x 3 modes x 2 rounds = 30 blocks, approximately 90 min.

## Session 3 candidates on 8 seeds (simulator, 2026-10-11)

Mean p95 / mean utilization of 8 scenarios × 8 seeds: f1 48.1 / 90.0, f8 52.2 / 94.8, x 53.4 / 94.8, t15x 30.5 / 91.4, t15xf8 31.3 / 91.9, t15f8 28.8 / 85.9 (5g-coupled UL 8 %), t20xf8 38.0 / 93.8.
- The ranking from one seed stays the same with 8 seeds.
- With thr 15, F8 alone does not help the coupled UL (8 %): the coupling delay is above 15 ms in half of the pings. Thus, a low threshold needs the cross delay.
- Cross delay for DL: no difference in the simulator, but a risk on the real link (after a capacity decrease, DL can learn its own queue). Use the cross delay for UL only.

## 5G finding: upload scheduling delay (session 3, block 11)

- t15x in ul mode: UL 6 Mbit/s (16 %), p95 28 ms. The UL shaper stayed near the minimum.
- Cause: with UL activity, the UL OWD is +14 ms p50, +24 ms p95, also at 6 Mbit/s (1/6 of the capacity). This is the 5G grant scheduling, not a queue. A UL threshold of 15 ms is too low on 5G.
- The simulator has no model for it. Thus, in the simulator a higher UL threshold only adds bufferbloat (d15u30xf8: 40.1 / 92.5 vs t15xf8 31.3 / 91.9). Add a model: UL delay with UL activity, 10 ms + noise (calibrate from the ul blocks).
- Simulator model added in the tune worktree (`ulGrantMs: 8`, `ulGrantJitterMs: 6` in 5g-coupled). Then the simulator shows the same result as the link: t15xf8 5g-coupled UL 11 % (link: 16 %), d15u30xf8 UL 93 %. Mean of 8 scenarios × 8 seeds (p95 / util): f1 49.7 / 87.8, t15xf8 32.0 / 86.2, d15u25xf8 37.8 / 92.7, d15u30xf8 40.8 / 92.6, d20u30xf8 44.5 / 93.7. Port the model to the F8 branch with the coupling model.
- Calibration mode (step 6b): measure this floor (short UL load at a low rate, UL OWD).
- Candidate for the next session: `recordings/5g-owd-d15u30xf8.yaml` (DL 15 ms, UL 30 ms, down 0.6, cross delay, F8).

## A/B session 3 (2026-10-11, 08:08)

Session `recordings/20261011-0808-enp7s0`. Binary: tune worktree (cross delay on both directions). All with F1 + F2. p95 ms and Mbit/s, 2 rounds:

| Mode | f1 (base) | f8 | x | t15x | t15xf8 |
|---|---|---|---|---|---|
| dl: p95, dl | 128/122, 136/122 | 153/122, 155/165 | 179/135, 147/167 | 298/118, 170/87 | 163/123, 177/131 |
| ul: p95, ul | 48/48, 21/23 | 69/67, 26/27 | 49/51, 22/22 | 28/29, 6/7 | 31/32, 20/13 |
| both: p95, dl, ul | 84/87, 130/211, 6/9 | 127/131, 165/163, 8/8 | 136/129, 195/194, 7/8 | 90/75, 165/144, 6/5 | 100/80, 186/172, 8/7 |

- F8 alone and x alone: more throughput (dl +20-40 %, ul +20 %), but p95 +20 to +45 ms. Not better on the trade-off.
- Threshold 15 without F8: UL collapses (6 Mbit/s). With F8 (t15xf8): UL 13-20, ul p95 31 ms (best), dl and both: more throughput than f1 at similar p95.
- t15xf8 is the best trade-off of this session. UL is still too low: the UL threshold of 15 ms is below the 5G grant delay (+14 ms p50 with UL activity).
- In "both" mode, unshaped UL is only 11-12 Mbit/s (DL load takes the radio). Thus, autorate UL 6-9 Mbit/s is 60-75 % of the reference, not 20 %.

## A/B session 4 (2026-10-11 10:02, `recordings/20261011-1002-enp7s0`, 30 blocks, 2 rounds)

Mbit/s (% unshaped), one-way delay delta p95 (pooled all reflectors, `scratchpad/owd-pool.py`). Values round 1 / round 2.

| Variant | dl: DL | dl: DL p95 | ul: UL | ul: UL p95 | both: DL | both: UL | both: DL p95 / UL p95 |
|---|---|---|---|---|---|---|---|
| f1 (main, 30/30) | 133/116 (72/48 %) | 49/52 | 20/21 (54/57 %) | 48/42 | 152/178 | 6/8 | 60/51, 48/57 |
| t15 (15/15) | 106/103 (57/42 %) | 31/34 | 15/11 (43/30 %) | 29/26 | 96/129 | 8/5 | 47/33, 54/45 |
| d15u25 | 105/101 (57/41 %) | 30/36 | 25/24 (68/66 %) | 55/52 | 100/101 | 13/10 | 63/29, 82/72 |
| d15u30 | 93/107 (51/44 %) | 67/31 | 27/25 (75/70 %) | 58/59 | 113/106 | 20/11 | 34/31, 78/89 |

Result:
- No variant is best on all criteria. Each is a trade-off between delay and throughput.
- DL threshold 15 ms: DL p95 about 31–36 ms instead of about 50 ms, but DL throughput decreases (dl mode 41–57 % instead of 48–72 %; both mode 96–129 instead of 152–178 Mbit/s).
- UL threshold 25/30 ms with F8 + cross delay: UL plus 20–30 % in ul mode and 1.5–3× in both mode, for about +10 ms UL p95 in ul mode. UL p95 in both mode is 72–89 ms (higher than f1, with more UL throughput).
- UL threshold 15 ms: UL collapses (5G grant delay, as in session 3).
- In dl mode (no UL load), UL p95 is 80–120 ms for all variants: coupling from DL activity, larger than in the earlier sessions. No shaper can control it.
- The session does not separate F8 from the cross delay. The simulator shows that F8 gives the most of the UL gain.

Decision (user agreed 2026-10-11): keep the default thresholds at 30/30 ms. The DL threshold is a user choice (delay vs throughput).

Update after the Copilot review of #18 (simulator, autorate-owd, 8 seeds):

| Variant | 5g-coupled UL | 5g-variable p95 | starlink-like p95 |
|---|---|---|---|
| main | 14 % | 53 ms | 33 ms |
| F8 | 71 % | 68 ms | 37 ms |
| `learn_cross_delay` | 88 % | 56 ms | 41 ms |
| F8 + learn | 88 % | 72 ms | 42 ms |

- F8 moves the operating point to a higher delay in all scenarios (also seen on the link in session 3: +20–45 ms p95). The coupling is an offset; `learn_cross_delay` removes it. Thus: no F8, PR #20 with `learn_cross_delay` only.
- `learn_cross_delay` on starlink-like: p95 33 → 41 ms (no coupling there). Use it only on 5G (PR #20 says this). A calibration (6b) can decide it: measure the coupling first.
- Copilot found on #19: after an UL capacity drop, Core learned the shaper queue (sim: p95 60 → 129 ms). Fix: learn only after 2 s of low UL load.
- Session 4 variants all had F8. A link test of `learn_cross_delay` alone is open: session 5.

### Session 5 (2026-10-11 12:45, `recordings/20261011-1245-enp7s0`): main vs `learn_cross_delay` alone (30/30, no F8), 27 blocks, 3 rounds

Mbit/s, values round 1 / 2 / 3. UL OWD p95 (pooled) in ms.

| Mode | main | learn |
|---|---|---|
| dl: DL | 130 / 83 / 165 | 128 / 144 / 99 |
| ul: UL (UL p95) | 23 / 21 / 23 (44/45/47) | 20 / 21 / 20 (46/45/42) |
| both: DL | 125 / 176 / 204 | 120 / 121 / 131 |
| both: UL (UL p95) | 16 / 10 / 7 (45/53/44) | 6 / 7 / 8 (52/64/82) |

Result: no gain, in both mode worse. **Cause: test method.** Each block starts the program again. In both mode, DL and UL load start at the same time: Core never sees "DL active, UL low" for 2 s. Replay of the recordings: learned UL cross delay 22 ms in the dl blocks (correct), 3 / 6 / 10 ms in the both blocks (learned only in the first seconds, then frozen). Thus session 5 did not test the option in both mode. In normal operation the program runs for a long time and learns in DL-only periods.

Fix for the test: PR #23 `record-session.sh -W SECONDS` (DL warm-up before both mode). Session 6 uses `-W 30`.

Also open (product, not test): if DL and UL are always active together, Core never learns. Possible later: keep the learned value over restarts (state file), or learn at the start of each DL burst before the UL ramps up. Decide after session 6.

### Session 6 (2026-10-11 14:13, `recordings/20261011-1413-enp7s0`): both mode, 30 s DL warm-up, 4 rounds

| Round | main DL / UL Mbit/s | learn DL / UL Mbit/s | main UL p95 | learn UL p95 |
|---|---|---|---|---|
| 1 | 115 / 5 | 95 / 5 | 75 | 91 |
| 2 | 96 / 6 | 86 / 11 | 77 | 108 |
| 3 | 88 / 6 | 111 / 9 | 75 | 114 |
| 4 | 115 / 5 | 120 / 19 | 87 | 108 |
| Mean | 104 / 5.5 | 103 / 11 | 79 | 105 |

Result: with the warm-up, `learn_cross_delay` doubles the upload under load in the two directions (5.5 → 11 Mbit/s, unshaped ~12–20) at the same download. Cost: UL OWD p95 +26 ms (79 → 105), DL p95 unchanged (45–58). This is the expected trade: the learned delay (~21 ms) raises the effective UL threshold to ~51 ms. Option to examine: `learn_cross_delay` with a lower UL threshold (20 ms → effective ~41 ms), but not below the grant floor (~25 ms per PR #22). Simulator first (5g-coupled, recorded scenarios after #25).

Simulator sweep (2026-10-11, branch of #25, refractory on, 8 seeds), UL threshold with `learn_cross_delay`:

| Scenario | off, 30 | learn, 30 | learn, 25 | learn, 20 |
|---|---|---|---|---|
| 5g-coupled p95 / UL % | 64.8 / 7 | 69.7 / 95 | 67.4 / 93 | 65.5 / 79 |
| 5g-recorded-a p95 / UL % | 63.7 / 11 | 79.2 / 77 | 75.2 / 73 | 66.6 / 51 |
| 5g-recorded-b p95 / UL % | 52.8 / 11 | 83.8 / 74 | 77.9 / 70 | 63.2 / 51 |

DL unchanged in all. learn + UL 20 gives 4.6x the upload for +3 to +10 ms p95 (learn + 30: 7x the upload, +5 to +31 ms). Candidate 5G profile: `learn_cross_delay: true`, UL threshold 20 to 25.

### Session 7 (prepared): main vs learn (UL 30) vs learn (UL 20), both mode with warm-up

Config `recordings/5g-s7-learn-u20.yaml` (= 5g-s5-learn + UL thr 20, adjust_down 40). 4 rounds x 3 policies + unshaped = 16 blocks, approximately 55 min.

### Session 7 (2026-10-11 15:10, `recordings/20261011-1510-enp7s0`): both mode, 30 s warm-up, 4 rounds

| Policy | DL Mbit/s (4 rounds) | mean | UL Mbit/s | mean | UL OWD p95 | mean | DL OWD p95 |
|---|---|---|---|---|---|---|---|
| main (30/30) | 115 111 106 119 | 113 | 16 6 5 5 | 8.0 | 73 62 65 73 | 68 | 46–73 |
| learn30 | 109 86 114 107 | 104 | 11 12 19 8 | 12.5 | 118 101 101 65 | 96 | 46–57 |
| learn20 | 114 90 108 106 | 104.5 | 7 18 8 19 | 13.0 | 96 89 86 84 | 89 | 47–53 |

Result: learn20 gives the same UL as learn30 (+60 % vs main) at 7 ms lower UL p95; DL −8 % for both learn variants. Noisy (UL per block 5–19). Sessions 6+7 together: learn_cross_delay roughly doubles/×1.6 UL under two-way load for +20–30 ms UL p95. 5G profile proposal: `learn_cross_delay: true`, UL threshold 20–25 ms (calibrate floor 23–25). Defaults stay 30/30, learn off.

### Session 8 (done 2026-10-11 16:20–17:18, `recordings/20261011-1620-enp7s0`): latency target on the link (PR #27 binary)

`recordings/5g-s8-ul-target60.yaml` = 5g-s5-learn + UL `latency_target_p95_ms: 60`, `latency_target_min_thr_ms: 25`, `latency_target_window_s: 20`. Binary `../cake-autorate-go-lt/cake-autorate-go` (41c53a4, rebuilt after the review fixes). Base policy: learn30 (main binary). Modes ul + both (warm-up), 3 rounds = 18 blocks, ~55 min:
`sudo ./scripts/record-session.sh -i enp7s0 -c recordings/5g-s5-learn.yaml -m "ul both" -r 3 -W 30 -A target60=recordings/5g-s8-ul-target60.yaml,../cake-autorate-go-lt/cake-autorate-go`
Expect (sim): UL p95 near 60–70 ms, more UL than learn30. Check the log for the threshold changes (`grep "latency target" NN-*.log`).
Result (excess RTT p50/p95, Mbit/s, base = learn30 vs target60):

| Mode | base UL | target60 UL | base RTT p95 | target60 RTT p95 |
|---|---|---|---|---|
| ul (3 rounds) | 22 20 21 | 24 24 25 | 49 44 50 | 64 70 74 |
| both DL/UL | 112/7 113/9 85/11 | 113/19 96/11 115/9 | 106 173 136 | 113 133 135 |

- ul: the target operates as designed. Log: UL OWD p95 per 20 s window 27–54 ms (below 60), threshold 30 → 33 → … → 48–53 ms at the block end (not converged in 150 s). One window at 92.8 ms gave a decrease 48 → 39. Result: +15 % UL for +20 ms RTT p95.
- both: UL OWD p95 88–115 ms also at the minimum threshold 25 ms → "not reachable" logged in each block; one floor undo (block 12). UL radio delay (cross + grant) is above 60 ms. Throughput differences in both are in the noise.
- Conclusion: the latency target is a correct bandwidth/latency trade-off knob for one-way load. For 5G with two-way load, a UL target below approximately 100 ms is not reachable; the log tells it. Longer blocks are necessary to see convergence (window 20 s, approximately 6 windows per block).
- Not done: pooled per-ping OWD analysis (the log windows give the same p95 information).

### Session 4 configs (prepared)

Main (`./cake-autorate-go`, F1 option) with `5g-owd-f1.yaml` vs branch feat/learn-cross-delay (`../cake-autorate-go-cross/cake-autorate-go`: F8 + cross delay for UL only) with `5g-s4-t15.yaml` (15/15), `5g-s4-d15u25.yaml` (DL 15, UL 25), `5g-s4-d15u30.yaml` (DL 15, UL 30). All: down 0.6, cross delay, F1. 30 blocks, approximately 90 min.

## Comparison with upstream (LAST step: after the plan is done and the tool is tuned; prepared, branch feat/upstream-compare, worktree ../cake-autorate-go-up, 3422428, not pushed)

- `record-session.sh -U NAME=CONFIG`: policy upstream-NAME. The original (clone in `../cake-autorate`, ac75f49) controls CAKE. cake-autorate-go measures with `--no-adjust` (same measurement for all policies).
- Configs: `recordings/upstream-5g-fping.sh` (RTT/2, echo reflectors), `recordings/upstream-5g-fping-ts.sh` (ICMP timestamps with fping, timestamp reflectors). Same rates and interfaces as `5g-owd.yaml`. fping 5.5 output matches the upstream fping-ts parser (checked on loopback).
- Pairs: upstream fping vs ours `ping_method: icmp`; upstream fping-ts vs ours `icmp-timestamp` (best config after session 3).
- First on the fiber emulation (repeatable trace, no 5G data), then a short 5G session. Resource usage: run `benchmark.sh` again on the GL-MT6000.

## Next steps (revised 2026-10-11 19:40)

Connection: back on the fiber line since 2026-10-11 evening (5G day pass used up). No 5G tests until the next 5G day pass. Local tests on fiber: CI emulation (no internet), `emulate-link.sh` with 5G traces.


1. Merged 2026-10-12: #26 (calibrate), #29 (CI emulation), #34 (known-issue links). #27 (latency target) parked as draft 2026-10-12 (user decision): small gain on the link (UL-only +15 % UL for +20 ms p95; both directions: target not reachable), high complexity (~15 Copilot cycles, each found a new edge case). Startup fix in a local stash in ../cake-autorate-go-lt. Revisit with Starlink data or user demand.
2. Upstream comparison on 5G (step 8): moved to another day (user decision 2026-10-11 19:20). Needs a new 5G data pass. Branch `feat/upstream-compare` is rebased and ready; run the session below, then the emulation comparison.
3. After #27: `calibrate` proposes `latency_target_min_thr_ms` from the grant floor (small PR on #26 code).
4. Fiber emulation upstream comparison done 2026-10-12 (`recordings/20261012-0834-emu-upstream`, trace-5g.csv 400/80 max, 2 rounds, ours icmp-timestamp defaults vs upstream fping-ts, same rates and reflectors). Clean modes (excess RTT p50/p95/p99, throughput % of trace capacity):
   - DL: ours 1.4–1.6 / 43–44 / 64–67 ms, 74–75 %; upstream 5.7–7.9 / 66–69 / 89–92 ms, 89–91 %; unshaped p95 175–601 ms.
   - UL: ours 9–10 / 39–40 / 165–178 ms, 86–87 %; upstream 2.1–5.3 / 63–64 / 265–280 ms, 87 %.
   - Both directions: NOT valid in this run. Link outages of 15–65 s (DL 0 bytes, all pings lost) in unshaped and ours blocks. Cause found with `stats.tsv` (diag run `20261012-0949-emu-diag`): emulation bug. GRO on enp7s0 gave the DL bottleneck packets up to 64 KB; tbf does not send a packet larger than its burst (1 ms at the trace rate); when the trace lowered the rate with such a packet queued, the queue stopped (full, 1000 packets, 61 s) until the rate rose above ~120 Mbit/s. Fix: `emulate-link.sh` turns off GRO on the uplink during the run (feat/upstream-compare). Next: repeat the both-direction comparison (`-m both -r 2`). Diag block without outage (single round): ours p95 64 ms, DL 65 %, UL 78 %; upstream p95 92 ms, DL 74 %, UL 75 %.
   - Bug found by the outages: health check restarted pingers in STALL; on STALL → RUNNING a second set started and the first never stopped (3× ping rate to all 8 reflectors after 2 outages). PR #35, merged 2026-10-12 (also joins the pinger goroutines on stop).
   - Both directions after the GRO fix (`recordings/20261012-1031-emu-both`, 2 rounds, no outages, no STALL): ours p50/p95/p99 15.6–17.3 / 55–57 / 490–611 ms, DL 67–69 %, UL 77–78 %; upstream 23–26 / 96–106 / 303–376 ms, DL 72–74 %, UL 75–79 %; unshaped p95 284–325 ms.
   - Ours p99 tail: two UL capacity cliffs in the trace (80 → 5 Mbit/s for 1.2 s at 80.6 s and 101.6 s). Ours has UL shaped at 57–78 Mbit/s (close to capacity), the bottleneck queue fills, UL OWD 620–760 ms for ~2 s; ours cuts to 4 Mbit/s in < 0.5 s, but the queue is already full. Upstream gets 290–410 ms there. Trade-off of a shaper close to capacity; only these 2 events. Idea: faster/larger cut when the UL OWD jumps by several 100 ms (bufferbloat at a cliff), or check if the 5G trace cliffs to the 5 Mbit/s floor are real (trace generator clamps at min).
   - Result: ours has much lower typical delay (p95 −35…−45 % in all modes), upstream gets +5…+15 points DL throughput; UL equal. p99 in both-direction mode worse for ours (capacity cliffs). Next tuning targets: DL throughput (increase rate faster after a cut?) and the cliff response.
   - Correction 2026-10-12: the earlier DL-only results (ours 74–75 %) had GRO effects too (short tbf stalls). After the GRO fix (`recordings/20261012-1059-emu-rise`): ours DL 87–88 %, upstream 93–95 %.
   - DL gap cause: decrease cascade (median 7 decreases in 0.3 s per bufferbloat event, rate to 0.59×; detection window keeps the delayed pings). Tried in sim: increase gate per rate sample (rejected: less throughput), `refractory_on_decrease` (too much delay), `repeat_decrease_on_rise` (decrease again in the refractory period only while the delta rises). Emulation A/B: rise gives DL 93 % (= upstream) for +1 ms p95; both directions +2/+4 points for +6 ms p95. Sim: bad with RTT/2. PR #36 merged 2026-10-12: option, default false (user decision 2026-10-12: keep it off by default), README recommends with icmp-timestamp.
   - Remaining: p99 at UL capacity cliffs (both directions). Check on the next 5G day pass with rise.
5. Step 8 without 5G: upstream comparison on the CI emulation (repeatable, no data cost): add `-U` to `ci-emulation.sh` or run `emulate-link.sh -- -U`. darkmoon: build for x86 in the OpenWrt VM (step 9 setup), then the same emulation; resource use with `benchmark.sh` on the GL-MT6000.
6. 6b online part (`calibrate --iface`): run the session steps itself (lower priority; the offline flow works).
7. Step 9: LuCI/OpenWrt package (`PLAN-LUCI.md`). Needs the QEMU VM (user: `modprobe kvm_amd`, `pacman -S qemu-base`).
8. Step 10: Starlink from 2026-10-28 (branch `feat/starlink-reconfig`).
9. Known issues are GitHub issues #30 (F1), #31 (F2), #32 (F4), #33 (F5); PR #34 links them in the CI summary. `refractory_on_decrease` as default rejected 2026-10-11: sim baseline 5g-variable OWD p95 58 → 115 ms (more throughput, much more delay). Idea for F1/F2 without that cost: block repeated cuts only during a short spike (e.g. no second cut while the delta decreases).
10. Housekeeping: remove old worktrees; root-owned `recordings/20261011-1610-enp7s0` (user: sudo rm).

5G tests still useful (next 5G day):
- Upstream vs ours on 5G (step 8): `-U fts=recordings/upstream-5g-fping-ts.sh` vs `5g-s7-learn-u20.yaml` (our best 5G profile), modes `ul both`, `-W 30`, 2 rounds = 12 blocks ≈ 40 min. Gives the main result of the comparison on the real link.
- Not needed: a re-test of the latency target after the #27 changes (they affect the reference at moderate load and RTT mode; session 8 used OWD at full load, the main path did not change).

## Fiber dry run (2026-10-10)

- Line: UniFi UXG, Smart Queue 630/320 Mbit/s. Saturation check: 616/321 Mbit/s with 4 workers. The tools operated correctly.
- Bufferbloat is very low because of the Smart Queue: unshaped, loaded p99 RTT 13.5 ms (dl) and 10 ms (ul).
- **Bug found (PR #8):** links without delay thresholds got thresholds of 0. Autorate went to min_rate in 3 s. The autorate blocks are not valid. Do them again after PR #8 (rebuild the binary).
- Analysis: the idle RTT is different for each reflector (Cloudflare approximately 2 ms, others 6 to 8 ms). The analysis tool (2c) must calculate the excess RTT for each reflector (RTT minus the minimum of the reflector), not from all pings together.
- `record-session.sh`: the files belonged to root. Fixed (chown to SUDO_USER).

## Lessons from steps 1 and 2

1. **The latency under load is high because of the default thresholds.** With steady load, autorate keeps 40 to 65 ms of excess RTT. The threshold is 30 ms OWD, but the OWD is RTT/2. Thus, the RTT can increase by 60 ms before a cut. To get a low latency, we must fix F4 and then tune the thresholds.
2. **F4 has the largest effect.** It decreases the DL throughput by approximately half when the UL is saturated. On 5G and Starlink, the UL is frequently saturated.
3. **One seed for each scenario is not sufficient for tuning.** If we tune on one random trace, the result can be correct only for that trace. Use more seeds for each scenario before step 6.
4. **The baseline check stops regressions, but it does not require improvements.** Update the baseline after each fix. Then the next change must be equal or better.
5. **The simulator has limits.** It uses a fluid model with Reno growth (real CUBIC and BBR increase faster). It has no random packet loss and no ping timeouts. It assumes that CAKE fully isolates the sparse flows. The 5G recordings must show if these limits are important.
6. **The recordings are on the critical path.** We cannot calibrate the simulator without them. Thus, `--record` must come first in step 2b.

## Test strategy

### Level 1: deterministic `Core` (done)

- `Core` gets events (`HandlePing`, `HandleRate`) with timestamps.
- `Core` sends its decisions through the `Actuator` interface (`SetRate`, `StartPingers`, `StopPingers`, `InvalidateShaper`).
- `LinkController` runs the goroutines and the I/O.

### Level 2: link simulator (done, calibration open)

- Files: `sim_test.go`, `sim_scenarios_test.go`, `sim_baseline_test.go`, `testdata/sim_baseline.json`.
- Commands:
  - Scorecard: `go test -run TestSimScorecard -v`
  - Baseline check: `go test -run TestSimBaseline`
  - Accept new scores: `go test -run TestSimBaseline -update`
- CI writes the baseline comparison, the scorecard and the known issues to the job summary.
- Risk: we tune the controller for the simulator, not for real links. To decrease this risk, use our own recordings and public traces.

### Level 3: emulation with network namespaces (later)

- Use the real binary, the real CAKE qdisc, `netem` with trace replay, and `iperf3` or `flent`.
- Run it in GitHub Actions with sudo.
- It tests the netlink and pinger code. The simulator does not test this code.

## Roadmap

- [x] **1. Deterministic `Core`** (PR #4).
- [x] **2a. Simulator, scorecard, known-issue tests, baseline check** (PR #5, PR #6).
- [x] **2b. `--record`** (PR #7). Unblocks the 5G recordings.
  - JSONL with gzip (`.gz`) and a size limit (`--record-max-mb`). Events: `header`, `start`, `ping`, `rate`, `reflector_reset`, `shaper`, `state`.
  - `RecordingCore` is used by the controller and by the simulator. A test replays simulator recordings and gets the same decisions.
  - Procedure in `docs/recording.md`, for a router and for a PC without a router (`scripts/cake-pc.sh`).
  - File size: approximately 13 MB/h (`.jsonl`) or 1.5 MB/h (`.jsonl.gz`).
  - Session tools: `scripts/record-session.sh` (saturation check, blocks that alternate unshaped/fixed/autorate), `--no-adjust`, `load-gen.sh` fixes.
  - [x] Dry run on the fiber connection of the PC (2026-10-10). It found the threshold bug (PR #8).
  - [x] Real 5G sessions (2026-10-10/11): 3 baseline sessions, 3 A/B sessions.
- [x] **2b+. 5G emulation on fiber** (`scripts/emulate-link.sh`, after the fiber dry run).
  - Network namespace "router" (load-gen, cake-autorate-go, CAKE) → veth → root namespace with a variable bottleneck (tbf with a large queue, rate from a trace, netem for base RTT) → enp7s0.
  - Home line: UniFi UXG, Smart Queue 630/320 Mbit/s. Keep it on. Emulated rates must stay ≤ 70 % of it: DL ≤ 440 Mbit/s, UL ≤ 220 Mbit/s.
  - Traces: simulator traces first, then Mahimahi, then the capacity of the real 5G recordings.
  - Later: the same setup without internet (local iperf3 namespace) for CI (step 7).
- [x] **2c. Replay and trace import.** Done 2026-10-11 (PR #25, merged). Gap sim vs link explained by two causes: (1) 1-s mean in trace-from-session hides sub-second capacity dips (5–10 % of 200 ms samples < half of the 2 s mean); (2) recorded scenarios had no UL radio delays. With raw traces (`-r`) + coupling/grant: dl-only DL 36–39 % (link 22–50), ul-only UL 57–61 % (link 56–63). `TestSimCalibration_5GRecorded` keeps it. Remaining: UL capacity under DL load is ~1/3 on the link (11 vs 36 Mbit/s), not modelled. Also found: DL OWD under DL load has a heavy tail (p95 33–94 ms, also at shaper 10–40 % of mean capacity), mostly in bursts: short capacity dips.
  - Replay a recording through `Core` as a test.
  - Import capacity traces (CSV, Mahimahi).
  - Calibrate the simulator with the 5G recordings. Compare the queue delay under load, the jitter and the capacity changes.
- [x] **3. Fixes for parity with upstream.** Done 2026-10-11. F1 option (#11), F2 (#11), F7 closed, F8 rejected (#18 closed), F3 rejected (#24). Known-issue tests 1, 2, 4 stay: they document RTT/2 limits (the fixes are for one-way delays).
  - F1: refractory period for decreases.
  - F2: cut from `min(achieved, shaper)`.
  - F3: use the delta EWMA for the `avg_owd_delta_*` thresholds. Add an isolated test.
  - F7: calculate the delta before the baseline update. Closed: upstream does the same.
  - F8: increase up to the delay threshold (upstream factor). Branch `fix/increase-to-threshold`.
  - Change each known-issue test into a regression test when its issue is fixed. Update the baseline.
- [x] **4. OWD for each direction (F4).** PR #10. Confirmed on 5G.
  - Use ICMP timestamp requests (types 13 and 14, as `tsping` does). If a reflector does not reply, use RTT/2.
  - Risk: some reflectors do not reply to ICMP timestamp requests. Make a list of reflectors that reply.
  - Add a config key (for example `ping_method`). The default does not change the behavior.
- [x] **5. Handling for 5G.** Done 2026-10-11: `learn_cross_delay` (#20), UL grant-delay model and threshold floor (#22), F2 fast cut after capacity drops (#11). Starlink moved to step 10.
- [x] **6. Tuning.** Done for 5G 2026-10-11: sessions 3–8, sweeps on 8 seeds. Defaults stay 30/30, learn_cross_delay off; 5G profile documented (#28). Profiles for Starlink: step 10.
  - Use more seeds for each scenario and the recorded traces.
  - Do parameter sweeps on the simulator. Then, examine the best values on the real links.
  - Make the defaults better, or add link profiles (for example `profile: starlink`).
- [~] **6b. Calibration mode** (offline part: PR #26 merged 2026-10-12; online part `--iface` open) (after step 6: tuning shows which parameters matter).
  - 2026-10-11: offline part, PR #26 `cake-autorate-go calibrate -session DIR` (worktree ../cake-autorate-go-calib). Copilot up to 6 rounds (user request). Rules: rates from unshaped 1 s means (min p5/2, base p25, UL base ≤ p50 with DL load, max p95); DL thr max(30, 2× idle jitter); learn_cross_delay if cross p50 > 10 ms (from autorate dl blocks); UL thr ≥ grant p50 + 10 ms; reflectors: best pinger_count+2 by idle jitter. On 3 sessions: cross p50 25–33, grant p50 13–15, UL base ~10 Mbit/s. Open: online part (run the measurements), emit `latency_target_min_thr_ms` for UL from the grant floor once #27 merges.
  - Goal: at a new place (other 5G cell, Starlink), measure the link and generate a fitting config file.
  - `cake-autorate-go calibrate --iface IFACE --out FILE.yaml`. It uses the session tools (load generation, saturation check, recording). It can also use a saved recording (offline).
  - Measurements and the config values that they set:
    - Capacity range (saturation check, more than one time) → `min/base/max_rate_kbps` for each direction.
    - Idle RTT floor and jitter for each reflector (60 s) → delay thresholds.
    - Reflectors that reply here (timestamp test, lowest jitter) → `reflectors`, `ping_method`.
    - UL delay from DL load (short DL load, UL OWD) → `learn_cross_delay`, UL threshold.
    - Link type: RTT floor steps at seconds 12/27/42/57 → Starlink profile (step 5).
    - Optional: short closed-loop test of 2-3 threshold candidates.
  - The generated file has a comment for each value with the measured data.
  - Data: approximately 3-5 GB on 5G (mostly the saturation check).
  - The cell changes during the day. Thus, calibration sets the ranges and the start values. Values that change (cross delay, threshold from the idle jitter) adapt at run time.
- 5G profile docs: PR #28 (README "5G Links": learn_cross_delay, UL threshold 20, learning condition).
- [-] **6c. Latency target mode** (PR #27 parked as draft 2026-10-12; session 8 done) (after 6, with or after 6b). User idea 2026-10-11: "max bandwidth for a given latency target".
  - 2026-10-11: PR #27 (worktree ../cake-autorate-go-lt). Per direction `latency_target_p95_ms`, `latency_target_min_thr_ms`; global `latency_target_window_s` (30). Scales the 3 thresholds ×0.8/×1.1 per window, range [min/thr, 3]. Floor detection: undo a decrease without ≥5 % p95 gain, keep that floor 5 min. Sim (10 min, 4 seeds): steady-dl target 20 → 21 ms; 5g-coupled UL 40/60/80 → 39/69/87 ms at 44/68/76 % (default 55 ms, 61 %); recorded UL 60 → 63–66 ms at 52–53 % (default 34–40 %). Recorded DL: weak effect (capacity dips). Needs a link test.
  - Problem: the thresholds are an indirect control. The delay that users see under load is threshold + overshoot + radio floor. Session 4: DL threshold 15 → DL p95 ~33 ms, 30 → ~50 ms. The same threshold gives a different p95 on another cell.
  - Design: a slow outer loop (window 60 s or more) above `Core`. It measures the p95 of the OWD delta while the load is high (achieved > 75 % of the shaper rate). It adjusts the delay threshold (and maybe `adjust_down`) of each direction: p95 above the target → threshold down, p95 below the target with margin → threshold up. Small steps, limits `[min_thr, max_thr]`.
  - Floor: the loop must not set a threshold below the delay that no shaper can remove (5G UL grant delay, cross delay, Starlink floor steps). Else the rate collapses (session 3/4: UL threshold 15 → UL 30–43 %). Use the learned cross delay and the measured floor (6b). If the target is below the floor: log "target not reachable", keep the minimum useful threshold.
  - Config proposal (off by default; thresholds are the start values):
    ```yaml
    latency_target:
      dl_p95_ms: 40
      ul_p95_ms: 50
      window_s: 60
    ```
  - Alternative (simpler): one knob `balance: 0..1` that selects threshold presets. Less exact, does not adapt to the cell. Prefer the target.
  - Determinism: the outer loop is in `Core` (time from events), so record/replay and the simulator can test it. Simulator test: the same target on 5g-variable and 5g-coupled gives p95 near the target and higher throughput than a fixed low threshold.
- [x] **7. CI emulation** (level 3). PR #29, merged 2026-10-12. CI: 8 runs + 1 local run, autorate p95 42–46 ms, DL 85–87 %, UL 76–78 %.
  - 2026-10-11: branch `ci/emulation` (PR #29). `scripts/ci-emulation.sh`: 3 namespaces (cie-router: CAKE via cake-pc.sh + binary + iperf3 clients + ping probe; cie-modem: tbf+netem bottleneck on each egress, square wave 40/20 Mbit/s DL, 10/5 UL, 15 s; cie-server: reflectors 10.210.2.1-6 on lo, kernel ICMP timestamp replies, iperf3 servers). Phases unshaped vs autorate (10 s idle + 60 s both-direction load). Checks: unshaped p95 ≥ 100 ms, autorate p95 ≤ 60 ms, DL ≥ 50 %, UL ≥ 40 % of mean capacity, ≥ 10 shaper changes, ≥ 100 OWD pings. CI job `emulation` (apt iperf3, ethtool, linux-modules-extra). Review fixes: netlink check (fails on a tc fallback), rate-wave liveness, time-weighted mean capacity, portable p95, all namespaces checked before setup, limits as environment variables. First CI run passed with margin (see Status).
- [ ] **8. Comparison with upstream and darkmoon** (before 9 and 10). Latency, throughput (session with `-U`, emulation first, then 5G) and resource use (`benchmark.sh` on the GL-MT6000).
  - darkmoon (github.com/kamikaonashi/openwrt-package-darkmoon, MIT, C, 5434c96 2026-03-14): C port of upstream for OpenWrt with uloop, netlink, UCI, procd and a LuCI app. Add it as a policy in `record-session.sh` (like `-U`) and in `benchmark.sh` (CPU/RAM on the GL-MT6000).
  - Algorithm = upstream: window average delta for the size (F3), increase up to the delay threshold (F8, which we rejected), cut from the shaper rate (no F2), refractory after each cut (F1 on), RTT/2 by default. No learned cross delay, no grant-delay floor. Thus, expected on 5G: like upstream fping-ts, higher p95 than ours, UL low with DL load.
  - It sets up CAKE and IFB itself (with cake-mq). We need CAKE already configured (sqm-scripts). One procd instance for each UCI section (multi-link by sections). Status JSON in /var/run every 200 ms for the LuCI status widget.
- [ ] **9. OpenWrt package and LuCI app** (after 8). Detailed plan: `PLAN-LUCI.md`.
  - Package: OpenWrt Makefile (Go cross-build), procd init with one instance per link or one process with all links (we support multiple links in one process: keep one process).
  - Config: UCI `/etc/config/cake-autorate` → generated YAML (init script, `uci` → YAML), or native UCI read in Go. Prefer generation: the YAML stays the single format, tests do not change.
  - LuCI (JS view, `form.GridSection`/`TypedSection` with addremove for links): one section per link (interfaces, min/base/max, thresholds per direction, reflectors, ping method, `learn_cross_delay`), one global section (pinger, EWMA, refractory, sleep, stall, logging). Tabs: General, Links, Advanced, Status.
  - Status: write a status JSON (state, shaper and achieved rates, deltas, learned cross delay for each link) and a LuCI status include (like darkmoon `75_darkmoon.js`).
  - Calibration button: rpcd/ucode call that runs `cake-autorate-go calibrate` (6b) for one link, shows the result, and applies it to UCI on confirmation.
  - Optional: CAKE setup by the package (as darkmoon) so sqm-scripts is not necessary.
  - Test without the router: OpenWrt x86-64 image in QEMU/KVM (real kernel with CAKE, IFB, netlink, procd, rpcd, LuCI; same as the target except the CPU). Host: AMD with svm, but kvm_amd not loaded and no qemu installed (2026-10-11): user runs `sudo modprobe kvm_amd` and `sudo pacman -S qemu-base`. LuCI through port forward (hostfwd 8080 → 80). Optional: aarch64 image (armsr/armv8) with qemu-system-aarch64 (TCG, slow) to test the arm64 binary of the GL-MT6000. The same VM can also serve step 7 (CI emulation). Docker (openwrt/rootfs) is lighter but uses the host kernel: good for the UI only.
- [ ] **10. Starlink: measurement and tuning** (dedicated last step; Starlink access only from **2026-10-28**). User decision 2026-10-11.
  - 2026-10-11: branch `feat/starlink-reconfig` (7af38ac, pushed, **no PR yet**: needs real Starlink data). Option `starlink_reconfiguration` (off by default, OWD only). Measures the floor step in 600 ms after each 15 s boundary (smallest delta increase, smaller of the two directions or the idle direction), then shifts all baselines. Sim: +40 ms floor step at s57 → DL shaper 108 (off) vs 157 (on) Mbit/s; starlink-like 8 seeds p95 33.1 → 34.1, DL 89.6 → 89.0. RTT/2: worse (cannot separate floor and queue) → OWD only.
  - Source of the 15 s / 12-27-42-57 schedule: public measurement studies of Starlink (for example Mohan et al., "A Multifaceted Look at Starlink Performance", WWW 2024) and the cake-autorate Starlink discussions. From model knowledge, not checked in this session. Verify with our own Starlink recordings (from 2026-10-28): plot the OWD per second-of-minute, check the steps at 12/27/42/57 and whether the step is the same in the two directions. If the offsets differ: make period and offset config keys.
  - Sessions on Starlink with `record-session.sh` (dl / ul / both, unshaped + autorate + `starlink_reconfiguration` variant). Trace import (`trace-from-session.sh -r`) → `starlink-recorded` scenario, calibrate `starlink-like` like 2c.
  - Then: PR for `starlink_reconfiguration` (or reject it), Starlink profile (thresholds), Starlink part of the calibration mode (6b: detect the 15 s floor steps) and of the comparison (8).
