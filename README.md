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

Generate a default configuration file:

```bash
./cake-autorate-go --defaults > /etc/cake-autorate/config.yaml
```

Edit the configuration to match your setup. At minimum, you must set:

- `download.interface` — your download interface (e.g., `ifb-wan`)
- `upload.interface` — your upload interface (e.g., `wan`)
- Rate limits for both directions (`min_rate_kbps`, `base_rate_kbps`, `max_rate_kbps`)

See [config.example.yaml](config.example.yaml) for a fully documented example.

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
