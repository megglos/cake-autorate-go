# Development

This document tells how to test a change to the controller. Each change must show its effect on the bufferbloat **and** on the throughput.

## Structure

| Part | File | Function |
|---|---|---|
| Controller | `core.go` | `Core` makes all decisions. It does no I/O, starts no goroutines and does not read the clock. Thus, the same events always give the same decisions. |
| Side effects | `controller.go`, `shaper.go`, `pinger.go`, `tsping.go`, `monitor.go` | Pingers, throughput monitor and shaper. They send events to `Core`, and `Core` sends its decisions through the `Actuator` interface. |
| Recording | `recorder.go` | `RecordingCore` writes each event and each decision to a file (`--record`). See [recording.md](recording.md). |
| Simulator | `sim_test.go`, `sim_scenarios_test.go` | A fluid model of a link with a bottleneck queue, TCP flows and ping replies. It runs `Core` with simulated time. |

## Tests

Run all tests:

```bash
go test ./...
```

CI runs `go test -race ./...` on each pull request.

Use `-short` to skip the simulator tests (scorecard and baseline).

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
| `latency-spike` | A 300 ms delay spike that is not a queue |

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

`testdata/sim_baseline.json` contains the scores of `autorate` for each scenario. `TestSimBaseline` fails if a score is worse than the baseline by more than the tolerance (latency: 10 % or 5 ms; throughput: 3 percentage points). It also shows a table with the changes.

If a change makes the scores better, write the new baseline and commit it with the change:

```bash
go test -run TestSimBaseline -update
```

### Known-issue tests

A `TestKnownIssueN_...` test examines a behavior that we want but that the controller does not have yet. While the issue occurs, the test passes and logs the issue. When a change fixes the issue, the test fails. Then change it into a usual test that requires the correct behavior.

### Replay

`TestRecording_ReplayGivesSameDecisions` records a simulator run and gives the recorded events to a new `Core`. The decisions must be the same. Use the same procedure for a recording from a real link: a change to `Core` then shows its effect on real events.

## Tests on a real link

The simulator has limits (fluid model, simple jitter, no packet loss). Thus, examine an important change also on a real link or in an emulation:

- [recording.md](recording.md): sessions on a real link with `scripts/record-session.sh`. The option `-A NAME=CONFIG[,BINARY]` adds a variant. The blocks of all variants alternate, thus each variant gets the same link conditions (A/B test).
- [emulation.md](emulation.md): a variable link (for example 5G) on a fast connection, with a repeatable capacity trace.

On a mobile link, the capacity changes much during a session. Compare the variants of the same session, and use two or more rounds.
