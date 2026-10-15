# cake-autorate-go

**A Go rewrite of [cake-autorate](https://github.com/lynxthecat/cake-autorate). It started as an experiment: how much fewer resources does a native program use than the original Bash scripts? Now, it also has changes to the algorithm for variable links such as 5G and Starlink: low bufferbloat and high throughput.**

> **WARNING:** This project is experimental, and AI tools wrote most of the
> code. The author still tests it. Use it at your own risk and with much
> caution: it can operate incorrectly or set incorrect rates on your traffic
> shaper. If you need a proven solution, use the
> [original cake-autorate](https://github.com/lynxthecat/cake-autorate).

## Attribution

This project is a Go reimplementation of [cake-autorate](https://github.com/lynxthecat/cake-autorate) by [lynxthecat](https://github.com/lynxthecat) and the contributors. The original algorithm, design and research are their work.

The original project has the [GPL-2.0](https://www.gnu.org/licenses/old-licenses/gpl-2.0.html) license. This rewrite has the same license.

## Why a Go rewrite?

There were two goals:

- Use fewer resources than the original Bash scripts.
- Find how much faster a compiled program can follow the delay changes and adjust the rates.

The original cake-autorate is a large Bash script (approximately 1,800 lines). It starts many background processes (fping, monitor, log manager), and these processes send data through FIFOs. On a router with few resources, this design has overhead: Bash starts new processes for calculations, for parsing and for process control.

Go is a good choice for these reasons:

- **Goroutines** replace the Bash background processes one to one.
- **Cross-compilation** to ARM is easy (`GOARCH=arm64`).
- **One static binary**: the router needs no other programs for the usual operation. Only if netlink fails, the program uses `tc` (iproute2).
- **Channels** replace the FIFOs.

## Differences from the original

These functions are only in cake-autorate-go:

- **One process for all links.** The original starts one instance with its own processes for each link.
- **ICMP timestamps in the program.** `icmp-timestamp` needs no external pinger. The original needs tsping, irtt or fping 5.3 or later for one-way delays.
- **Netlink** sets the CAKE bandwidth. The original starts `tc` for each change.
- **Options for 5G links:** `learn_cross_delay`, `repeat_decrease_on_rise` and `refractory_on_decrease` (see [5G links](#5g-links)). Their default is off. Thus, the default behavior stays near the original.
- **`calibrate`** makes a proposed config from a test session on your link.
- **Config checks:** a warning for each unknown key (with a similar known key), and a warning for each reflector that is known to not reply to ICMP timestamp requests. If all reflectors of a link are known to not reply, the program does not start.
- **Recording and replay** (`--record`, `--no-adjust`): the same events always give the same decisions. A simulator, emulations and session scripts use this to compare configs and versions, also with the original (see [Tests and tools](#tests-and-tools)).

These functions are only in the original: the pingers irtt and tsping, a reflector list from a URL, the MQTT publisher, the CPU statistics, and the export of the log.

## Comparison with the original

These tests compare the delay and the throughput of the two controllers on the same emulated link. Each controller sets the CAKE rate on the same link, with the same rates and reflectors. Both use one-way delays from ICMP timestamps (cake-autorate-go: `icmp-timestamp`, cake-autorate: `fping-ts`). All other values are the defaults of each program. Each test uses the same load in each block, and the same capacity changes.

The delay is the excess RTT under load: the RTT minus the minimum RTT. The throughput is a percentage of the mean capacity of the emulated link.

**Emulation of a 5G link** with [`scripts/emulate-link.sh`](docs/emulation.md) on a fiber connection: a capacity trace with download 30 to 400 Mbit/s and upload 5 to 80 Mbit/s, with deep fades. A queue of 1000 packets in the emulated modem. 2 rounds.

| Load | Controller | Delay p50 / p95 / p99 | Throughput |
|---|---|---|---|
| Download | cake-autorate-go | 7–9 / 42–43 / 58–63 ms | 87–88 % |
| Download | cake-autorate-go with `repeat_decrease_on_rise` | 14–15 / 44–45 / 62–66 ms | 93 % |
| Download | cake-autorate | 13–17 / 59–60 / 80–109 ms | 93–95 % |
| Two directions | cake-autorate-go | 15–17 / 56–57 / 380–448 ms | DL 67–68 %, UL 78 % |
| Two directions | cake-autorate-go with `repeat_decrease_on_rise` | 21–22 / 63 / 475–505 ms | DL 69–70 %, UL 81–82 % |
| Two directions | cake-autorate | 21–23 / 85–89 / 251–425 ms | DL 75–78 %, UL 75–78 % |

**CI emulation** ([`scripts/ci-emulation.sh`](docs/development.md), each CI run): a square wave (download 40 and 20 Mbit/s, upload 10 and 5 Mbit/s, 15 s each), load in the two directions.

| Controller | Delay p95 | Throughput |
|---|---|---|
| cake-autorate-go | 43.7 ms | DL 85 %, UL 78 % |
| cake-autorate | 82.9 ms | DL 85 %, UL 77 % |

Results:

- cake-autorate-go has a much lower usual delay: the p95 is approximately 30 to 50 % lower.
- With download load, cake-autorate gets more throughput. `repeat_decrease_on_rise` closes this gap for approximately 2 ms more delay (p95).
- Under load in the two directions, the p99 of cake-autorate-go is higher. The cause is the deep fades of the trace (for example, the upload capacity decreases from 50 to 5 Mbit/s for 1.6 s). The two controllers have their delay peaks in the same seconds. cake-autorate-go keeps the rate nearer to the capacity, thus the modem queue fills a little more before the first decrease has an effect.
- These results are from emulations. A comparison on a real 5G link is not done yet.

## Benchmark: Bash vs Go

Measured on a **GL.iNet GL-MT6000** with OpenWrt, 60 seconds for each version, a sample each 5 seconds, with [`benchmark.sh`](benchmark.sh). An earlier version of cake-autorate-go gave these values. A new measurement with the current version is planned.

### Resource usage (one WAN)

| Value | Bash | Go | Difference |
|---|---|---|---|
| **Processes** | 6 | 1 | **6x fewer** |
| **Memory (RSS)** | 11,640 KB (peak 11,728 KB) | 10,457 KB (peak 10,860 KB) | approximately 10 % less |
| **CPU** | 6.15 % (peak 6.7 %) | 4.22 % (peak 4.3 %) | **1.5x less** |

With one WAN interface, the Go version uses fewer resources in all values. The difference is small, because Bash is sufficiently efficient for one workload that waits for I/O. With more interfaces, the difference increases (see [Multi-WAN](#multi-wan)).

### Rate adjustments

Measured under continuous load on two WAN interfaces. The Go version makes more rate adjustments each second. Thus, it follows the changes of the link more closely:

| Link | Direction | Bash adjustments/s | Go adjustments/s |
|---|---|---|---|
| **Primary** | DL | 0.3 | 0.6 |
| **Primary** | UL | 0.3 | 0.5 |
| **Secondary** | DL | 0.7 | 0.8 |
| **Secondary** | UL | 0.3 | 0.5 |

The Go version makes 1.5 to 2 times more rate adjustments each second than Bash. With 3 to 7 times more decisions (see [Decisions](#decisions)), the Go shaper makes smaller and more frequent adjustments when the load changes.

### Multi-WAN

Many routers have **more than one WAN interface** (failover, load balancing, or different uplinks). Measured on the same router with **two WAN interfaces**, 120 seconds for each version:

| Value | Bash | Go | Difference |
|---|---|---|---|
| **Processes** | 12 | 1 | **12x fewer** |
| **Memory (RSS)** | 22,640 KB (peak 22,688 KB) | 10,688 KB (peak 11,792 KB) | **2x less** |
| **CPU** | 10.95 % (peak 15.2 %) | 5.20 % (peak 6.3 %) | **2.1x less** |

The Bash version starts **a full set of processes for each interface** (Bash, fping, awk and monitor processes). Thus, its resource usage increases linearly with the number of interfaces. The Go version controls all interfaces in one process with goroutines. A second WAN link adds almost no overhead.

On a router with 128 to 256 MB of RAM, 23 MB or 11 MB for one daemon is an important difference, because other services (dnsmasq, firewall, VPN) also need memory.

### Decisions

The Go version makes more rate decisions each second:

| Link | Bash decisions/s | Go decisions/s | Difference |
|---|---|---|---|
| **Primary** | 1.1 | 7.7 | **7x more** |
| **Secondary** | 1.5 | 5.0 | **3.3x more** |

The Go version also shows the types of decisions (bufferbloat, high load, decay).

### Summary

| Aspect | Better | Details |
|---|---|---|
| Memory (1 WAN) | **Go (approximately 10 %)** | 10.5 MB vs 11.6 MB |
| Memory (2 WAN) | **Go (2x)** | 11 MB vs 23 MB: Bash increases linearly with each interface |
| CPU (1 WAN) | **Go (1.5x)** | 4.2 % vs 6.2 % |
| CPU (2 WAN) | **Go (2.1x)** | 5.2 % vs 11.0 %: netlink, no new processes |
| Rate adjustments | **Go (1.5–2x)** | More rate adjustments each second |
| Decisions | **Go (3–7x)** | 5 to 7.7 decisions/s vs 1.1 to 1.5 decisions/s |
| Processes | **Go** | 1 vs 6 to 12 processes (increases with the interfaces) |
| Multi-WAN | **Go** | Goroutines vs a full set of processes for each interface |
| Installation | **Go** | One static binary, no Bash, awk or fping (`tc` only if netlink fails) |
| Maturity | **Bash** | Tested by many users for a long time |

## Features

- Changes the CAKE bandwidth from delay measurements.
- Baselines with EWMA, and bufferbloat detection.
- Three states: **Running**, **Idle** and **Stall**.
- Monitors the replies of the reflectors and replaces a reflector that misses too many replies (if the list has a spare reflector).
- Compensation for the serialization delay of the packets.
- One-way delays with ICMP timestamps (`ping_method: icmp-timestamp`), without an external pinger: a queue in one direction decreases only the rate of that direction.
- Options for 5G links: `learn_cross_delay` (the upload delay that download activity causes) and `repeat_decrease_on_rise` (fewer decreases while the queue empties). See [5G links](#5g-links).
- More than one link in one process.
- `calibrate`: makes a proposed config from a test session on your link.
- Recording and replay of the controller inputs and decisions, a link simulator, and link emulations for tests (see [Tests and tools](#tests-and-tools)).
- YAML config with default values for all settings.
- Small resource usage for routers.

## Installation

### Pre-built binaries

Download the latest release for your platform from the [Releases](https://github.com/megglos/cake-autorate-go/releases) page.

### Build from source

```bash
# Native build
go build -o cake-autorate-go .

# Cross-compile for ARMv8 (for example Raspberry Pi 4, modern OpenWrt routers)
GOOS=linux GOARCH=arm64 go build -o cake-autorate-go .
```

## OpenWrt service installation

Run [`setup.sh`](setup.sh) on your router. It installs the binary, makes a default config and makes a procd service:

```bash
# Copy the binary and the script to the router, then:
chmod +x setup.sh
./setup.sh ./cake-autorate-go
```

The script installs the binary to `/usr/sbin/cake-autorate-go`, makes a default config in `/etc/cake-autorate/config.yaml`, and adds the procd service `cake-autorate-go`. Then, do the instructions on the screen to configure, enable and start the service.

## Configuration

cake-autorate-go uses one YAML config file (default: `/etc/cake-autorate/config.yaml`). Each setting has a default value. Thus, set only the values that are different for your link.

To make a config file with the default values:

```bash
./cake-autorate-go --defaults > /etc/cake-autorate/config.yaml
```

[config.example.yaml](config.example.yaml) has an example with a comment for each value.

The program shows a warning for each key in the file that it does not know (for example, a typing error). The warning gives the line number and, if a known key is similar, that key. An unknown key has no effect.

To check a config file without a start of the program, use `-check-config`. The program prints the warnings and exits. The exit code is 1 if the file is not valid. Add `-strict` to get exit code 1 also when there are warnings:

```bash
./cake-autorate-go -config /etc/cake-autorate/config.yaml -check-config -strict
```

### Quick start (one WAN)

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

### More than one WAN link

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

### Reflectors

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

### Ping method

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

### Delay thresholds

Each direction has three delay thresholds. They set when the controller finds bufferbloat and how much it decreases the rate:

```yaml
owd_delta_delay_thr_ms: 30.0               # OWD delta above this: a bufferbloat sample
avg_owd_delta_max_adjust_up_thr_ms: 10.0   # Increase the rate only if the average delta is below this
avg_owd_delta_max_adjust_down_thr_ms: 60.0  # At this average delta: the largest rate decrease
```

- `owd_delta_delay_thr_ms`: the threshold of the one-way delay (OWD) delta. When the difference between the current OWD and the baseline is more than this value, the controller counts a bufferbloat sample.
- `avg_owd_delta_max_adjust_up_thr_ms`: the shaper increases the rate only when the average OWD delta is less than this value. A lower value gives fewer rate increases.
- `avg_owd_delta_max_adjust_down_thr_ms`: at this average delta, the controller applies the largest rate decrease. Between the thresholds, the decrease is between the minimum and the maximum adjustment factors.

### Rate adjustment

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

### EWMA parameters

The algorithm uses exponentially weighted moving averages (EWMA) for the delay baselines:

```yaml
alpha_baseline_increase: 0.001   # The baseline increases very slowly when the delay increases
alpha_baseline_decrease: 0.9     # The baseline decreases quickly when the delay decreases
alpha_delta_ewma: 0.095          # Not used (kept for config compatibility)
```

The two values are different on purpose. The baseline must increase slowly: else it includes the bufferbloat. The baseline must decrease quickly: then it follows a real decrease of the path delay.

### Idle state

These settings stop the pings on a connection without traffic:

```yaml
enable_sleep_function: true
connection_active_thr_kbps: 2000   # Below this: idle
sustained_idle_sleep_thr_s: 60.0   # Time (s) with low traffic before the Idle state
```

When the download and the upload are both below `connection_active_thr_kbps` for `sustained_idle_sleep_thr_s` seconds, the controller goes into the **Idle** state. It stops the pings and makes no rate adjustments. The shaper rates do not change (with `min_shaper_rates_enforcement: true`, they go to `min_rate_kbps`). When the traffic in one direction is at `connection_active_thr_kbps` or more again, the controller goes back to **Running**. It sets the rates to `base_rate_kbps` and starts the pings again.

Before the Idle state, with low traffic, the controller is in the Running state. Then, the rate moves slowly to `base_rate_kbps`.

### Stall detection

The program finds a connection that has stopped:

```yaml
stall_detection_thr: 5                  # Number of ping timeouts in sequence for a stall
global_ping_response_timeout_s: 10.0    # Minimum time since the last ping reply for a stall (s)
connection_stall_thr_kbps: 10           # Traffic at or above this ends the stall
```

The controller goes into the **Stall** state when the two conditions are true: `stall_detection_thr` ping timeouts in sequence, and no ping reply for more than `global_ping_response_timeout_s`. In the Stall state, it stops the pings and makes no rate adjustments. When the traffic in one direction is at `connection_stall_thr_kbps` or more, it goes back to **Running**, sets the rates to `base_rate_kbps` and starts the pings again.

### Timing

```yaml
monitor_interval_ms: 200                # Time between two throughput samples
bufferbloat_refractory_period_ms: 300   # Time after a bufferbloat decrease without a rate increase
refractory_on_decrease: false           # true: also only one rate decrease in each refractory period
repeat_decrease_on_rise: false          # true: a second decrease in the refractory period only when the delay increases
learn_cross_delay: false                # true: ignore the upload delay from download activity (icmp-timestamp only)
decay_refractory_period_ms: 1000        # Time between two adjustments for low load
```

### Logging

```yaml
log_to_file: true
log_file_path: /var/log/cake-autorate.log
log_file_max_size_kb: 2000    # The program truncates the log when it is larger than this
debug: false                  # Write more log messages (or use the --debug flag)
```

### 5G links

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

### Configuration reference

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

## Usage

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

The `--record FILE` option writes the inputs of the controller (pings, throughput samples, reflector changes) and its decisions (shaper rates, state changes) to a JSONL file. It does not record the start and stop of the pingers. A replay of the file through the controller gives the same decisions. Use recordings to analyze a session, to calibrate the link simulator and to tune the controller. For the file format and a recording procedure, see [docs/recording.md](docs/recording.md).

### Calibration from a session

`cake-autorate-go calibrate -session DIR [-out FILE]` analyzes a session of `scripts/record-session.sh` and writes a proposed config for the link. Each value has a comment with the measured data. Examine the values before you use them.

For the full procedure (how to record the session, what the script does, and how to read the result), see [docs/calibration.md](docs/calibration.md).

| Measurement | From | Config value |
|---|---|---|
| Capacity (1 s means) | unshaped `dl`, `ul` and `both` blocks | `min_rate_kbps` (p5 / 2), `base_rate_kbps` (p25; upload: not more than the p50 with download load), `max_rate_kbps` (p95) |
| Idle jitter of each reflector | the idle time at the start of each block | delay thresholds (at least 2 × the median jitter, at least 30 ms); `reflectors`: the `pinger_count` + 2 (at least 4) with the lowest jitter, without the ones above 2 × the median (the program selects the active reflectors at random from the list) |
| Upload delay from download activity | autorate `dl` blocks | `learn_cross_delay` (on if the median is more than 10 ms; without these blocks, the value of the session config stays) |
| Upload grant delay (5G) | autorate `ul` blocks, while the upload shaper rate is less than half of the capacity | upload threshold: at least the median + 10 ms |
| ICMP timestamp replies | all blocks | `ping_method` (`icmp-timestamp` with 4 or more reflectors) |

Use a session of one link with the modes `dl`, `ul` and `both` and the policies `unshaped` and `autorate`. If a block type is missing, the output tells which values are not measured. The command does not accept a session with more than one link.

## Tests and tools

- **Unit tests and replay:** `go test ./...`. The core of the controller does not read the clock and starts no goroutines. Thus, the same events always give the same decisions, in the tests and in a replay of a recording.
- **Link simulator:** a model of a link with a bottleneck queue, TCP flows and ping replies, with scenarios such as a steady link, a capacity drop, 5G and Starlink. A scorecard gives the delay and the throughput for each scenario, and a baseline check finds regressions. See [docs/development.md](docs/development.md).
- **CI emulation:** [`scripts/ci-emulation.sh`](scripts/ci-emulation.sh) tests the real binary, CAKE and netlink in network namespaces, without an internet connection. Each CI run also runs the original cake-autorate on the same link for comparison (option `-U`).
- **Link emulation:** [`scripts/emulate-link.sh`](scripts/emulate-link.sh) emulates a variable link (for example 5G) from a capacity trace on a fast connection, with real TCP traffic. See [docs/emulation.md](docs/emulation.md).
- **Test sessions on a real link:** [`scripts/record-session.sh`](scripts/record-session.sh) measures different policies in alternating blocks: no shaper, a fixed shaper, cake-autorate-go, other configs or binaries (`-A`), and the original cake-autorate (`-U`). [`scripts/summarize-session.sh`](scripts/summarize-session.sh) shows the delay and the throughput of each block. See [docs/recording.md](docs/recording.md).

## Prerequisites

- **CAKE qdisc:** CAKE must already be configured on your interfaces. This program changes only the bandwidth parameter. It does not set up CAKE. For tests on a PC, [`scripts/cake-pc.sh`](scripts/cake-pc.sh) sets up CAKE and the IFB interface.
- **Linux:** the program sets the bandwidth through netlink sockets (no new processes). If netlink is not available, the shaper uses `tc` (iproute2).

## Development

See [docs/development.md](docs/development.md): the structure of the code, the tests, the link simulator and its scorecard, the baseline check, and tests on a real link.

## Status

This is an **experimental** project. The core algorithm comes from the original cake-autorate, with some changes. The larger changes for 5G links (see [5G links](#5g-links)) are options, and their default is off. The tests are emulations, a simulator and test sessions on one 5G link. The program is not yet tested much in daily use. Use it at your own risk and report problems as issues.

## License

This project has the GNU General Public License v2.0. See [LICENSE](LICENSE).

This is a derivative work of [cake-autorate](https://github.com/lynxthecat/cake-autorate), which also has the GPL-2.0 license.
