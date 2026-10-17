# Development

This document tells how to test a change to the controller. Each change must show its effect on the bufferbloat **and** on the throughput.

## Tests and tools

- **Unit tests and replay:** `go test ./...`. The core of the controller does not read the clock and starts no goroutines. Thus, the same events always give the same decisions, in the tests and in a replay of a recording.
- **Link simulator:** a model of a link with a bottleneck queue, TCP flows and ping replies. Scenarios include a steady link, a capacity drop, 5G and Starlink. A scorecard gives the delay and the throughput for each scenario, and a baseline check finds regressions. See [Simulator](#simulator).
- **CI emulation:** [`scripts/ci-emulation.sh`](../scripts/ci-emulation.sh) tests the real binary, CAKE and netlink in network namespaces, without an internet connection. Each CI run also runs the original cake-autorate on the same link for comparison (option `-U`).
- **Link emulation:** [`scripts/emulate-link.sh`](../scripts/emulate-link.sh) emulates a variable link (for example 5G) from a capacity trace on a fast connection, with real TCP traffic. See [emulation.md](emulation.md).
- **Test sessions on a real link:** [`scripts/record-session.sh`](../scripts/record-session.sh) measures different policies in alternating blocks. The policies are no shaper, a fixed shaper, cake-autorate-go, other configs or binaries (`-A`), and the original cake-autorate (`-U`). [`scripts/summarize-session.sh`](../scripts/summarize-session.sh) shows the delay and the throughput of each block. See [recording.md](recording.md).

## Structure

| Part | File | Function |
|---|---|---|
| Controller | `core.go` | `Core` makes all decisions. It does no I/O, starts no goroutines and does not read the clock. Thus, the same events always give the same decisions. |
| Side effects | `controller.go`, `shaper.go`, `pinger.go`, `tsping.go`, `monitor.go` | The pingers and the throughput monitor send events to `Core`. `Core` sends its decisions through the `Actuator` interface to the shaper (rates) and to the pingers (start, stop). |
| Recording | `recorder.go` | `RecordingCore` writes each input event, each shaper rate and each state change to a file (`--record`). It does not record the start and stop of the pingers or the shaper invalidation. See [recording.md](recording.md). |
| Simulator | `sim_test.go`, `sim_scenarios_test.go` | A fluid model of a link with a bottleneck queue, TCP flows and ping replies. It runs `Core` with simulated time. |

## Tests

Run all tests:

```bash
go test ./...
```

CI runs `go test -race ./...` on each pull request.

Use `-short` to skip the simulator scorecard and the baseline check. The other simulator tests (for example the known-issue tests) also run with `-short`. They are fast.

### Unit tests

`core_test.go` gives events to `Core` and examines the decisions with a fake `Actuator`. Use this level for a change to one rule of the controller.

### Simulator

The simulator runs each scenario for 120 s with simulated time. One run takes some milliseconds.

Scenarios (`simScenarios` in `sim_scenarios_test.go`):

| Scenario | Link |
|---|---|
| `steady-dl` | Constant capacity, load on the download |
| `capacity-drop` | The download capacity decreases from 200 to 40 Mbit/s and then increases again |
| `bidir-saturated` | Load in the two directions |
| `5g-variable` | Capacity changes as a random walk with deep fades, large buffers |
| `starlink-like` | The capacity and the RTT floor change every 15 s, large jitter |
| `latency-spike` | The RTT floor increases by 120 ms for 300 ms. This delay is not a queue |
| `5g-recorded-a`, `5g-recorded-b` | Capacity from two recorded 5G sessions (`testdata/traces/`, each 200 ms sample), with the upload delays of the same link (as `5g-coupled`). `TestSimCalibration_5GRecorded` compares the result with the 5G link |
| `5g-coupled` | 5G radio delays on the upload that no shaper can remove: download activity adds a median of 21 ms, upload activity adds 14 ms (p50) and 24 ms (p95). Load in the two directions |

Policies:

| Policy | Shaper |
|---|---|
| `unshaped` | No shaper |
| `fixed-base` | The shaper stays at the base rate |
| `autorate` | `Core` with ICMP echo (RTT/2 for each direction) |
| `autorate-owd` | `Core` with ICMP timestamps (delay of each direction) |

Show the scorecard for all scenarios and policies:

```bash
go test -run TestSimScorecard -v
```

The columns:

| Column | Meaning |
|---|---|
| `p50 ms`, `p95 ms`, `p99 ms` | Queue delay that a sparse flow sees during the load |
| `bloat s` | Time with a queue delay of more than 30 ms |
| `dl %`, `ul %` | Delivered data, as a percentage of the capacity, during the load |
| `sets` | Number of shaper rate changes |

### Baseline check

`testdata/sim_baseline.json` contains the scores of `autorate` and `autorate-owd` for each scenario (key `policy/scenario`). Each score is the mean of 4 seed variants. One seed can hide a change that occurs only with some traces. The two policies use different rules in `Core`. `TestSimBaseline` fails if a score is worse than the baseline by more than the tolerance. The tolerances are: p95 and p99 10 % or 5 ms, `bloat s` 10 % or 1 s (each the larger value), `dl %` and `ul %` 3 percentage points. It also shows a table with the changes.

If a change makes the scores better, write the new baseline and commit it with the change:

```bash
go test -run TestSimBaseline -update
```

### Known-issue tests

A `TestKnownIssueN_...` test examines a behavior that we want but that the controller does not have yet. While the issue occurs, the test passes and logs the issue. When a change fixes the issue, the test fails. Then change it into a usual test that requires the correct behavior.

### Replay

`TestRecording_ReplayGivesSameDecisions` records a simulator run and gives the recorded events to a new `Core`. The shaper rates and the state changes must be the same. The test does not compare the start and stop of the pingers or the shaper invalidation.

The test always uses its own simulator recording. The helper `replayRecording` in `recorder_test.go` can also replay a recording from a real link, but it reads only uncompressed JSONL. Thus, first decompress the recording (`gunzip -k NN-MODE-POLICY.jsonl.gz`), then call the helper with the path in a temporary test. With a changed `Core`, the replayed decisions are different from the recorded decisions where the change has an effect.

### Emulation test

The simulator does not test the netlink code, the pingers and the CAKE qdisc. `scripts/ci-emulation.sh` tests the real binary on an emulated link in three network namespaces (router, modem, server). It does not need an internet connection. The modem has a large queue and a rate that changes. The test has two phases with load in the two directions (iperf3): CAKE without a rate limit, then cake-autorate-go. It fails if the unshaped phase has no bufferbloat, or if autorate does not keep the delay and the throughput in the limits. CI runs it in the job `emulation`.

With `-U DIR`, the test has a third phase: the original [cake-autorate](https://github.com/lynxthecat/cake-autorate) (a clone in DIR) controls CAKE with the same rates and reflectors. This phase has no limits. The result shows its delay and throughput next to the values of cake-autorate-go. CI uses it with a fixed commit of cake-autorate. It needs fping.

```sh
go build -o cake-autorate-go .
sudo ./scripts/ci-emulation.sh        # result in recordings/<date>-ci-emu/result.txt
sudo ./scripts/ci-emulation.sh -U ../cake-autorate   # with the upstream phase
sudo ./scripts/ci-emulation.sh -k     # remove the setup after a crash
```

## Tests on a real link

The simulator has limits (fluid model, simple jitter, no random packet loss on the radio link; it models only the drops of the queues). Thus, examine an important change also on a real link or in an emulation:

- [recording.md](recording.md): sessions on a real link with `scripts/record-session.sh`. The option `-A NAME=CONFIG[,BINARY]` adds a variant. The blocks of all variants alternate. Thus, the blocks of the variants are near in time. A slow change of the link has a similar effect on each variant (A/B test). The link conditions are not the same in each block.
- [emulation.md](emulation.md): a variable link (for example 5G) on a fast connection, with a repeatable capacity trace.

On a mobile link, the capacity changes much during a session. Compare the variants of the same session, and use two or more rounds.
