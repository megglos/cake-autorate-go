# cake-autorate-go

**A Go rewrite of [cake-autorate](https://github.com/lynxthecat/cake-autorate) — an experiment to explore how much less resources a native implementation may occupy compared to the original bash scripts.**

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

This is where the Go version significantly outperforms bash. During a load ramp-up from ~80 Mbps to 200 Mbps:

| Metric | Bash | Go (1 WAN) | Go (2 WANs) |
|---|---|---|---|
| **Time to full bandwidth** | ~4.6 seconds | ~1.2 seconds | ~1.3 seconds |
| **Adjustment interval** | ~200-250 ms | ~44-59 ms | ~45-55 ms |
| **Speedup** | — | **~3.8x faster** | **~3.5x faster** |

With native netlink for CAKE bandwidth adjustments, the Go version reacts to each ping result and adjusts the shaper in ~45-59 ms — even while managing two WAN interfaces concurrently. The tight interval range reflects the elimination of `tc` subprocess variance. In bash, each adjustment cycle requires multiple fork+exec calls (fping parsing, awk calculations, tc invocation), each costing ~5-20 ms on a router CPU, adding up to ~200-250 ms per cycle.

**Why this matters:** For a latency-sensitive traffic shaper, reaching full bandwidth in ~1.2 seconds vs ~4.6 seconds means noticeably less bufferbloat during load transitions (e.g., starting a download, joining a video call). The Go version maintains this responsiveness even with multiple WAN interfaces — both links reach full bandwidth within 1.3 seconds simultaneously.

#### Multi-WAN Scaling

The single-interface benchmark above shows rough parity on memory and CPU. However, routers often manage **multiple WAN interfaces** (dual-WAN failover, load balancing, or separate uplinks). This is where the architectural differences become decisive.

Measured on the same OpenWrt router with **two WAN interfaces** configured, 60 seconds per version:

| Metric | Bash | Go | Improvement |
|---|---|---|---|
| **Processes** | 11 (peak 11) | 1 | **11x fewer** |
| **Memory (RSS)** | 21,603 KB (peak 21,704 KB) | 10,435 KB (peak 11,156 KB) | **2x less** |
| **CPU** | 12.11% (peak 13.8%) | 4.65% (peak 5.3%) | **2.6x less** |

The bash version spawns a **full process tree per interface** — each WAN link gets its own set of bash, fping, awk, and monitor subprocesses. Resource usage scales linearly with the number of interfaces. The Go version handles all interfaces within a single process using goroutines, so adding a second WAN link adds negligible overhead.

On a resource-constrained router with 128–256 MB of RAM, the difference between 21 MB and 10 MB for a single daemon is significant — especially when running alongside other services (dnsmasq, firewall, VPN).

#### Summary

| Aspect | Winner | Details |
|---|---|---|
| Memory (1 WAN) | **Go (~10%)** | 10.5 MB vs 11.6 MB |
| Memory (2 WAN) | **Go (2x)** | 10 MB vs 21 MB — bash scales linearly per interface |
| CPU (1 WAN) | **Go (1.5x)** | 4.2% vs 6.2% |
| CPU (2 WAN) | **Go (2.6x)** | 4.7% vs 12% — native netlink + zero-alloc hot path |
| Responsiveness | **Go (3.5–3.8x)** | 1.2s (1 WAN) / 1.3s (2 WANs) vs 4.6s ramp-up |
| Process count | **Go** | 1 vs 5-11 processes (scales with interfaces) |
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

Download the latest release for your platform from the [Releases](https://github.com/megglos/cake-autorotate-go/releases) page.

### Build from source

```bash
# Native build
go build -o cake-autorate .

# Cross-compile for ARMv8 (e.g., Raspberry Pi 4, modern OpenWrt routers)
GOOS=linux GOARCH=arm64 go build -o cake-autorate .
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
./cake-autorate --defaults > /etc/cake-autorate/config.yaml
```

Edit the configuration to match your setup. At minimum, you must set:

- `download.interface` — your download interface (e.g., `ifb-wan`)
- `upload.interface` — your upload interface (e.g., `wan`)
- Rate limits for both directions (`min_rate_kbps`, `base_rate_kbps`, `max_rate_kbps`)

See [config.example.yaml](config.example.yaml) for a fully documented example.

## Usage

```bash
# Run with default config path (/etc/cake-autorate/config.yaml)
sudo ./cake-autorate

# Run with custom config
sudo ./cake-autorate --config /path/to/config.yaml

# Enable debug logging
sudo ./cake-autorate --debug

# Print version
./cake-autorate --version
```

Root privileges (or `CAP_NET_RAW` + `CAP_NET_ADMIN`) are required for ICMP pinging and `tc` commands.

## Prerequisites

- **CAKE qdisc** must already be configured on your interfaces. This tool only adjusts the bandwidth parameter — it does not set up CAKE itself.
- **Linux** with `tc` (iproute2) available in PATH.

## Status

This is an **experimental** project. While the core algorithm faithfully reimplements the original cake-autorate logic, it has not been extensively tested in production. Use at your own risk and please report issues.

## License

This project is licensed under the GNU General Public License v2.0 — see [LICENSE](LICENSE).

This is a derivative work of [cake-autorate](https://github.com/lynxthecat/cake-autorate), which is also licensed under GPL-2.0.
