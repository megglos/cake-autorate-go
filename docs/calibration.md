# Calibration

This document tells how to measure a link and get a config that fits it. Use it at a new place: another 5G cell, another provider, another modem. The procedure has two steps:

1. Record a session on the link with `scripts/record-session.sh` (approximately 40 minutes).
2. Analyze the session with `cake-autorate-go calibrate`. It writes a proposed config.

The result is a proposal, not a final config. Read the comments in the file and compare them with the explanations below.

## Why calibrate

The controller needs values that depend on the link:

| Value | Wrong value: effect |
|---|---|
| `max_rate_kbps` | Too low: the shaper limits the throughput. Too high: no problem, but the rate takes longer to go down. |
| `min_rate_kbps` | Too high: when the capacity decreases below it, the shaper cannot prevent bufferbloat. |
| `base_rate_kbps` | The start rate and the rate without load. Too high: bufferbloat at the start of a load. |
| Delay thresholds | Too low: the jitter of the link alone shows bufferbloat, and the rate goes down without a queue. Too high: more delay under load. |
| Upload threshold on 5G | Below the 5G upload grant delay: the upload rate collapses (on a 5G link: 15 ms gave 30 to 43 % of the capacity, 25 ms gave 66 to 68 %). |
| `learn_cross_delay` | On 5G, download activity adds approximately 20 to 30 ms to the upload delay. Without the option, this delay keeps the upload rate low when the download is busy. |
| `reflectors`, `ping_method` | Reflectors with a high jitter, or reflectors that do not reply to ICMP timestamps, make the delay measurement worse. |

These values change with the place and with the time of day. Thus, one config for all links is not good.

## Requirements

- A Linux PC that connects directly to the modem (Ethernet or USB), not through a router. The script sets up CAKE on the PC. See [recording.md](recording.md), "Record from a PC without a router".
- `curl`, `jq`, `tc` (iproute2), the kernel modules `sch_cake`, `ifb`, `act_mirred`.
- A config with **one** link, `ping_method: icmp-timestamp`, the download interface `ifb4<interface>` and the upload interface `<interface>`. The rates in it are only start values; the calibration measures them. Example:

  ```yaml
  links:
    - name: 5g
      download:
        interface: ifb4enp7s0
        adjust: true
        min_rate_kbps: 30000
        base_rate_kbps: 100000
        max_rate_kbps: 400000
      upload:
        interface: enp7s0
        adjust: true
        min_rate_kbps: 4000
        base_rate_kbps: 15000
        max_rate_kbps: 50000
      ping_interface: enp7s0
  ping_method: icmp-timestamp
  learn_cross_delay: true
  ```

- **Data volume.** The session fills the link many times. On a 5G link with 150 to 380 Mbit/s download, a session of 27 blocks used 68 GB. A session of 12 blocks with load in the two directions used 38 GB. The session below has 12 blocks: expect approximately 30 to 40 GB on such a link. Use an unlimited data plan. For a shorter session, use `-b 60` (60 s of load in each block).

## Step 1: record a session

```bash
go build .
sudo ./scripts/record-session.sh -i enp7s0 -c my-5g.yaml -r 2 -W 30
```

- `-i`: the interface of the modem.
- `-c`: the config (see above).
- `-r 2`: two rounds. Two rounds show if the link changed during the session.
- `-W 30`: in the blocks with load in the two directions, 30 s of download load first. Then the controller can learn the upload delay from download activity before the upload starts.

The script runs approximately 40 minutes (12 blocks of approximately 3 minutes, and the saturation check). Do not use the network during the session. Do not move the modem.

**CAKE before the session:** do not set it up yourself, and stop other shapers on this interface (SQM, a running cake-autorate or cake-autorate-go). The script sets up CAKE itself and controls it in each block: CAKE without a bandwidth limit in the `unshaped` blocks, CAKE controlled by cake-autorate-go in the `autorate` blocks. It replaces an existing root qdisc on the interface and removes CAKE at the end. "Unshaped" thus does not mean "without CAKE": CAKE is there, but it does not limit the rate. Then the buffers of the modem and the network fill, as without CAKE.

### What the script does

**1. Setup.** The script adds CAKE on the modem interface (upload) and on an IFB interface for the download (`scripts/cake-pc.sh`). It writes `session.txt` with the settings and system data.

**2. Saturation check (approximately 2 minutes).** CAKE has no bandwidth limit. The script applies a load in one direction for 20 s, with 4, 8 and 16 parallel connections, first download, then upload. It measures the throughput in the last 10 s. This finds:

- the capacity of the link (the highest rate);
- the number of connections that fills the link. If 16 connections give more than 10 % more than 8, the link is possibly not full, and the script writes a warning.

The download connections get large files from speed test servers in Europe. The upload connections send data to the Cloudflare speed test. The servers must be faster than the link; else the "capacity" is the server limit.

**3. Blocks.** Each round has one block for each load mode and policy:

| Load mode | Load |
|---|---|
| `dl` | download only |
| `ul` | upload only |
| `both` | download and upload at the same time (after the download warm-up of `-W`) |

| Policy | Shaper |
|---|---|
| `unshaped` | CAKE without a bandwidth limit: the link at its limit. Shows the capacity and the bufferbloat without a shaper. |
| `autorate` | cake-autorate-go controls the CAKE bandwidth with your config. |

The blocks alternate (dl-unshaped, dl-autorate, ul-unshaped, …). Thus, a slow change of the link has a similar effect on each policy.

Each block:

1. Start cake-autorate-go with `--record` (also in the unshaped blocks, with `--no-adjust`: it measures, but does not change the shaper).
2. **Idle time** (15 s): no load. The pings show the delay floor and the jitter of each reflector without load.
3. **Load time** (150 s): the load of the mode. In the unshaped blocks, this is a stress test: the load fills the link and its buffers.
4. 5 s, then stop cake-autorate-go.

After each block, the script shows a summary line (see below).

**4. End.** The script removes CAKE and the IFB interface. If you stop the script with Ctrl+C, it also removes them.

### What you see during the session

After each block, the script shows a line like this one (`scripts/summarize-session.sh`):

```
block mode policy       | excess RTT p50/95/99 | dl Mbit/s (% ref) | ul Mbit/s (% ref)
1     dl   unshaped     | 860.4 1151.7 1169.5 ms  | 185 (100%)        | -
2     dl   autorate     |  43.1 125.4 189.3 ms  | 133 (72%)         | -
```

- **excess RTT**: the RTT minus the lowest RTT of the same reflector in the block. This is the delay that the load adds. In the unshaped blocks, it shows the size of the buffers of the link (here: more than 1 s). In the autorate blocks, it shows how well the shaper limits the delay.
- **Mbit/s (% ref)**: the throughput, and the percentage of the unshaped block of the same round and mode. On a mobile link, the capacity changes much during a session. Thus, compare the rounds, not only one block.

Good signs: the unshaped blocks show a high excess RTT (the buffers are full, the link is the bottleneck). The autorate blocks show a much lower excess RTT.

Bad signs:

- The unshaped blocks show a low excess RTT (for example less than 50 ms): the load did not fill the link. The capacity values are then too low. Use more connections (`-w 16`), or examine the servers.
- The script stops with "cake-autorate-go stopped": see the log `NN-MODE-POLICY.log`.
- The throughput of the rounds is very different: the link changed. Record again at another time, or use more rounds (`-r 3`).

## Step 2: analyze the session

```bash
./cake-autorate-go calibrate -session recordings/20261011-1245-enp7s0 -out my-5g-calibrated.yaml
```

The command reads `blocks.tsv`, `session.txt` and the recordings of the session. It does not use the network.

### What it measures, and why

| Measurement | Blocks | Why these blocks |
|---|---|---|
| **Download and upload capacity**: the mean rate of each second | unshaped `dl`, `ul` | Without a shaper, the load fills the link. The first 3 s of the load are not used (TCP slow start). |
| **Upload capacity with download load** | unshaped `both` | On 5G, the upload capacity is much lower when the download is busy (in the recordings: approximately 10 instead of 38 Mbit/s). |
| **Idle jitter of each reflector**: p95 of the delay above the lowest delay | idle time of each block | Without load, the delay changes only from the link and the reflector. The lowest delay is taken in each block, because the clock offset of a reflector changes during a session. |
| **Upload delay from download activity** ("cross delay") | autorate `dl` | The download shaper keeps the ACK rate low, thus the upload has no queue, and the upload delay is from the 5G scheduler. Not the unshaped `dl` blocks: there, the ACKs can fill the upload, and the delay is a queue. |
| **Upload grant delay** | autorate `ul`, only while the upload shaper rate is less than half of the capacity | On 5G, upload data waits for a grant of the scheduler. At a low rate, there is no queue: the delay is the grant delay. |
| **Reflectors with ICMP timestamps** | all blocks | Only these give one-way delays for each direction. |

### How it sets the values

| Value | Rule | Reason |
|---|---|---|
| `max_rate_kbps` | p95 of the capacity | The shaper must be able to use the highest capacity. |
| `base_rate_kbps` | p25 of the capacity; with a stable capacity: 95 % of the p25; upload on a link that shares the radio resources (a capacity that is not stable, or a median cross or grant delay of 5 ms or more): not more than the p50 with download load | A rate that the link usually has. A high start rate gives bufferbloat at the start of a load. On a stable link, p25 is the capacity: then the queue is in the network, not in CAKE. On a wired line, the upload with download load is low only without shaping: its acknowledgements wait in the queue of the download (fiber: 148 instead of 332 Mbit/s unshaped, 300 Mbit/s with shaping). |
| `min_rate_kbps` | p5 of the capacity / 2; upload: also not more than p5 / 2 with download load | The 1 s means hide short capacity decreases. The shaper must be able to go below them. |
| Delay thresholds | at least 30 ms (with a stable capacity: 10 ms), at least 2 × the median idle jitter of the reflectors in the list | Below 2 × the jitter, the jitter alone shows bufferbloat. On 5G, a lower threshold decreases the throughput. On a fiber line, a queue of 10 to 20 ms in the network did not decrease the rate with 30 ms. |
| Upload threshold | also at least the median grant delay + 10 ms; without `learn_cross_delay`, also at least the p95 of the cross delay + 10 ms; with `learn_cross_delay`, also at least max(p50, p95 − p50) of the cross delay + 10 ms | Below it, the upload rate collapses on 5G. Without `learn_cross_delay`, the cross delay stays in the upload delta: below its p95, the download pushes the upload rate down. With `learn_cross_delay`, the learned delay (approximately the p50) is not more than the threshold. |
| `learn_cross_delay` | on if the median cross delay is more than 10 ms | On 5G it is 25 to 33 ms; on fiber approximately 0. |
| `ping_method`, `reflectors` | `icmp-timestamp` if 4 or more reflectors reply (one-way delays in 90 % or more of the pings, not counting the load of the unshaped blocks, where pings time out); the `pinger_count` + 2 (at least 4) with the lowest jitter, without the ones above 2 × the p25 of the jitter + 2 ms | The program selects the active reflectors at random from the list. Thus, the list must not contain bad reflectors. The path to some reflectors can be congested: the p25 is the jitter of the good reflectors. With the median, half of the reflectors can be noisy. |

A direction has a stable capacity if the p25 of its capacity is 90 % or more of the p75, for example on fiber or cable. On 5G and Starlink, the capacity changes more. The rule does not use the p5 and the p95: a session has few 1 s means, and one dip at the start of a new connection of the load is the p5.

The rules come from 5G sessions in October 2026 (Vodafone, Berlin). For other link types (fiber, cable, Starlink), the rules are not tested yet.

### How to read the result

Each value has a comment with the measured data:

```yaml
    upload:
      interface: enp7s0
      adjust: true
      # unshaped capacity (1 s means, 442 s): p5 26.8, p25 33.8, p50 37.9, p95 42.5 Mbit/s; with download load: p5 4.8, p50 10.6 Mbit/s (...)
      min_rate_kbps: 2500
      base_rate_kbps: 10500
      max_rate_kbps: 42500
      # upload grant delay: p50 15.0, p95 58.0 ms (4382 pings); threshold >= p50 + 10 ms = 25 ms
      owd_delta_delay_thr_ms: 30
```

Examine these points:

- **Number of seconds and pings.** A short session (for example 60 s of capacity) gives unreliable values. Record more rounds.
- **Spread of the capacity** (p5 to p95). A large spread is usual on 5G (here: 147 to 658 Mbit/s download). If the p95 is much higher than the p50, short peaks set the maximum; this is not a problem.
- **Upload with download load.** If it is much lower than without (here: 10.6 instead of 37.9 Mbit/s), the link can share the radio resources. Then a low upload base rate is correct. On a wired line, the queue of the unshaped download causes the decrease. Then the calibration keeps the base rate.
- **Cross delay and grant delay.** On fiber, both are near 0. On 5G, the cross delay is approximately 20 to 35 ms and the grant delay approximately 13 to 15 ms (median). Much higher values can show a weak signal or a busy cell.
- **Notes** (`# NOTE:` at the top). They tell which values were not measured, and why (a missing block type, or pings without one-way delays). For these values:
  - From the session config: the rates (no unshaped blocks), `learn_cross_delay` (no usable autorate `dl` blocks), `reflectors` (fewer than 4 reflectors reply to ICMP timestamps).
  - Calibration defaults, not the session config: the delay thresholds (30 ms without measured idle jitter) and `ping_method`. The default `ping_method` is `icmp` if fewer than 4 reflectors reply to ICMP timestamps. If your session config has other values, set them again in the output.
- `pinger_count` and `ping_source_addr` come from the session config. `connection_active_thr_kbps` also comes from the session config, but it is not more than half of the lowest minimum rate. Else, on a slow link, the traffic stays below it and the controller cannot leave the idle state. The other keys that the output does not contain get the program defaults.

Then test the proposed config: record a second session with it as a variant, and compare it with your old config:

```bash
sudo ./scripts/record-session.sh -i enp7s0 -c my-5g.yaml -r 2 -W 30 -A calibrated=my-5g-calibrated.yaml
```

## Online calibration

The daemon can also run a calibration session for one link itself, on the router. Another program makes the load: for example the LuCI page in a browser behind the router. Thus, no PC with the tools of step 1 is necessary. The analysis is the same as in step 2. During the session, the traffic to `speed.cloudflare.com` goes through the link of the session, also on a router with more than one WAN link (see `pin` in [configuration.md](configuration.md#control-socket)).

The control socket has the commands (see [Control socket](configuration.md#control-socket)):

- `calibrate start LINK [LOAD_S]`: start a session. One session at a time. Both directions of the link need the rate control (`adjust: true`).
- `calibrate status`: the status as one line of JSON.
- `calibrate stop [SESSION]`: stop the session. With `SESSION` (the field `session` of the status), stop only that session: a program that shows an older session does not stop a newer one.

The session has 5 blocks: unshaped `dl`, autorate `dl`, unshaped `ul`, autorate `ul`, unshaped `both`. Each block has 15 s of idle time, `LOAD_S` of load (default 40 s) and 3 s at the end. With 40 s, the session takes approximately 5 minutes. In the unshaped blocks, the shaper rate of the link is 10 Gbit/s (no useful limit) and the rate control stops. In the autorate blocks, the normal rate control runs.

The session records the pings of the link with its configured ping method. Thus, with `ping_method icmp`, the recordings do not show if reflectors reply to ICMP timestamps. In the idle time of the first block, the session also sends ICMP timestamp requests. It sends them to the configured reflectors, to the default reflectors for `icmp-timestamp` and to reflectors in North America, Asia and Australia (maximum 32, 4 requests each second to each reflector). The rules for the result of this probe are the same as for the recordings. At least 4 reflectors with 20 replies, 90 % replies and a low jitter give `ping_method icmp-timestamp`, with the reflectors that have the lowest jitter. The probe leaves out the far reflectors: a reflector is in the list only if its lowest RTT is at most 2 × the lowest RTT of all reflectors, or the lowest RTT + 30 ms (the larger value). A far reflector can have a low jitter, but its replies come late. Else the proposal is `icmp`, and the reason tells how many reflectors replied. Sometimes the probe cannot run (for example, no raw socket). Or a reflector gets fewer than 20 requests, because the probe stopped early or a DNS lookup was slow. Then the recordings select the ping method, and a note tells why. After a change from `icmp` to `icmp-timestamp`, calibrate again: only the one-way delays show the upload delay from download activity (`learn_cross_delay`).

The program that makes the load reads `calibrate status` each second:

| Field | Meaning |
|---|---|
| `state` | `none` (no session since the start of the daemon), `running`, `done`, `failed` or `stopped` |
| `block`, `blocks`, `mode`, `policy` | the current block |
| `session` | the identifier of the session: each start gives a new value |
| `phase` | `idle`, `load` or `tail` |
| `load` | the load that the program must make now: `dl`, `ul`, `both`, or empty (no load) |
| `phase_left_s`, `left_s` | the time left in the phase and in the session |
| `error` | the reason of a failed session |
| `result` | at the end: the proposal and the measured data |

Each status request is also a sign that the load program operates. Without a request for 15 s, the session stops: then there is no load, and the link must not stay without a shaper limit. At the end of the session (also after an error or a stop), the rate control of the link continues as before. A direction that was paused stays paused.

The session writes its files next to the control socket, in the directory `calibration`. The session goes to `sessions/LINK` (in the format of step 1), and the result goes to `results/LINK.json`. The directories must belong to the user of the program, and other users must not be able to write to them. The program makes them with mode 0700. Else the session fails. The rate control comes back after the last block, before the analysis. The result has the proposed values for each direction: `min_rate_kbps`, `base_rate_kbps`, `max_rate_kbps` and `owd_delta_delay_thr_ms`. A rate that was not measured is not in the result. The result also has `learn_cross_delay`, `ping_method`, `reflectors` and `connection_active_thr_kbps`. Then the reasons (`why`), the notes and the measured distributions (`measured`).

Differences to the session of step 1:

- One round, shorter blocks: the result is less reliable on a link with a large change of the capacity. Calibrate again at another time and compare.
- The load comes from the other program. If it cannot fill the link (for example a slow PC or a slow load server), the capacity is too low. Then the unshaped blocks show a low delay increase.
- The other traffic of the network adds to the load. Do not use the network during the session.

## Limits

- One session measures one time of day. The capacity of a mobile cell changes during the day (load of other users). Record at a busy time (for example in the evening) to get a safe `min_rate_kbps`.
- The controller adapts the rate between `min_rate_kbps` and `max_rate_kbps`, and `learn_cross_delay` adapts to the current cross delay. Thus, the calibration sets the ranges and the start values; the controller does the rest.
- The analysis uses one link. For a router with more links, calibrate each link alone.
- Starlink is not covered yet (the reconfiguration every 15 s needs other measurements).
