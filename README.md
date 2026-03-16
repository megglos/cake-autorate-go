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

| Metric | Bash | Go |
|---|---|---|
| **Time to full bandwidth** | ~4.6 seconds | ~1.1 seconds |
| **Adjustment interval** | ~200-250 ms | ~35-70 ms |
| **Speedup** | — | **~4x faster** |

The Go version reacts to each ping result and adjusts the shaper in ~35-70 ms, bottlenecked only by the single `tc` exec call. In bash, each adjustment cycle requires multiple fork+exec calls (fping parsing, awk calculations, tc invocation), each costing ~5-20 ms on a router CPU, adding up to ~200-250 ms per cycle.

**Why this matters:** For a latency-sensitive traffic shaper, reaching full bandwidth in 1 second vs 5 seconds means noticeably less bufferbloat during load transitions (e.g., starting a download, joining a video call).

#### Summary

| Aspect | Winner | Details |
|---|---|---|
| Memory | Tie | ~10 MB each |
| CPU | Tie | ~7-8% each |
| Responsiveness | **Go (4x)** | 1.1s vs 4.6s ramp-up |
| Process count | **Go** | 1 vs 5 processes |
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
