# cake-autorate-go

**A Go rewrite of [cake-autorate](https://github.com/lynxthecat/cake-autorate) — an experiment to explore how much less resources a native implementation may occupy compared to the original bash scripts.**

## Attribution

This project is a Go reimplementation of the excellent [cake-autorate](https://github.com/lynxthecat/cake-autorate) by [lynxthecat](https://github.com/lynxthecat) and contributors. All credit for the original algorithm, design, and research goes to the original authors.

The original project is licensed under [GPL-2.0](https://www.gnu.org/licenses/old-licenses/gpl-2.0.html), and this rewrite maintains the same license.

## Why a Go rewrite?

The original cake-autorate is a sophisticated bash script (~1,800 lines) that orchestrates multiple background processes (fping, monitor, log manager) communicating via FIFOs. On resource-constrained routers, this architecture has overhead:

| Resource | Bash (original) | Go (this project) |
|---|---|---|
| **Processes** | 4-6+ (bash, fping, awk, monitor) | 1 (single binary) |
| **Memory** | ~20-50MB (multiple processes) | ~5-10MB (single process) |
| **CPU** | Fork/exec for arithmetic, parsing | Native computation |
| **Startup** | Slow (source configs, spawn processes) | Near-instant |

Go was chosen because:
- **Goroutines** are a natural 1:1 replacement for bash background processes
- **Cross-compilation** to ARM is trivial (`GOARCH=arm64`)
- **Single static binary** — no runtime dependencies on the router
- **Channels** replace FIFO-based IPC with zero overhead

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
