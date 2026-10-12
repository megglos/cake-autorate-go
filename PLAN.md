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

## Session results

All results of the test sessions are in [RESULTS.md](RESULTS.md): fiber dry run, emulation on fiber, 5G sessions 1–3, A/B sessions 1–8 on 5G, fiber emulation comparison with upstream.

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

## Coupling fix: rebaseline_at_min_rate_ms (branch feat/ul-dl-coupling, worktree ../cake-autorate-go-coupling, bdc478e on 79935db, not pushed)

- If a direction stays at its minimum rate with bufferbloat for the hold time, Core adds the delay to the OWD baseline of that direction. Off by default.
- Simulator: new coupling model (`ulCouplingMs` = 40 ms, full value at 20 % DL use, as in the 5G data) and scenarios 5g-coupled, 5g-coupled-dl. 5g-coupled: ul 7 → 85 %, dl 99 → 93 %, p95 69 → 77 ms. Other scenarios do not change.
- Hold time is necessary: without it, a UL capacity a little above the minimum gives p95 57 → 514 ms (the queue at the minimum rate empties slowly). With 2 s: no change.
- Rejected alternative: give the UL delay to DL when UL is at the minimum. Simulator: p95 88 → 20 ms, but dl 98 → 34 % and ul stays at 7 %. The 5G data shows that a DL decrease does not remove the delay. Thus, do not do this.
- autorate-owd 5g-variable p99 414 ms (p95 47 ms): one event. UL load starts at t=30 s during a UL fade to 5 Mbit/s, with the shaper at the base rate (20 Mbit/s). The 800 ms buffer fills before the first cut. Low priority.

## Simulator sweep (2026-10-11, base: OWD + F1 + F2 + rebaseline 2 s)

One parameter at a time, 7 scenarios. Mean p95 / mean utilization: base 52.9 ms / 94.0 %.
- The delay threshold is the largest lever. It sets the operating point of the queue. thr 15 ms: 33.5 / 93.7. thr 20: 37.3 / 94.1. thr 40: 62.2 / 91.3.
- Max decrease 0.6 (default 0.75): 45.5 / 94.3. Refractory 150 ms: 47.7 / 94.1. Other parameters (detection 2 or 4 of 6, high_load_thr, max increase, ping interval): small effect or worse.
- Combinations: thr 15 + down 0.6: 28.4 / 92.0. thr 20 + down 0.6: 34.1 / 93.9. With 8 ms jitter: 25.6 / 90.0 and 31.4 / 91.9.
- Without F1, thr 15: 26.5 / 85.2. F1 keeps the throughput at low thresholds.
- Risk: the simulator jitter model is simple. On the real link, a low threshold can give false bufferbloat. Test on 5G.
- Configs: `recordings/5g-owd-t20.yaml`, `recordings/5g-owd-t15.yaml` (f1f2 + rb + thr + `shaper_rate_max_adjust_down_bufferbloat: 0.6`).

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
4. Fiber emulation comparison with upstream done 2026-10-12 ([RESULTS.md](RESULTS.md#fiber-emulation-comparison-with-upstream-2026-10-12)). Ours: much lower delay (p95 −35…−45 %), DL 87–88 % vs upstream 93–95 %, UL equal. Emulation bug (GRO + tbf burst) fixed on feat/upstream-compare; pinger leak fixed (PR #35). `repeat_decrease_on_rise` (PR #36, merged, default off by user decision) closes the DL gap with `icmp-timestamp`. Remaining: p99 at UL capacity cliffs (both directions); check on the next 5G day pass with the option.
5. Step 8 without 5G: upstream comparison on the CI emulation (repeatable, no data cost): add `-U` to `ci-emulation.sh` or run `emulate-link.sh -- -U`. darkmoon: build for x86 in the OpenWrt VM (step 9 setup), then the same emulation; resource use with `benchmark.sh` on the GL-MT6000.
6. 6b online part (`calibrate --iface`): run the session steps itself (lower priority; the offline flow works).
7. Step 9: LuCI/OpenWrt package (`PLAN-LUCI.md`). Needs the QEMU VM (user: `modprobe kvm_amd`, `pacman -S qemu-base`).
8. Step 10: Starlink from 2026-10-28 (branch `feat/starlink-reconfig`).
9. Known issues are GitHub issues #30 (F1), #31 (F2), #32 (F4), #33 (F5); PR #34 links them in the CI summary. `refractory_on_decrease` as default rejected 2026-10-11: sim baseline 5g-variable OWD p95 58 → 115 ms (more throughput, much more delay). Idea for F1/F2 without that cost: block repeated cuts only during a short spike (e.g. no second cut while the delta decreases).
10. Housekeeping: remove old worktrees; root-owned `recordings/20261011-1610-enp7s0` (user: sudo rm).

5G tests still useful (next 5G day):
- Upstream vs ours on 5G (step 8): `-U fts=recordings/upstream-5g-fping-ts.sh` vs `5g-s7-learn-u20.yaml` (our best 5G profile), modes `ul both`, `-W 30`, 2 rounds = 12 blocks ≈ 40 min. Gives the main result of the comparison on the real link.
- Not needed: a re-test of the latency target after the #27 changes (they affect the reference at moderate load and RTT mode; session 8 used OWD at full load, the main path did not change).

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
  - [x] Dry run on the fiber connection of the PC (2026-10-10). It found the threshold bug (PR #8). See [RESULTS.md](RESULTS.md#fiber-dry-run-2026-10-10).
  - [x] Real 5G sessions (2026-10-10/11): 3 baseline sessions, 8 A/B sessions. Results: [RESULTS.md](RESULTS.md).
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
- [x] **6. Tuning.** Done for 5G 2026-10-11: sessions 3–8 ([RESULTS.md](RESULTS.md#ab-session-3-2026-10-11-0808)), sweeps on 8 seeds. Defaults stay 30/30, learn_cross_delay off; 5G profile documented (#28). Profiles for Starlink: step 10.
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
  - Fiber emulation vs upstream done 2026-10-12: [RESULTS.md](RESULTS.md#fiber-emulation-comparison-with-upstream-2026-10-12). Open: 5G link, darkmoon, resource use.
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
