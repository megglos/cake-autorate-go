# cake-autorate-go

**A Go rewrite of [cake-autorate](https://github.com/lynxthecat/cake-autorate) — an experiment to explore how many fewer resources a native implementation may occupy compared to the original bash scripts.**

> **WARNING:** This project is experimental and largely vibe-coded. It is still
> undergoing testing by the author. Use it at your own risk and with high
> caution -- it may behave unexpectedly or misconfigure your traffic shaper.
> If you need a proven solution, use the
> [original cake-autorate](https://github.com/lynxthecat/cake-autorate) instead.

## Attribution

This project is a Go reimplementation of the excellent [cake-autorate](https://github.com/lynxthecat/cake-autorate) by [lynxthecat](https://github.com/lynxthecat) and contributors. All credit for the original algorithm, design, and research goes to the original authors.

The original project is licensed under [GPL-2.0](https://www.gnu.org/licenses/old-licenses/gpl-2.0.html), and this rewrite maintains the same license.

## Why a Go rewrite?

The motivation was twofold: to explore how much fewer resources a native implementation may consume compared to the original bash scripts, and to see how much more responsive a compiled solution could be at tracking latency changes and adjusting rates.

The original cake-autorate is a sophisticated bash script (~1,800 lines) that orchestrates multiple background processes (fping, monitor, log manager) communicating via FIFOs. On resource-constrained routers, this architecture has overhead from constant fork/exec cycles for arithmetic, parsing, and process coordination.

Go was chosen because:
- **Goroutines** are a natural 1:1 replacement for bash background processes
- **Cross-compilation** to ARM is trivial (`GOARCH=arm64`)
- **Single static binary** — no runtime dependencies on the router
- **Channels** replace FIFO-based IPC with zero overhead

### Benchmark: Bash vs Go

Measured on a **GL.iNet GL-MT6000** running OpenWrt, 60 seconds per version, sampled every 5 seconds using [`benchmark.sh`](benchmark.sh).

#### Steady-State Resource Usage

| Metric | Bash | Go | Improvement |
|---|---|---|---|
| **Processes** | 6 | 1 | **6x fewer** |
| **Memory (RSS)** | 11,640 KB (peak 11,728 KB) | 10,457 KB (peak 10,860 KB) | ~10% less |
| **CPU** | 6.15% (peak 6.7%) | 4.22% (peak 4.3%) | **1.5x less** |

Even on a single WAN interface, the Go version is leaner across all metrics. The process count drops from 6 to 1, CPU usage is 1.5x lower, and memory is ~10% less. The savings are modest here because bash is reasonably efficient for a single I/O-bound workload — the real gains compound with multiple interfaces (see Multi-WAN below).

#### Responsiveness (Rate Adjustment Speed)

Measured during sustained load across two WAN interfaces. The Go version sustains a higher rate of adjustments per second, meaning it tracks changing conditions more closely:

| Link | Direction | Bash adj/s | Go adj/s |
|---|---|---|---|
| **Primary** | DL | 0.3 | 0.6 |
| **Primary** | UL | 0.3 | 0.5 |
| **Secondary** | DL | 0.7 | 0.8 |
| **Secondary** | UL | 0.3 | 0.5 |

The Go version consistently makes 1.5–2x more rate adjustments per second than bash. Combined with 3–7x higher decision throughput (see Decision Throughput below), this means the Go shaper tracks load transitions more granularly — smaller, more frequent adjustments rather than larger, less frequent ones.

**Why this matters:** More frequent adjustments mean the shaper tracks actual bandwidth demand more closely, reducing both over-provisioning (wasted capacity) and under-provisioning (bufferbloat). This is especially important during rapid load transitions like starting a download or joining a video call.

#### Multi-WAN Scaling

The single-interface benchmark above shows rough parity on memory and CPU. However, routers often manage **multiple WAN interfaces** (dual-WAN failover, load balancing, or separate uplinks). This is where the architectural differences become decisive.

Measured on the same OpenWrt router with **two WAN interfaces** configured, 120 seconds per version:

| Metric | Bash | Go | Improvement |
|---|---|---|---|
| **Processes** | 12 | 1 | **12x fewer** |
| **Memory (RSS)** | 22,640 KB (peak 22,688 KB) | 10,688 KB (peak 11,792 KB) | **2x less** |
| **CPU** | 10.95% (peak 15.2%) | 5.20% (peak 6.3%) | **2.1x less** |

The bash version spawns a **full process tree per interface** — each WAN link gets its own set of bash, fping, awk, and monitor subprocesses. Resource usage scales linearly with the number of interfaces. The Go version handles all interfaces within a single process using goroutines, so adding a second WAN link adds negligible overhead.

On a resource-constrained router with 128–256 MB of RAM, the difference between 23 MB and 11 MB for a single daemon is significant — especially when running alongside other services (dnsmasq, firewall, VPN).

#### Decision Throughput

The Go version processes significantly more rate decisions per second, enabled by its tight goroutine-based control loop:

| Link | Bash decisions/s | Go decisions/s | Speedup |
|---|---|---|---|
| **Primary** | 1.1 | 7.7 | **7x more** |
| **Secondary** | 1.5 | 5.0 | **3.3x more** |

The Go version also provides a breakdown of decision types (bufferbloat, high-load, decay), giving better visibility into shaper behavior.

#### Summary

| Aspect | Winner | Details |
|---|---|---|
| Memory (1 WAN) | **Go (~10%)** | 10.5 MB vs 11.6 MB |
| Memory (2 WAN) | **Go (2x)** | 11 MB vs 23 MB — bash scales linearly per interface |
| CPU (1 WAN) | **Go (1.5x)** | 4.2% vs 6.2% |
| CPU (2 WAN) | **Go (2.1x)** | 5.2% vs 11.0% — native netlink + zero-alloc hot path |
| Responsiveness | **Go (1.5–2x)** | More frequent rate adjustments per second |
| Decision throughput | **Go (3–7x)** | 5–7.7 decisions/s vs 1.1–1.5 decisions/s |
| Process count | **Go** | 1 vs 5-12 processes (scales with interfaces) |
| Multi-WAN scaling | **Go** | Goroutines vs full process trees per interface |
| Deployment | **Go** | Single static binary, no bash/awk/fping dependencies |
| Maturity | **Bash** | Battle-tested with a large user community |

## Features

- Dynamic CAKE bandwidth adjustment based on latency measurements
- EWMA-based baseline tracking and bufferbloat detection
- Three operating states: **Running**, **Idle**, **Stall**
- Reflector health monitoring with automatic replacement
- Wire-packet serialization delay compensation
- YAML configuration with sensible defaults
- Minimal resource footprint suitable for embedded routers

## Installation

### Pre-built binaries

Download the latest release for your platform from the [Releases](https://github.com/megglos/cake-autorate-go/releases) page.

### Build from source

```bash
# Native build
go build -o cake-autorate-go .

# Cross-compile for ARMv8 (e.g., Raspberry Pi 4, modern OpenWrt routers)
GOOS=linux GOARCH=arm64 go build -o cake-autorate-go .
```

## OpenWrt Service Installation

Run [`setup.sh`](setup.sh) on your router to install the binary, generate a default config, and create a procd service:

```bash
# Copy the binary and script to the router, then:
chmod +x setup.sh
./setup.sh ./cake-autorate-go
```

This installs the binary to `/usr/sbin/cake-autorate-go`, creates a default config at `/etc/cake-autorate/config.yaml`, and registers the `cake-autorate-go` procd service. Follow the on-screen instructions to configure, enable, and start the service.

## Configuration

cake-autorate-go uses a single YAML configuration file (default: `/etc/cake-autorate/config.yaml`). Every setting has a sensible default, so you only need to override what differs from your setup.

Generate a default configuration file as a starting point:

```bash
./cake-autorate-go --defaults > /etc/cake-autorate/config.yaml
```

See [config.example.yaml](config.example.yaml) for a fully annotated example.

### Quick Start (Single WAN)

At minimum, set the interfaces and rate limits for your connection:

```yaml
links:
  - name: primary
    download:
      interface: ifb-wan        # Your download interface
      adjust: true
      min_rate_kbps: 25000      # Minimum download bandwidth
      base_rate_kbps: 100000    # Steady-state target bandwidth
      max_rate_kbps: 100000     # Maximum download bandwidth
    upload:
      interface: wan            # Your upload interface
      adjust: true
      min_rate_kbps: 5000
      base_rate_kbps: 35000
      max_rate_kbps: 35000
```

**Finding your interfaces:** On OpenWrt, the download interface is typically `ifb-wan` (or `ifb4eth0` / `ifb4eth1` depending on your setup), and the upload interface matches your WAN device (e.g., `wan`, `eth0`, `eth1`). Run `tc qdisc show` to see which interfaces have CAKE configured.

**Setting rates:** `base_rate_kbps` should match your ISP's provisioned speed. Set `min_rate_kbps` to the lowest usable bandwidth (typically 10–25% of base) and `max_rate_kbps` to the maximum your link can sustain. If unsure, set `max_rate_kbps` equal to `base_rate_kbps` to start.

### Multi-WAN Setup

To manage multiple WAN links, add additional entries to the `links` array. Each link runs concurrently within the same process — no need to spawn separate instances.

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
    ping_interface: wan         # Bind pings to this WAN for policy routing

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

For multi-WAN routers (e.g., with mwan3), set `ping_interface` on each link so ICMP packets are routed through the correct WAN using `SO_BINDTODEVICE`. You can optionally set `ping_source_addr` to bind to a specific source IP.

### Legacy Single-Link Format

For backward compatibility, single-WAN setups can use the flat top-level format instead of the `links` array:

```yaml
download:
  interface: ifb-wan
  adjust: true
  min_rate_kbps: 5000
  base_rate_kbps: 20000
  max_rate_kbps: 80000
upload:
  interface: wan
  adjust: true
  min_rate_kbps: 5000
  base_rate_kbps: 20000
  max_rate_kbps: 35000
reflectors:
  - 1.1.1.1
  - 8.8.8.8
```

This is automatically migrated to a single link named "default" at load time. The `links` format is preferred for new configurations.

### Reflectors

Reflectors are remote hosts used for latency measurement via ICMP ping. Each link can specify its own reflector list, or inherit the defaults:

```yaml
reflectors:              # Default reflectors (used if a link omits its own)
  - 1.1.1.1
  - 1.0.0.1
  - 8.8.8.8
  - 8.8.4.4
  - 9.9.9.9
  - 149.112.112.112
  - 208.67.222.222
  - 208.67.220.220

pinger_count: 6          # How many reflectors to ping simultaneously
ping_interval_ms: 300    # Interval between pings per reflector (ms)
```

Misbehaving reflectors (those that stop responding or show erratic latency) are automatically detected and replaced from the pool.

### Delay Thresholds

Each direction has three delay thresholds that control how aggressively bufferbloat is detected and acted upon:

```yaml
owd_delta_delay_thr_ms: 30.0               # OWD delta above this = possible bufferbloat
avg_owd_delta_max_adjust_up_thr_ms: 10.0   # Only increase rate if avg delta is below this
avg_owd_delta_max_adjust_down_thr_ms: 60.0  # Maximum avg delta — triggers largest rate decrease
```

- `owd_delta_delay_thr_ms` — The one-way delay (OWD) delta threshold. When the delta between the current OWD and the baseline exceeds this, a bufferbloat sample is counted.
- `avg_owd_delta_max_adjust_up_thr_ms` — The average OWD delta must be below this value for the shaper to increase the rate. Lower values make rate increases more conservative.
- `avg_owd_delta_max_adjust_down_thr_ms` — At this average delta, the maximum rate decrease is applied. The actual decrease is interpolated between the min and max adjustment factors.

### Rate Adjustment Tuning

These factors control how the shaper adjusts bandwidth in response to load and bufferbloat:

```yaml
# Bufferbloat response — rate is multiplied by a factor between max and min
shaper_rate_min_adjust_down_bufferbloat: 0.99   # Mild bufferbloat → small decrease
shaper_rate_max_adjust_down_bufferbloat: 0.75   # Severe bufferbloat → large decrease

# High load — rate increases when load is above high_load_thr and no bufferbloat
shaper_rate_min_adjust_up_load_high: 1.0        # Minimum increase (1.0 = hold steady)
shaper_rate_max_adjust_up_load_high: 1.04       # Maximum increase per cycle

# Low load — gentle drift toward base_rate when load is below high_load_thr
shaper_rate_adjust_down_load_low: 0.99          # Decay toward base (when rate > base)
shaper_rate_adjust_up_load_low: 1.01            # Recovery toward base (when rate < base)

high_load_thr: 0.75    # Fraction of current rate considered "high load" (0.0–1.0)
```

### EWMA Parameters

The algorithm uses exponentially weighted moving averages (EWMA) to track delay baselines:

```yaml
alpha_baseline_increase: 0.001   # Baseline moves very slowly when delay rises
alpha_baseline_decrease: 0.9     # Baseline tracks quickly when delay drops
alpha_delta_ewma: 0.095          # Smoothing for the delay delta moving average
```

The asymmetry between `alpha_baseline_increase` (slow) and `alpha_baseline_decrease` (fast) is intentional — the baseline should be slow to rise (to avoid normalizing bufferbloat) but quick to drop (to track genuine improvements in path latency).

### Idle and Sleep Detection

To avoid unnecessary rate adjustments on idle connections:

```yaml
enable_sleep_function: true
connection_active_thr_kbps: 2000   # Below this = idle
sustained_idle_sleep_thr_s: 60.0   # Seconds of idle before entering sleep state
```

When the connection enters the **Idle** state, rate adjustments pause and bandwidth drifts back toward `base_rate_kbps`. After `sustained_idle_sleep_thr_s` seconds, pinging stops entirely (**Sleep** state) and resumes when traffic reappears.

### Stall Detection

Detects when a connection appears completely stalled:

```yaml
stall_detection_thr: 5                  # Consecutive stall samples to trigger
connection_stall_thr_kbps: 10           # Rate below this = stall sample
global_ping_response_timeout_s: 10.0    # All reflectors must respond within this
```

### Timing

```yaml
monitor_interval_ms: 200                # How often to sample interface throughput
bufferbloat_refractory_period_ms: 300   # Cooldown between rate increases after bufferbloat
decay_refractory_period_ms: 1000        # Cooldown between low-load decay adjustments
```

### Logging

```yaml
log_to_file: true
log_file_path: /var/log/cake-autorate.log
log_file_max_size_kb: 2000    # Log is truncated when it exceeds this size
debug: false                  # Enable verbose debug logging (or use --debug flag)
```

### Configuration Reference

| Setting | Default | Description |
|---|---|---|
| `links[].download.interface` | `ifb-wan` | Download interface name |
| `links[].upload.interface` | `wan` | Upload interface name |
| `links[].*.adjust` | `false` | Enable rate adjustment for this direction |
| `links[].*.min_rate_kbps` | `5000` | Minimum bandwidth (kbit/s) |
| `links[].*.base_rate_kbps` | `20000` | Steady-state target bandwidth (kbit/s) |
| `links[].*.max_rate_kbps` | `80000`/`35000` | Maximum bandwidth (DL/UL, kbit/s) |
| `links[].reflectors` | 8 public DNS | ICMP ping targets for latency measurement |
| `links[].ping_interface` | *(none)* | Bind ICMP to this interface (multi-WAN) |
| `links[].ping_source_addr` | *(none)* | Source IP for ICMP packets |
| `pinger_count` | `6` | Reflectors to ping simultaneously |
| `ping_interval_ms` | `300` | Ping interval per reflector (ms) |
| `enable_sleep_function` | `true` | Enable idle/sleep detection |
| `connection_active_thr_kbps` | `2000` | Idle threshold (kbit/s) |
| `sustained_idle_sleep_thr_s` | `60.0` | Idle time before sleep (seconds) |
| `bufferbloat_detection_window` | `6` | Samples in detection window |
| `bufferbloat_detection_thr` | `3` | Samples to trigger bufferbloat |
| `alpha_baseline_increase` | `0.001` | EWMA alpha for rising baseline |
| `alpha_baseline_decrease` | `0.9` | EWMA alpha for falling baseline |
| `alpha_delta_ewma` | `0.095` | EWMA alpha for delay delta |
| `high_load_thr` | `0.75` | High-load threshold (0.0–1.0) |
| `monitor_interval_ms` | `200` | Throughput sampling interval (ms) |
| `bufferbloat_refractory_period_ms` | `300` | Cooldown after bufferbloat (ms) |
| `decay_refractory_period_ms` | `1000` | Cooldown between decay adjustments (ms) |
| `reflector_response_deadline_s` | `1.0` | Per-reflector response timeout (s) |
| `global_ping_response_timeout_s` | `10.0` | All-reflector response timeout (s) |
| `log_to_file` | `true` | Enable file logging |
| `log_file_path` | `/var/log/cake-autorate.log` | Log file location |
| `log_file_max_size_kb` | `2000` | Max log file size (KB) |
| `debug` | `false` | Enable debug logging |

## Usage

```bash
# Run with default config path (/etc/cake-autorate/config.yaml)
sudo ./cake-autorate-go

# Run with custom config
sudo ./cake-autorate-go --config /path/to/config.yaml

# Enable debug logging
sudo ./cake-autorate-go --debug

# Print version
./cake-autorate-go --version
```

Root privileges (or `CAP_NET_RAW` + `CAP_NET_ADMIN`) are required for ICMP pinging and netlink-based CAKE bandwidth control.

## Prerequisites

- **CAKE qdisc** must already be configured on your interfaces. This tool only adjusts the bandwidth parameter — it does not set up CAKE itself.
- **Linux** — bandwidth adjustments use netlink sockets directly (no subprocess overhead). If netlink is unavailable, the shaper falls back to `tc` (iproute2).

## Status

This is an **experimental** project. While the core algorithm faithfully reimplements the original cake-autorate logic, it has not been extensively tested in production. Use at your own risk and please report issues.

## License

This project is licensed under the GNU General Public License v2.0 — see [LICENSE](LICENSE).

This is a derivative work of [cake-autorate](https://github.com/lynxthecat/cake-autorate), which is also licensed under GPL-2.0.
