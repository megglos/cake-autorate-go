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

Measured on a real OpenWrt router over 120 seconds per version, sampled every 5 seconds using [`benchmark.sh`](benchmark.sh).

#### Steady-State Resource Usage

| Metric | Bash | Go | Notes |
|---|---|---|---|
| **Processes** | 5 (peak 6) | 1 | Bash runs bash + fping + awk + monitor subprocesses |
| **Memory (RSS)** | 9,856 KB | 10,552 KB | Roughly equivalent; Go runtime baseline is ~8 MB |
| **CPU** | 7.05% (peak 7.6%) | 8.11% (peak 8.3%) | Roughly equivalent at steady state |

Steady-state resource usage is **roughly equivalent**. The Go runtime's baseline memory (~8 MB) offsets the per-process savings. For a lightweight, I/O-bound workload that mostly sleeps between ping cycles, bash is efficient at steady state.

#### Responsiveness (Rate Adjustment Speed)

This is where the Go version significantly outperforms bash. During a load ramp-up from ~80 Mbps to 200 Mbps:

| Metric | Bash | Go (1 WAN) | Go (2 WANs) |
|---|---|---|---|
| **Time to full bandwidth** | ~4.6 seconds | ~1.1 seconds | ~1.3 seconds |
| **Adjustment interval** | ~200-250 ms | ~35-70 ms | ~45-55 ms |
| **Speedup** | — | **~4x faster** | **~3.5x faster** |

With native netlink for CAKE bandwidth adjustments, the Go version reacts to each ping result and adjusts the shaper in ~45-55 ms — even while managing two WAN interfaces concurrently. The tight interval range reflects the elimination of `tc` subprocess variance. In bash, each adjustment cycle requires multiple fork+exec calls (fping parsing, awk calculations, tc invocation), each costing ~5-20 ms on a router CPU, adding up to ~200-250 ms per cycle.

**Why this matters:** For a latency-sensitive traffic shaper, reaching full bandwidth in ~1 second vs ~5 seconds means noticeably less bufferbloat during load transitions (e.g., starting a download, joining a video call). The Go version maintains this responsiveness even with multiple WAN interfaces — both links reach full bandwidth within 1.3 seconds simultaneously.

#### Multi-WAN Scaling

The single-interface benchmark above shows rough parity on memory and CPU. However, routers often manage **multiple WAN interfaces** (dual-WAN failover, load balancing, or separate uplinks). This is where the architectural differences become decisive.

Measured on the same OpenWrt router with **two WAN interfaces** configured, 60 seconds per version:

| Metric | Bash | Go | Improvement |
|---|---|---|---|
| **Processes** | 11 (peak 11) | 1 | **11x fewer** |
| **Memory (RSS)** | 21,580 KB (peak 21,752 KB) | 10,539 KB (peak 10,932 KB) | **2x less** |
| **CPU** | 12.20% (peak 13.8%) | 5.06% (peak 5.4%) | **2.4x less** |

The bash version spawns a **full process tree per interface** — each WAN link gets its own set of bash, fping, awk, and monitor subprocesses. Resource usage scales linearly with the number of interfaces. The Go version handles all interfaces within a single process using goroutines, so adding a second WAN link adds negligible overhead.

On a resource-constrained router with 128–256 MB of RAM, the difference between 21 MB and 10 MB for a single daemon is significant — especially when running alongside other services (dnsmasq, firewall, VPN).

#### Summary

| Aspect | Winner | Details |
|---|---|---|
| Memory (1 WAN) | Tie | ~10 MB each |
| Memory (2 WAN) | **Go (2x)** | 10 MB vs 21 MB — bash scales linearly per interface |
| CPU (1 WAN) | Tie | ~7-8% each |
| CPU (2 WAN) | **Go (2.4x)** | 5% vs 12% — native netlink eliminates per-adjustment subprocess overhead |
| Responsiveness | **Go (3.5–4x)** | 1.1s (1 WAN) / 1.3s (2 WANs) vs 4.6s ramp-up |
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
