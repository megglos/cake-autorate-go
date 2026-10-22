# cake-autorate-go: session results

> Reference for the results of the test sessions (5G link, fiber line, fiber emulation). PLAN.md links here.
> Language: ASD-STE100. Recordings are in `recordings/` of the main worktree (not in git).

## Contents

- [Fiber dry run (2026-10-10)](#fiber-dry-run-2026-10-10)
- [Emulation on fiber (2026-10-10)](#emulation-on-fiber-2026-10-10)
- [5G sessions 1–3 (2026-10-10)](#first-5g-session-2026-10-10-2138)
- [A/B sessions 1–8 on 5G (2026-10-11)](#ab-session-1-2026-10-11-0504)
- [Fiber emulation: comparison with upstream (2026-10-12)](#fiber-emulation-comparison-with-upstream-2026-10-12)

## Fiber dry run (2026-10-10)

- Line: UniFi UXG, Smart Queue 630/320 Mbit/s. Saturation check: 616/321 Mbit/s with 4 workers. The tools operated correctly.
- Bufferbloat is very low because of the Smart Queue: unshaped, loaded p99 RTT 13.5 ms (dl) and 10 ms (ul).
- **Bug found (PR #8):** links without delay thresholds got thresholds of 0. Autorate went to min_rate in 3 s. The autorate blocks are not valid. Do them again after PR #8 (rebuild the binary).
- Analysis: the idle RTT is different for each reflector (Cloudflare approximately 2 ms, others 6 to 8 ms). The analysis tool (2c) must calculate the excess RTT for each reflector (RTT minus the minimum of the reflector), not from all pings together.
- `record-session.sh`: the files belonged to root. Fixed (chown to SUDO_USER).

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

## A/B session 2: rebaseline (2026-10-11, 06:56)

Plan: f1f2 vs f1f2 + `rebaseline_at_min_rate_ms: 2000`. Binary `../cake-autorate-go-coupling/cake-autorate-go` for both, configs `recordings/5g-owd-f1.yaml` and `recordings/5g-owd-f1-rb.yaml`. 24 blocks, approximately 72 minutes.

Session `recordings/20261011-0656-enp7s0`: f1f2 vs f1f2 + `rebaseline_at_min_rate_ms: 2000`. Result (2 rounds): no difference larger than the noise. "both" UL: 6 / 6 (f1f2) vs 6 / 7 Mbit/s (rb). Rebaseline is not useful on this link. Do not open a PR for it.
- Cause: the UL delay from DL activity is noisy. Per ping, under load: p10 10, p50 21, p90 38, p95 46 ms. 21 % of the pings are above 30 ms. Thus, the UL shaper goes up and down between 4 and 7 Mbit/s. It does not stay at the minimum rate, and the rebaseline does not occur.
- Simulator: with this noise (`ulCouplingJitterMs`), the simulator shows the same result (rebaseline has no effect, UL 51 %). The coupling model is now 10 ms + exponential noise with a mean of 12 ms.

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

## Session 4 configs

Main (`./cake-autorate-go`, F1 option) with `5g-owd-f1.yaml` vs branch feat/learn-cross-delay (`../cake-autorate-go-cross/cake-autorate-go`: F8 + cross delay for UL only) with `5g-s4-t15.yaml` (15/15), `5g-s4-d15u25.yaml` (DL 15, UL 25), `5g-s4-d15u30.yaml` (DL 15, UL 30). All: down 0.6, cross delay, F1. 30 blocks, approximately 90 min.

## Session 5 (2026-10-11 12:45, `recordings/20261011-1245-enp7s0`): main vs `learn_cross_delay` alone (30/30, no F8), 27 blocks, 3 rounds

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

## Session 6 (2026-10-11 14:13, `recordings/20261011-1413-enp7s0`): both mode, 30 s DL warm-up, 4 rounds

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

## Session 7 (2026-10-11 15:10, `recordings/20261011-1510-enp7s0`): both mode, 30 s warm-up, 4 rounds

Policies: main vs learn (UL 30) vs learn (UL 20). Config `recordings/5g-s7-learn-u20.yaml` (= 5g-s5-learn + UL thr 20, adjust_down 40).

| Policy | DL Mbit/s (4 rounds) | mean | UL Mbit/s | mean | UL OWD p95 | mean | DL OWD p95 |
|---|---|---|---|---|---|---|---|
| main (30/30) | 115 111 106 119 | 113 | 16 6 5 5 | 8.0 | 73 62 65 73 | 68 | 46–73 |
| learn30 | 109 86 114 107 | 104 | 11 12 19 8 | 12.5 | 118 101 101 65 | 96 | 46–57 |
| learn20 | 114 90 108 106 | 104.5 | 7 18 8 19 | 13.0 | 96 89 86 84 | 89 | 47–53 |

Result: learn20 gives the same UL as learn30 (+60 % vs main) at 7 ms lower UL p95; DL −8 % for both learn variants. Noisy (UL per block 5–19). Sessions 6+7 together: learn_cross_delay roughly doubles/×1.6 UL under two-way load for +20–30 ms UL p95. 5G profile proposal: `learn_cross_delay: true`, UL threshold 20–25 ms (calibrate floor 23–25). Defaults stay 30/30, learn off.

## Session 8 (done 2026-10-11 16:20–17:18, `recordings/20261011-1620-enp7s0`): latency target on the link (PR #27 binary)

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

## Fiber emulation: comparison with upstream (2026-10-12)

Setup: `emulate-link.sh` (branch `feat/upstream-compare`), trace `recordings/trace-5g.csv` (DL 30–400 Mbit/s, median 238; UL 5–80 Mbit/s, median 21.6), extra base RTT 20 ms, queue 1000 packets. Ours: `recordings/emu-ours-ts.yaml` (`icmp-timestamp`, defaults). Upstream: `recordings/upstream-emu-fping-ts.sh` (fping-ts, same rates and reflectors). Values: excess RTT p50 / p95 / p99, throughput in % of the trace capacity.

First run (`recordings/20261012-0834-emu-upstream`, 2 rounds, GRO still on: see the correction below):

- DL: ours 1.4–1.6 / 43–44 / 64–67 ms, 74–75 %; upstream 5.7–7.9 / 66–69 / 89–92 ms, 89–91 %; unshaped p95 175–601 ms.
- UL: ours 9–10 / 39–40 / 165–178 ms, 86–87 %; upstream 2.1–5.3 / 63–64 / 265–280 ms, 87 %.
- Both directions: NOT valid in this run. Link outages of 15–65 s (DL 0 bytes, all pings lost) in unshaped and ours blocks. Cause found with `stats.tsv` (diag run `20261012-0949-emu-diag`): emulation bug. GRO on enp7s0 gave the DL bottleneck packets up to 64 KB; tbf does not send a packet larger than its burst (1 ms at the trace rate); when the trace lowered the rate with such a packet queued, the queue stopped (full, 1000 packets, 61 s) until the rate rose above ~120 Mbit/s. Fix: `emulate-link.sh` turns off GRO on the uplink during the run (feat/upstream-compare). Diag block without outage (single round): ours p95 64 ms, DL 65 %, UL 78 %; upstream p95 92 ms, DL 74 %, UL 75 %.
- Bug found by the outages: health check restarted pingers in STALL; on STALL → RUNNING a second set started and the first never stopped (3× ping rate to all 8 reflectors after 2 outages). PR #35, merged 2026-10-12 (also joins the pinger goroutines on stop).
- Both directions after the GRO fix (`recordings/20261012-1031-emu-both`, 2 rounds, no outages, no STALL): ours p50/p95/p99 15.6–17.3 / 55–57 / 490–611 ms, DL 67–69 %, UL 77–78 %; upstream 23–26 / 96–106 / 303–376 ms, DL 72–74 %, UL 75–79 %; unshaped p95 284–325 ms.
- Ours p99 tail: two UL capacity cliffs in the trace (80 → 5 Mbit/s for 1.2 s at 80.6 s and 101.6 s). Ours has UL shaped at 57–78 Mbit/s (close to capacity), the bottleneck queue fills, UL OWD 620–760 ms for ~2 s; ours cuts to 4 Mbit/s in < 0.5 s, but the queue is already full. Upstream gets 290–410 ms there. Trade-off of a shaper close to capacity; only these 2 events. Idea: faster/larger cut when the UL OWD jumps by several 100 ms (bufferbloat at a cliff), or check if the 5G trace cliffs to the 5 Mbit/s floor are real (trace generator clamps at min).
- Result: ours has much lower typical delay (p95 −35…−45 % in all modes), upstream gets +5…+15 points DL throughput; UL equal. p99 in both-direction mode worse for ours (capacity cliffs). Next tuning targets: DL throughput (increase rate faster after a cut?) and the cliff response.
- Correction 2026-10-12: the earlier DL-only results (ours 74–75 %) had GRO effects too (short tbf stalls). After the GRO fix (`recordings/20261012-1059-emu-rise`): ours DL 87–88 %, upstream 93–95 %.
- DL gap cause: decrease cascade (median 7 decreases in 0.3 s per bufferbloat event, rate to 0.59×; detection window keeps the delayed pings). Tried in sim: increase gate per rate sample (rejected: less throughput), `refractory_on_decrease` (too much delay), `repeat_decrease_on_rise` (decrease again in the refractory period only while the delta rises). Emulation A/B: rise gives DL 93 % (= upstream) for +1 ms p95; both directions +2/+4 points for +6 ms p95. Sim: bad with RTT/2. PR #36 merged 2026-10-12: option, default false (user decision 2026-10-12: keep it off by default), README recommends with icmp-timestamp.
- p99 analysis (2026-10-12, after PR #37): the p99 in the both-direction mode comes from the deep fades of the trace, for all policies. Seconds with excess RTT > 150 ms (`20261012-1059-emu-rise`) are the same in all blocks: 80–82 s (UL 50 → 5 Mbit/s for 1.6 s), 102–103 s (UL 74 → 5 Mbit/s for 1.6 s) and 180–183 s (DL 350 → 30 Mbit/s for 1.2 s, then UL 16 → 5 Mbit/s for 2.4 s). Peak excess RTT in these seconds: ours 494–844 ms, rise 550–763 ms, upstream 584–631 ms.
- Pings with excess RTT (whole block, both mode, 4 pairs from `1031-emu-both` and `1059-emu-rise`):

  | Policy | > 100 ms | > 200 ms | > 400 ms |
  |---|---|---|---|
  | ours | 118–139 | 63–75 | 31–53 |
  | ours + rise | 190–200 | 62–68 | 44–45 |
  | upstream fping-ts | 371–427 | 38–80 | 21–46 |

  Upstream has 3× more pings above 100 ms. Above 400 ms, ours has 0–30 more pings for each block (< 1 % of 3970 pings), thus the p99 difference. Cause: ours shapes closer to the capacity, and a fade fills the 1000-packet queue before the first cut takes effect (at 40 Mbit/s excess, 100 ms of reaction fills 0.5 MB = 0.8 s at 5 Mbit/s). A controller cannot prevent most of this delay. A faster cut at a very large delta can remove a part. Low priority for 5G; check again for Starlink (handover every 15 s).

### A/B: `repeat_decrease_on_rise` (`recordings/20261012-1059-emu-rise`, after the GRO fix, 2 rounds)

| Mode | ours default | ours + `repeat_decrease_on_rise` | upstream fping-ts |
|---|---|---|---|
| DL | 7–9 / 42–43 / 58–63 ms, 87–88 % | 14–15 / 44–45 / 62–66 ms, 93 % | 13–17 / 59–60 / 80–109 ms, 93–95 % |
| both, DL / UL | 15–17 / 56–57 / 380–448 ms, 67–68 / 78 % | 21–22 / 63 / 475–505 ms, 69–70 / 81–82 % | 21–23 / 85–89 / 251–425 ms, 75–78 / 75–78 % |

Simulator for F1/F2 with the option (issues #30/#31): latency-spike minimum DL rate 42 → 58 % (`icmp`), 8 → 73 % (`icmp-timestamp`); capacity-drop no change (25 %).

## 5G live test of stall_recovery_ms (2026-10-21, PR #106)

Setup: cav1 on the 5G modem (Telekom mobile, 80.187.87.150), host on Wi-Fi. Build of PR #106 (d48b040), calibration of 2026-10-18 (DL 34/130/245, UL 19/94.5/110, thresholds 36 ms, icmp-timestamp). LuCI bufferbloat test (shaped), runs in the order A1 B1 C1 A2 B2 C2. A: stall_recovery_ms 0, DL thr 36. B: 500, 36. C: 500, 22. Unshaped (A1): DL 206 (p95 107, B), UL 99 (p95 95, C), both 188/77.

| Run | DL | UL | both | DL p95 (DL phase) | deep DL episodes | recoveries |
|---|---|---|---|---|---|---|
| A1 | 165 | 42 | 101/63 | 50.6 | 0 | - |
| B1 | 105 | 91 | 99/49 | 17.6 | 2 | 7 |
| C1 | 123 | 60 | 116/56 | 13.7 | 2 | 6 |
| A2 | 162 | 51 | 158/49 | 14.7 | 0 | - |
| B2 | 142 | 54 | 131/58 | 32.5 | 3 | 3 |
| C2 | 142 | 59 | 135/71 | 18.7 | 1 | 3 |

All runs grade A. Recordings: session scratchpad `5grun/rec-*.jsonl.gz` (not kept).

- The cell changes more than the option: the A runs had no deep episode (no stall) today, so the throughput per run does not show the effect.
- Time from a deep episode (< 35 % of the rate) back to 60 % of the rate before it: with the option 0.2-0.5 s (one 2.9 s, 8 episodes); without it on 2026-10-18: 3.3-16.2 s (median ~5 s, 8 episodes).
- 19 recoveries: 15 kept the rate for 1.5 s or more, 4 got a new decrease in 1.5 s (21 %).
- Delay: no increase seen (DL p95 in the DL phase 13.7-32.5 ms with the option, 14.7-50.6 ms without).
- Upload (separate problem): in the DL phase, the cross delay cuts the UL rate (A1: 94.5 -> 20 Mbit/s with UL idle), and the UL increase is slow because the UL delta under load (15-20 ms, grant delay) is above avg_owd_delta_max_adjust_up_thr_ms (10): the increase factor is then 1.0. Shaped UL 42-91 vs unshaped 99.

## 5G day pass (2026-10-21)

Setup: cav1 on the 5G modem (Telekom mobile), host on Wi-Fi. The VM makes the load (8 curl workers for each direction) and shapes with CAKE on eth1/ifb4eth1 (`scripts/openwrt-vm-session.sh`). Calibration of 2026-10-18: DL 34/130/245, UL 19/94.5/110 Mbit/s, thresholds 36 ms, `icmp-timestamp`, `stall_recovery_ms` 500. Original: cake-autorate ac75f49, `fping-ts`, the same rates, thresholds and reflectors. Saturation check: DL 169/234/248 Mbit/s with 4/8/16 workers, UL 94-96. Unshaped: DL 470-790 ms excess RTT (p50), 140-240 Mbit/s.

Raw data: branch `recordings`, directory `2026-10-21-5g-daypass` (README with all policies and configs).

| Session | Policies | Modes | Block | Rounds |
|---|---|---|---|---|
| dp2 | default, lcd, lcd+UL up thr 20, original | dl ul both | 60 s | 2 |
| dp3 | default, up thr 20, up thr 30 (both directions), original | dl ul both | 120 s | 2 |
| dp4 | default, rod, rdr, rod+lcd, original | dl both | 120 s | 2 |
| dp5 | default, rod, rdr | dl both | 120 s | 8 |
| dp6 | default, rod, inc, inc+rod, original (build with `increase_to_delay_thr`) | dl | 120 s | 12 |
| dp7 | default, rod, inc+rod, rod+lcd, original | both | 120 s | 12 |
| dp8 | default, rod, inc+rod, original (max 245, see below) | dl ul both | 120 s | 5 |
| dp11, dp11b | default, ping interval 200 ms, base rate 180 Mbit/s | dl both | 120 s | 3 + 3 |
| dp12 | default (max 245), learned max (prototype), max 500, inc+rod+learned max | dl both | 120 s | 3 |
| dp10 | default, rod, inc+rod, original | dl both | 120 s | 5 |
| dp9 | default, stall recovery off, rod, rod+stall recovery off | dl both | 120 s | 5 |
| dp13 | default (max 245), `auto_max_rate` (main build), max 500 | dl both | 120 s | 2 |

rod = `refractory_on_decrease`, rdr = `repeat_decrease_on_rise`, lcd = `learn_cross_delay`, inc = `increase_to_delay_thr` (new option, branch `feat/increase-to-threshold`). All sessions have an unshaped block in each round.

Method: each round has one block of each policy, in a fixed order. The cell changes much between blocks (the same policy: 40-79 % of unshaped DL). Thus the comparison uses pairs (variant minus default in the same round and mode, absolute Mbit/s and ms) and a t-test (95 %, * = significant). The % of the unshaped block of the round is not a good reference: it adds the noise of one more block.

### Download only (pairs from dp2-dp6)

| Variant - default | n | DL Mbit/s | p50 ms | p99 ms |
|---|---|---|---|---|
| rod | 22 | +12 ± 11 * | +0.9 ± 0.8 * | -18 ± 30 |
| inc | 12 | +16 ± 21 | +1.7 ± 1.2 * | +16 ± 37 |
| inc + rod | 12 | +22 ± 22 * | +3.9 ± 1.9 * | +33 ± 63 |
| original | 18 | +25 ± 16 * | +6.9 ± 1.6 * | +24 ± 41 |
| rdr | 10 | -7 ± 18 | -0.8 ± 1.1 | +5 ± 33 |
| up20, up30, lcd, lcd+up20 | 2 each | -18 to +11 (not significant) | | |

Means: default DL 55 % of unshaped, excess RTT 26/98/186 ms (p50/p95/p99, n=26); original 66 %, 33/117/213 ms (n=18).

- The original gets more download, but with more delay. This is a trade-off, not a fault of the default.
- rod gives +12 Mbit/s (about +11 %) for +1 ms p50 and no p99 cost. Cause: the default decreases at each delayed ping. In a radio stall, many pings in a row are delayed and the achieved rate is low, so the rate falls to the minimum. In dp6, the default made 50-95 cuts per 120 s block (2-17 to the minimum rate), rod 22-45 (0-3 to the minimum).
- inc: the default increase factor is 0 at a delay of `avg_owd_delta_max_adjust_up_thr_ms` (10 ms). On this cell the DL delay under load is 14-17 ms also without a queue, so the rate increases only slowly. inc uses the factor of the original (1 up to 10 ms, 0 at the delay threshold). Alone, it makes about 2 times more cuts (150-190 per block) and the gain is not significant. With rod, it gets most of the gain of the original for half of its delay cost.
- The trade-off is almost linear: default -> rod -> inc+rod -> original: +0 / +12 / +22 / +25-28 Mbit/s for +0 / +1 / +4 / +7 ms p50.

### Upload only (dp2, dp3, 4 blocks)

| Policy | p50/p95/p99 ms | UL |
|---|---|---|
| unshaped | 94/118/134 | 92 Mbit/s (100 %) |
| default | 22/49/62 | 86 Mbit/s (94 %) |
| original | 30/68/86 | 83 Mbit/s (91 %) |

The default is better than the original in delay (p50 -7.6 ms *, p99 -24 ms *) and equal in throughput. up20, up30, lcd and lcd+up20 give no gain.

### Both directions (dp2-dp5; dp7 adds 12 rounds)

| Variant - default | n | DL Mbit/s | p50 ms | p99 ms |
|---|---|---|---|---|
| rod | 10 | +14 ± 18 | +2.8 ± 1.1 * | +4 ± 27 |
| rdr | 10 | +15 ± 29 | +1.4 ± 1.1 * | +39 ± 153 |
| original | 6 | +25 ± 23 * | +16 ± 3 * | +36 ± 49 |

Means: default DL 61 % and UL 74 Mbit/s (unshaped: 44 Mbit/s UL, the DL load starves it), 30/81/153 ms (n=14); original 80 %, 73 Mbit/s, 47/121/179 ms (n=6). lcd gave +8 Mbit/s UL in dp2/dp3 (2 blocks, not significant).

dp7 (both directions, 12 rounds, 2026-10-21 22:40 to 2026-10-22 01:30):

| Variant - default | n | DL Mbit/s | p50 ms | p99 ms |
|---|---|---|---|---|
| inc + rod | 12 | +16 ± 7 * | +2.3 ± 2.0 * | +6 ± 23 |
| rod + lcd | 12 | +10 ± 6 * | +1.0 ± 1.7 | -12 ± 16 |
| rod | 12 | +1 ± 8 | +0.5 ± 0.9 | +6 ± 17 |
| original | 12 | +20 ± 14 * | +2.3 ± 2.7 | +12 ± 21 |

Means (p50/p95/p99 ms, DL % of unshaped, UL Mbit/s): default 26/55/88, 63 %, 106; rod 27/56/94, 63 %, 105; inc+rod 29/59/94, 69 %, 107; rod+lcd 27/54/77, 67 %, 107; original 29/65/100, 71 %, 106; unshaped 462/679/787, 100 %, 61.

- With load in the two directions, rod alone gives no download gain. inc+rod gets most of the gain of the original with the same p50 and a lower p99.
- rod+lcd has the lowest p99 and p95 of all policies, and +10 Mbit/s DL.
- All shaped policies get about 106 Mbit/s UL, 1.75 times the unshaped UL (the unshaped DL queue delays the UL ACKs).
- In dp7 (night), the delay cost of the original is much lower than in dp2-dp5 (+2 ms p50 against +16 ms).

### Sessions dp8 to dp13 (2026-10-22, 01:30 to 12:10)

From dp9 on, the configs have maximum rates 500/160 Mbit/s (`5g.yaml`); `5g-max245.yaml` keeps the calibrated maximum 245/110.

dp8 (night, 01:30 to 05:00): at night the cell gave more than 245 Mbit/s. All shaped policies stayed at the maximum rate most of the time, thus all differences are 0 ± 3 Mbit/s. The calibrated maximum limits the throughput at night. dp8 is not in the pooled values below. (The VM root file system became full at block 77; the session stopped there.)

dp12 (night, 3 rounds): fixed maximum 500 against 245: DL +55 ± 114 (dl), +69 ± 104 (both) Mbit/s, p50 +3 / +7 ms. The learned-max prototype (maximum increases when the shaper is at the maximum and the link uses it): +22 / +15 Mbit/s. The prototype reset the hold at each delayed ping and thus stayed above 245 for only 5-30 % of the time (max 500: 60-90 %). n = 3, no value is significant, but the direction is the same in all rounds with a free cell. After a fix of the hold (the hold continues through delayed pings), the simulator (night capacity 380-450 Mbit/s with radio stalls, 4 seeds) gives: default 49.3 %, learned max 53.6 %, max 500 58.1 %, inc+rod 52.4 %, inc+rod+learned max 64.1 %, inc+rod+max 500 66.1 % of capacity. This is `auto_max_rate` (PR #109, opt-in).

dp11 and dp11b (6 rounds): ping interval 200 ms (default 300) and base rate 180 Mbit/s (default 130) give no gain. Ping interval 200 ms in both: p99 +11 ± 9 ms *. Both are not candidates.

dp10 (morning, 07:30 to 09:30, 5 rounds):

| Variant - default | DL only: DL Mbit/s, p50, p99 | Both: DL Mbit/s, p50, p99 |
|---|---|---|
| rod | +14 ± 18, +0.4, -22 | +1 ± 30, +2.1, +14 |
| inc + rod | +20 ± 20, +4.4 *, +31 * | +5 ± 32, +16.5 *, +37 * |
| original | +29 ± 34, +7.4 *, +47 * | +9 ± 20, +16.9 *, +115 |

On the busy morning cell (unshaped p50 450-510 ms, at night about 350 ms), inc+rod costs as much delay as the original with both directions loaded. At night (dp7) the cost was +2 ms.

dp9 (09:30 to 11:30, 5 rounds), `stall_recovery_ms` 0 (srm0):

| Variant - default | DL only: DL Mbit/s, p50, p99 | Both: DL Mbit/s, p50, p99 |
|---|---|---|
| srm0 | -25 ± 5 *, -2.7 *, -2 | -14 ± 31, -4.0 *, -15 |
| rod | -11 ± 31, +0.9, +16 | +4 ± 22, +1.4 *, +15 |
| rod + srm0 | +6 ± 9, +0.4, -29 | -21 ± 22, -2.0 *, +48 |

Stall recovery gives +25 Mbit/s DL for about 3 ms p50. Keep it on by default.

dp13 (11:30 to 12:10, main build c822120, 2 rounds): the cell was weak at noon (unshaped 172-224 Mbit/s). `auto_max_rate` learned only in one block (both, round 2): the maximum went from 245 to 269.5 Mbit/s (one step) and the shaper was above 245 in 24 samples. In the other blocks the shaper was below the maximum, and `auto_max_rate` did not change it. DL against default: +34 (dl), +27 (both) Mbit/s, n = 2, mostly cell noise; p50 +1 to +3 ms. This confirms the live function of the merged build (config read, learning step, no cost on a weak cell), but not the gain.

Pooled pairs dp2-dp10 without dp8:

| Variant - default | Mode | n | DL Mbit/s | p50 ms | p99 ms |
|---|---|---|---|---|---|
| rod | dl | 32 | +9 ± 9 | +0.8 ± 0.6 * | -13 ± 25 |
| rod | both | 32 | +5 ± 7 | +1.6 ± 0.6 * | +8 ± 11 |
| rod + lcd | both | 14 | +8 ± 5 * | +1.7 ± 1.8 | -6 ± 15 |
| inc + rod | dl | 17 | +21 ± 15 * | +4.1 ± 1.4 * | +33 ± 42 |
| inc + rod | both | 17 | +13 ± 8 * | +6.4 ± 3.7 * | +15 ± 18 |
| original | dl | 23 | +26 ± 13 * | +7.0 ± 1.3 * | +29 ± 32 |
| original | both | 23 | +19 ± 9 * | +9.1 ± 3.5 * | +41 ± 29 * |

With more sessions, the gain of rod is smaller than in dp2-dp6 and not significant. Its cost is also small (about 1 ms p50, p99 equal or lower).

### Conclusions

- Delay: the default has the lowest p50 in all modes. Against the original: DL only -7 ms, UL only -8 ms, both -16 ms (all significant).
- Throughput: the original gets +25 Mbit/s more download (about +20 %) in DL only and in both. The upload is equal.
- The modes latency / balanced / bandwidth can be option sets (default / rod / inc+rod) instead of threshold factors: on this cell, higher thresholds (up20, up30; 36 -> 54 ms on 2026-10-18) gave no gain.
- rdr: no gain, remove it from the candidates for 5G.
- The A/B of rod on 2026-10-18 (LuCI test, 2 runs each, "mixed") was too small: with 22 pairs, rod is significant.
- Maximum rate: the calibrated maximum (day) limits the throughput at night (dp8, dp12). `auto_max_rate` (opt-in, PR #109) increases the maximum when the link uses it; a high fixed maximum gave more in dp12. Test on 5G and Starlink planned.
- Stall recovery: keep `stall_recovery_ms` 500 (dp9: -25 Mbit/s DL without it).
- The delay cost of inc+rod and of the original depends on the load of the cell: small at night (dp7), large in the morning (dp10). Thus: balanced = rod (or rod+lcd), bandwidth = inc+rod with `auto_max_rate`.
- Ping interval 200 ms and base rate 180 Mbit/s: no gain.
