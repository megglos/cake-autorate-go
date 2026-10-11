# Development

This document tells how to test a change to the controller. Each change must show its effect on the bufferbloat **and** on the throughput.

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

`testdata/sim_baseline.json` contains the scores of `autorate` and `autorate-owd` for each scenario (key `policy/scenario`). Each score is the mean of 4 seed variants. One seed can hide a change that occurs only with some traces. The two policies use different rules in `Core`. `TestSimBaseline` fails if a score is worse than the baseline by more than the tolerance (p95 and p99: 10 % or 5 ms, the larger value; `bloat s`: 10 % or 1 s, the larger value; `dl %` and `ul %`: 3 percentage points). It also shows a table with the changes.

If a change makes the scores better, write the new baseline and commit it with the change:

```bash
go test -run TestSimBaseline -update
```

### Known-issue tests

A `TestKnownIssueN_...` test examines a behavior that we want but that the controller does not have yet. While the issue occurs, the test passes and logs the issue. When a change fixes the issue, the test fails. Then change it into a usual test that requires the correct behavior.

### Replay

`TestRecording_ReplayGivesSameDecisions` records a simulator run and gives the recorded events to a new `Core`. The shaper rates and the state changes must be the same. The test does not compare the start and stop of the pingers or the shaper invalidation.

The test always uses its own simulator recording. The helper `replayRecording` in `recorder_test.go` can also replay a recording from a real link, but it reads only uncompressed JSONL. Thus, first decompress the recording (`gunzip -k NN-MODE-POLICY.jsonl.gz`), then call the helper with the path in a temporary test. With a changed `Core`, the replayed decisions are different from the recorded decisions where the change has an effect.

## Tests on a real link

The simulator has limits (fluid model, simple jitter, no random packet loss on the radio link; it models only the drops of the queues). Thus, examine an important change also on a real link or in an emulation:

- [recording.md](recording.md): sessions on a real link with `scripts/record-session.sh`. The option `-A NAME=CONFIG[,BINARY]` adds a variant. The blocks of all variants alternate. Thus, the blocks of the variants are near in time, and a slow change of the link has a similar effect on each variant (A/B test). The link conditions are not the same in each block.
- [emulation.md](emulation.md): a variable link (for example 5G) on a fast connection, with a repeatable capacity trace.

On a mobile link, the capacity changes much during a session. Compare the variants of the same session, and use two or more rounds.
