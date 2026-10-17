# Configuration

cake-autorate-go uses one YAML config file (default: `/etc/cake-autorate/config.yaml`). Each setting has a default value. Thus, set only the values that are different for your link.

With the OpenWrt package, do not use this file. The configuration is in UCI (`/etc/config/cake-autorate`). At each start, the service writes `/var/etc/cake-autorate.yaml` from it and uses that file (see [luci.md](luci.md#configuration-with-uci)). The UCI options have the same names and meaning as the keys on this page.

To make a config file with the default values:

```bash
./cake-autorate-go --defaults > /etc/cake-autorate/config.yaml
```

[config.example.yaml](../config.example.yaml) has an example with a comment for each value.

The program shows a warning for each key in the file that it does not know (for example, a typing error). The warning gives the line number and, if a known key is similar, that key. An unknown key has no effect.

To check a config file without a start of the program, use `-check-config`. The program prints the warnings and exits. The exit code is 1 if the file is not valid. Add `-strict` to get exit code 1 also when there are warnings:

```bash
./cake-autorate-go -config /etc/cake-autorate/config.yaml -check-config -strict
```

## Quick start (one WAN)

Set the interfaces and the rates of your connection:

```yaml
links:
  - name: primary
    download:
      interface: ifb-wan        # Your download interface
      adjust: true
      min_rate_kbps: 25000      # Minimum download bandwidth
      base_rate_kbps: 100000    # Usual download bandwidth
      max_rate_kbps: 100000     # Maximum download bandwidth
    upload:
      interface: wan            # Your upload interface
      adjust: true
      min_rate_kbps: 5000
      base_rate_kbps: 35000
      max_rate_kbps: 35000
```

**Find your interfaces:** on OpenWrt, the download interface is usually `ifb-wan` (or `ifb4eth0` / `ifb4eth1`). The upload interface is your WAN device (for example `wan`, `eth0` or `eth1`). To see the interfaces with CAKE, run `tc qdisc show`.

**Set the rates:** set `base_rate_kbps` to the speed of your contract. Set `min_rate_kbps` to the lowest bandwidth that you can use (usually 10 to 25 % of the base rate). Set `max_rate_kbps` to the maximum bandwidth of your link. If you do not know the maximum, set `max_rate_kbps` to `base_rate_kbps` first. To measure the values on your link, use [`calibrate`](#calibration-from-a-session).

## More than one WAN link

For more WAN links, add entries to the `links` list. All links operate at the same time in the same process. You do not have to start more instances.

```yaml
links:
  - name: primary
    download:
      interface: ifb4eth1
      adjust: true
      min_rate_kbps: 25000
      base_rate_kbps: 200000
      max_rate_kbps: 200000
    upload:
      interface: eth1
      adjust: true
      min_rate_kbps: 25000
      base_rate_kbps: 35000
      max_rate_kbps: 35000
    reflectors:
      - 1.1.1.1
      - 8.8.8.8
      - 9.9.9.9
    ping_interface: wan         # Send the pings through this WAN (policy routing)

  - name: secondary
    download:
      interface: ifb4eth2
      adjust: true
      min_rate_kbps: 5000
      base_rate_kbps: 50000
      max_rate_kbps: 100000
    upload:
      interface: eth2
      adjust: true
      min_rate_kbps: 5000
      base_rate_kbps: 10000
      max_rate_kbps: 20000
    reflectors:
      - 1.0.0.1
      - 8.8.4.4
    ping_interface: wan2
```

On a router with more than one WAN (for example with mwan3), set `ping_interface` for each link. Then, the ICMP packets go through the correct WAN (`SO_BINDTODEVICE`). You can also set `ping_source_addr` to use a specific source IP address.

## Link overrides

A link can override four top-level keys: `ping_method`, `pinger_count`, `ping_interval_ms` and `learn_cross_delay`. Write the key inside the link. A key that the link does not set uses the top-level value. Use this on a router with different link types, for example a 5G link with `icmp-timestamp` and a fiber link with `icmp`. If a link sets `ping_method` and no reflectors, and the config sets no top-level reflectors, the link uses the default list for its own ping method.

```yaml
ping_method: icmp
links:
  - name: fiber
    # ...
  - name: g5
    ping_method: icmp-timestamp
    learn_cross_delay: true
    # ...
```

## Reflectors

Reflectors are remote hosts. The program sends ICMP pings to them to measure the delay. You can set reflectors in two places:

- `links[].reflectors`: the reflectors of one link.
- `reflectors` (top level): the reflectors of each link that does not set its own.

If you set neither, the program uses a default list for the `ping_method` (see [Ping method](#ping-method)). The default list for `icmp`:

```yaml
reflectors:
  - 1.1.1.1
  - 1.0.0.1
  - 8.8.8.8
  - 8.8.4.4
  - 9.9.9.9
  - 149.112.112.112
  - 208.67.222.222
  - 208.67.220.220

pinger_count: 6          # Number of reflectors that the program pings at the same time
ping_interval_ms: 300    # Time between two pings to one reflector (ms)
```

The program counts the missed replies of each active reflector. When a reflector misses too many replies (`reflector_misbehaving_detection_thr` in the last `reflector_misbehaving_detection_window` pings), the program replaces it with a spare reflector: a reflector from the list that is not active. The list must thus have more reflectors than `pinger_count`. Else, the reflector stays active. The program does not examine the delay of a reflector: a reflector with a high or unstable delay stays active.

## Ping method

```yaml
ping_method: icmp-timestamp   # "icmp" (default) or "icmp-timestamp"
```

- `icmp` uses ICMP echo. It measures only the round-trip time (RTT). The controller uses RTT/2 as the delay of each direction. Thus, a queue in one direction also decreases the rate of the other direction. For example, a saturated upload decreases the download rate.
- `icmp-timestamp` uses ICMP timestamp requests (RFC 792). The reply gives the delay of each direction. Thus, the controller decreases only the rate of the direction with the queue. On 5G and Starlink, the upload is frequently saturated. Thus, use `icmp-timestamp` on these links.

Requirements for `icmp-timestamp`:

- Reflectors that reply to ICMP timestamp requests. Many public DNS servers do not reply (for example 1.1.1.1, 8.8.8.8, 9.9.9.9). If the config does not set `reflectors`, the program uses this default list:

  ```yaml
  reflectors:
    - 94.140.14.14            # AdGuard DNS
    - 94.140.15.15            # AdGuard DNS
    - fra-de-ping.vultr.com
    - ams-nl-ping.vultr.com
    - par-fr-ping.vultr.com
    - fsn1-speed.hetzner.com
    - nbg1-speed.hetzner.com
    - hel1-speed.hetzner.com
  ```

  If you set reflectors, use only reflectors that reply. The program shows a warning for each reflector that is known to not reply (the default reflectors for `icmp`). If all reflectors of a link are known to not reply, the program does not start. The program does not test the other reflectors at the start. The program replaces a reflector that does not reply with a spare reflector from the list (see [Reflectors](#reflectors)).
- A firewall that lets ICMP timestamp replies (type 14) through.
- A correct clock (NTP) on the router. The controller uses only the change of each one-way delay. Thus, a constant clock offset has no effect, but a fast clock drift can look like a delay change.

## Delay thresholds

Each direction has three delay thresholds. They set when the controller finds bufferbloat and how much it decreases the rate:

```yaml
owd_delta_delay_thr_ms: 30.0               # OWD delta above this: a bufferbloat sample
avg_owd_delta_max_adjust_up_thr_ms: 10.0   # Increase the rate only if the average delta is below this
avg_owd_delta_max_adjust_down_thr_ms: 60.0  # At this average delta: the largest rate decrease
```

- `owd_delta_delay_thr_ms`: the threshold of the one-way delay (OWD) delta. When the difference between the current OWD and the baseline is more than this value, the controller counts a bufferbloat sample.
- `avg_owd_delta_max_adjust_up_thr_ms`: the shaper increases the rate only when the average OWD delta is less than this value. A lower value gives fewer rate increases.
- `avg_owd_delta_max_adjust_down_thr_ms`: at this average delta, the controller applies the largest rate decrease. Between the thresholds, the decrease is between the minimum and the maximum adjustment factors.

## Rate adjustment

These factors set how the shaper changes the bandwidth for load and bufferbloat:

```yaml
# Bufferbloat: the rate is multiplied by a factor between max and min
shaper_rate_min_adjust_down_bufferbloat: 0.99   # Small bufferbloat: small decrease
shaper_rate_max_adjust_down_bufferbloat: 0.75   # Large bufferbloat: large decrease

# High load: the rate increases when the load is above high_load_thr and there is no bufferbloat
shaper_rate_min_adjust_up_load_high: 1.0        # Minimum increase (1.0: no change)
shaper_rate_max_adjust_up_load_high: 1.04       # Maximum increase for each step

# Low load: the rate moves slowly to base_rate when the load is below high_load_thr
shaper_rate_adjust_down_load_low: 0.99          # Decrease to the base rate (when the rate is above the base rate)
shaper_rate_adjust_up_load_low: 1.01            # Increase to the base rate (when the rate is below the base rate)

high_load_thr: 0.75    # Part of the current rate that is "high load" (0.0 to 1.0)
```

## EWMA parameters

The algorithm uses exponentially weighted moving averages (EWMA) for the delay baselines:

```yaml
alpha_baseline_increase: 0.001   # The baseline increases very slowly when the delay increases
alpha_baseline_decrease: 0.9     # The baseline decreases quickly when the delay decreases
alpha_delta_ewma: 0.095          # Not used (kept for config compatibility)
```

The two values are different on purpose. The baseline must increase slowly: else it includes the bufferbloat. The baseline must decrease quickly: then it follows a real decrease of the path delay.

## Idle state

These settings stop the pings on a connection without traffic:

```yaml
enable_sleep_function: true
connection_active_thr_kbps: 2000   # Below this: idle
sustained_idle_sleep_thr_s: 60.0   # Time (s) with low traffic before the Idle state
```

When the download and the upload are both below `connection_active_thr_kbps` for `sustained_idle_sleep_thr_s` seconds, the controller goes into the **Idle** state. It stops the pings and makes no rate adjustments. The shaper rates do not change (with `min_shaper_rates_enforcement: true`, they go to `min_rate_kbps`). When the traffic in one direction is at `connection_active_thr_kbps` or more again, the controller goes back to **Running**. It sets the rates to `base_rate_kbps` and starts the pings again.

Before the Idle state, with low traffic, the controller is in the Running state. Then, the rate moves slowly to `base_rate_kbps`.

## Stall detection

The program finds a connection that has stopped:

```yaml
stall_detection_thr: 5                  # Number of ping timeouts in sequence for a stall
global_ping_response_timeout_s: 10.0    # Minimum time since the last ping reply for a stall (s)
connection_stall_thr_kbps: 10           # Traffic at or above this ends the stall
```

The controller goes into the **Stall** state when the two conditions are true: `stall_detection_thr` ping timeouts in sequence, and no ping reply for more than `global_ping_response_timeout_s`. In the Stall state, it stops the pings and makes no rate adjustments. When the traffic in one direction is at `connection_stall_thr_kbps` or more, it goes back to **Running**, sets the rates to `base_rate_kbps` and starts the pings again.

## Timing

```yaml
monitor_interval_ms: 200                # Time between two throughput samples
bufferbloat_refractory_period_ms: 300   # Time after a bufferbloat decrease without a rate increase
refractory_on_decrease: false           # true: also only one rate decrease in each refractory period
repeat_decrease_on_rise: false          # true: a second decrease in the refractory period only when the delay increases
learn_cross_delay: false                # true: ignore the upload delay from download activity (icmp-timestamp only)
decay_refractory_period_ms: 1000        # Time between two adjustments for low load
```

## Logging

```yaml
log_to_file: true
log_file_path: /var/log/cake-autorate.log
log_file_max_size_kb: 2000    # The program truncates the log when it is larger than this
debug: false                  # Write more log messages (or use the --debug flag)
```

## Status file

Set `status_file` to a path, for example `/var/run/cake-autorate/status.json`. The program then writes the status of all links to this file each second. A tool, for example a web interface, can read it. The program writes a temporary file and renames it, thus a reader never sees a partial file. The program removes the file when it stops.

The program writes the file each second. Thus, use a path on a RAM file system, for example `/var/run` on OpenWrt, so that the flash memory does not wear out.

The file is JSON. The key `schema` is the version of the format. For each link, it shows the state, whether the shaping is off (`unshaped`), the number of ping replies since the start (`replies`; the delay deltas change only with a new reply), and for each direction whether the rate control is paused, the shaper rate, the measured rate, the load, the last delay delta, its threshold, whether the last delta was above the threshold (`high_delay`; the threshold includes the time to send one packet at the shaper rate), the number of pings with a high delay in the detection window, the learned cross delay and the number of rate changes. It also shows the baseline RTT and the health of each reflector.

## Control socket

Set `control_socket` to a path, for example `/var/run/cake-autorate/control.sock`. The program then listens on this Unix socket. The path can have not more than 90 bytes. With the control socket, each link name must have 1 to 64 characters of `A-Z`, `a-z`, `0-9`, `_`, `.` and `-`. Only the owner of the socket can connect. A client sends one line and reads one line as the answer, `ok` or `error: MESSAGE`:

| Command | Effect |
|---|---|
| `pause LINK [dl\|ul]` | Set the shaper to the base rate and stop the rate changes. Without a direction, the command applies to both directions. |
| `resume LINK [dl\|ul]` | Start the rate control again from the base rate. |
| `unshape LINK [SECONDS]` | Turn the shaping of the link off: the shaper rate of each direction with rate control is 10 Gbit/s (no useful limit), and the rate control stops. With `SECONDS` (1 to 3600), the shaping comes back after this time. |
| `shape LINK` | Turn the shaping of the link on again. Each direction gets its pause state from before `unshape`: a paused direction stays paused at the base rate, the others start the rate control again from the base rate. |
| `list` | Answer with the link names. |
| `calibrate start LINK [LOAD_S]` | Start a calibration session for the link (see [calibration.md](calibration.md#online-calibration)). `LOAD_S`: the load time of each block, 20 to 120 s (default 40). |
| `calibrate status` | Answer with the status of the session as one line of JSON. Another program makes the load that the field `load` tells. |
| `calibrate stop [SESSION]` | Stop the session (with `SESSION`: only if that session runs). The rate control continues as before the session. |
| `history` | Answer with the samples of the last 15 minutes of each link, as one line of JSON. The program adds one sample each second, when it writes the status file. Only with `status_history: true` and a status file. |

Each `pause` and `resume` sets the base rate, also when the direction is already in that state. Thus, send the command again after an error answer. A paused direction still measures delay. It does not change the shaper rate. The pause does not survive a restart. The status file shows the pause of each direction (`paused`). A recording contains the pause and resume events, thus a replay gives the same decisions.

Each `unshape` sets the rates again, also when the shaping is already off; after an error answer of `unshape` or `shape`, send the command again. Use `unshape` for a short comparison, for example a bufferbloat test with and without the shaper. The delay measurement and the status continue: an idle link starts its pings at once, and the link does not go to `IDLE` while the shaping is off. While the shaping is off, the program refuses `pause`, `resume` and `calibrate start`; during a calibration, it refuses `unshape`. The unshaped state does not survive a restart, and the program turns the shaping on again before it stops. The status file shows the state of each link (`unshaped`) and, for `unshape LINK SECONDS`, the time left (`unshaped_left_s`). A program that turns the shaping off for a test (for example a web page) should send `SECONDS`: then the shaping comes back also when the program stops. A recording contains the mode (`unshaped`) and the rate changes of `unshape` (`fixed_rate`) and of `shape` (`pause`).

```bash
echo "pause primary ul" | nc -U /var/run/cake-autorate/control.sock
```

The answer of `history` has the fields `schema`, `updated` (the time of the last sample), `interval_ms`, `fields` (the names of the values of a sample) and `links`. Each link has a `name` and `samples`, the oldest sample first. A sample is an array in the order of `fields`: the time in ms since 1970 (`t_ms`), then for the download and the upload the shaper rate and the achieved rate in kbit/s, and the delay delta and the threshold in ms. The program keeps the history only in memory: a restart starts with an empty history. The history is off by default (`status_history`). On, it uses approximately 40 bytes for each sample: approximately 36 kB for each link.

## 5G links

On 5G, the radio adds upload delay that no shaper can remove:

- **Cross delay:** while the download is busy, the scheduler gives fewer upload grants. On the 5G test link, download activity added a median of 21 to 33 ms to the upload delay.
- **Grant delay:** an active upload waits for its grants. On the test link, a median of 13 to 15 ms.

With the default thresholds (30 ms), the cross delay alone is near the upload threshold. Under load in the two directions, the upload rate then decreases to near `min_rate_kbps`, also without a queue in the shaper. These settings gave the best results on the test link. Add them to your link config (they do not replace the interfaces and the rates):

```yaml
links:
  - name: 5g
    upload:
      owd_delta_delay_thr_ms: 20                # 20 to 25: above the grant delay
      avg_owd_delta_max_adjust_down_thr_ms: 40
ping_method: icmp-timestamp                     # one-way delays: necessary for learn_cross_delay
refractory_on_decrease: true
learn_cross_delay: true
```

- `learn_cross_delay` learns the cross delay and ignores it. It learns only while the download is active and the upload load was below 50 % of the upload shaper rate for 2 s or more. Thus, it needs times with download load and a low upload. If the load starts in the two directions at the same time, the learned value can stay too low. In the tests, 30 s of download load came before the upload load. On the test link, it gave approximately 1.6 to 2 times the upload throughput under load in the two directions, for 20 to 30 ms more upload delay (p95).
- A lower upload threshold (20 ms) with `learn_cross_delay` gave the same upload throughput as 30 ms, with approximately 7 ms less upload delay (p95). Do not set the upload threshold below the grant delay: then the upload rate collapses.
- The results change much from one test to the next (the 5G capacity changes). Make a test on your link before you change the defaults.
- `repeat_decrease_on_rise` (with `icmp-timestamp`) prevents the extra decreases while the queue empties after a decrease. In a fiber emulation with a 5G capacity trace (download load, 2 rounds), it gave 93 % of the capacity instead of 87 to 88 %, for approximately 2 ms more delay (p95). Under load in the two directions: 2 to 4 % more throughput, approximately 6 ms more delay (p95). In the simulator, it increased the delay much with RTT/2 (`icmp`, `fping`). Thus, use it only with one-way delays. It has no effect when `refractory_on_decrease` is `true`. The tests on the 5G link used `refractory_on_decrease` and not this option. A test of `repeat_decrease_on_rise` on the 5G link is not done yet.

## Configuration reference

The defaults are the values of `--defaults`. In a `links` entry, set the interfaces, `adjust` and the rates: a value that the entry does not set is empty, `false` or 0. Only the delay thresholds and the reflectors of a link come from the top level when the entry does not set them.

| Setting | Default | Description |
|---|---|---|
| `links[].download.interface` | `ifb-wan` | Download interface name |
| `links[].upload.interface` | `wan` | Upload interface name |
| `links[].*.adjust` | `true` (`false` in a `links` entry that does not set it) | Change the rate of this direction |
| `links[].*.min_rate_kbps` | `5000` | Minimum bandwidth (kbit/s) |
| `links[].*.base_rate_kbps` | `20000` | Usual bandwidth (kbit/s) |
| `links[].*.max_rate_kbps` | `80000`/`35000` | Maximum bandwidth (DL/UL, kbit/s) |
| `links[].reflectors` | the default list for the `ping_method` (see [Reflectors](#reflectors) and [Ping method](#ping-method)) | ICMP ping targets for the delay measurement |
| `links[].ping_interface` | *(none)* | Send the ICMP packets through this interface (multi-WAN) |
| `links[].ping_source_addr` | *(none)* | Source IP address of the ICMP packets |
| `pinger_count` | `6` | Number of reflectors that the program pings at the same time |
| `ping_interval_ms` | `300` | Time between two pings to one reflector (ms) |
| `ping_method` | `icmp` | `icmp` (RTT/2 for each direction) or `icmp-timestamp` (delay of each direction) |
| `enable_sleep_function` | `true` | Use the Idle state (stop the pings without traffic) |
| `connection_active_thr_kbps` | `2000` | Idle threshold (kbit/s) |
| `sustained_idle_sleep_thr_s` | `60.0` | Time with low traffic before the Idle state (s) |
| `bufferbloat_detection_window` | `6` | Number of samples in the detection window |
| `bufferbloat_detection_thr` | `3` | Number of bufferbloat samples in the window for bufferbloat |
| `alpha_baseline_increase` | `0.001` | EWMA alpha when the baseline increases |
| `alpha_baseline_decrease` | `0.9` | EWMA alpha when the baseline decreases |
| `alpha_delta_ewma` | `0.095` | Not used. The original uses it to compare reflectors. Kept for config compatibility |
| `high_load_thr` | `0.75` | High-load threshold (0.0 to 1.0) |
| `monitor_interval_ms` | `200` | Time between two throughput samples (ms) |
| `bufferbloat_refractory_period_ms` | `300` | Time after a bufferbloat decrease without a rate increase (ms) |
| `refractory_on_decrease` | `false` | Apply the bufferbloat refractory period also to decreases (one decrease in each period) |
| `repeat_decrease_on_rise` | `false` | In the refractory period after a decrease, decrease the rate again only when the delay is higher than at the last decrease. After a decrease, the queue empties only slowly, and each delayed ping decreases the rate again. This option prevents these extra decreases. Use it with `icmp-timestamp` (see [5G links](#5g-links)). `refractory_on_decrease` has priority |
| `learn_cross_delay` | `false` | With `icmp-timestamp`: learn the upload delay that download activity causes (for example, the 5G scheduler), and ignore it. It learns while the download is active and the upload load was less than 50 % of the upload shaper rate for 2 s or more. The learned delay is not more than the upload delay threshold |
| `decay_refractory_period_ms` | `1000` | Time between two adjustments for low load (ms) |
| `reflector_response_deadline_s` | `1.0` | Time in which a reflector must reply (s) |
| `global_ping_response_timeout_s` | `10.0` | Minimum time since the last ping reply for the Stall state (s) |
| `log_to_file` | `true` | Write the log to a file |
| `log_file_path` | `/var/log/cake-autorate.log` | Path of the log file |
| `log_file_max_size_kb` | `2000` | Maximum size of the log file (KB) |
| `debug` | `false` | Write more log messages |
| `status_file` | (empty) | Path of the status file. The program writes the status of all links to it each second. Empty: no status file |
| `status_history` | `false` | Keep the samples of the last 15 minutes of each link in memory, for the control command `history` (approximately 36 kB for each link). Needs `status_file` |
| `control_socket` | (empty) | Path of the Unix socket for the commands `pause`, `resume`, `list`, `history` and `calibrate`. Not more than 90 bytes. Empty: no control socket |

## Command line

```bash
# Run with the default config path (/etc/cake-autorate/config.yaml)
sudo ./cake-autorate-go

# Run with a different config
sudo ./cake-autorate-go --config /path/to/config.yaml

# Write more log messages
sudo ./cake-autorate-go --debug

# Record the controller inputs and decisions (see docs/recording.md)
sudo ./cake-autorate-go --record /tmp/session.jsonl.gz

# Measure the link without changes to the shaper rates
sudo ./cake-autorate-go --no-adjust --record /tmp/link.jsonl.gz

# Show the default config
./cake-autorate-go --defaults

# Show the version
./cake-autorate-go --version
```

The program must have root privileges (or `CAP_NET_RAW` and `CAP_NET_ADMIN`): it sends ICMP pings and sets the CAKE bandwidth through netlink.

### Recording

The `--record FILE` option writes the inputs of the controller (pings, throughput samples, reflector changes) and its decisions (shaper rates, state changes) to a JSONL file. It does not record the start and stop of the pingers. A replay of the file through the controller gives the same decisions. Use recordings to analyze a session, to calibrate the link simulator and to tune the controller. For the file format and a recording procedure, see [recording.md](recording.md).

### Calibration from a session

`cake-autorate-go calibrate -session DIR [-out FILE]` analyzes a session of `scripts/record-session.sh` and writes a proposed config for the link. Each value has a comment with the measured data. Examine the values before you use them.

For the full procedure (how to record the session, what the script does, and how to read the result), see [calibration.md](calibration.md).

| Measurement | From | Config value |
|---|---|---|
| Capacity (1 s means) | unshaped `dl`, `ul` and `both` blocks | `min_rate_kbps` (p5 / 2), `base_rate_kbps` (p25; upload: not more than the p50 with download load), `max_rate_kbps` (p95) |
| Idle jitter of each reflector | the idle time at the start of each block | delay thresholds (at least 2 × the median jitter, at least 30 ms); `reflectors`: the `pinger_count` + 2 (at least 4) with the lowest jitter, without the ones above 2 × the median (the program selects the active reflectors at random from the list) |
| Upload delay from download activity | autorate `dl` blocks | `learn_cross_delay` (on if the median is more than 10 ms; without these blocks, the value of the session config stays) |
| Upload grant delay (5G) | autorate `ul` blocks, while the upload shaper rate is less than half of the capacity | upload threshold: at least the median + 10 ms |
| ICMP timestamp replies | all blocks | `ping_method` (`icmp-timestamp` with 4 or more reflectors) |

Use a session of one link with the modes `dl`, `ul` and `both` and the policies `unshaped` and `autorate`. If a block type is missing, the output tells which values are not measured. The command does not accept a session with more than one link.
