# Comparison with the original

This page compares cake-autorate-go with the original [cake-autorate](https://github.com/lynxthecat/cake-autorate). It shows the reasons for the rewrite and the differences. It also shows the delay and the throughput on emulated links and on a real 5G link, and the resource use on a router.

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
- **Options for 5G links:** `learn_cross_delay`, `repeat_decrease_on_rise`, `refractory_on_decrease`, `increase_to_delay_thr`, `auto_max_rate` (default off) and `stall_recovery_ms` (default on, only with `icmp-timestamp`) (see [5G links](configuration.md#5g-links)). With the default `ping_method` (`icmp`), the behavior stays near the original.
- **`calibrate`** makes a proposed config from a test session on your link.
- **Config checks:** a warning for each unknown key (with a similar known key). Also a warning for each reflector that is known to not reply to ICMP timestamp requests. If all reflectors of a link are known to not reply, the program does not start.
- **Recording and replay** (`--record`, `--no-adjust`): the same events always give the same decisions. A simulator, emulations and session scripts use this to compare configs and versions, also with the original (see [Tests and tools](development.md#tests-and-tools)).

These functions are only in the original: the pingers irtt and tsping, a reflector list from a URL, and the MQTT publisher. Also the CPU statistics and the export of the log.

## Delay and throughput

These tests compare the delay and the throughput of the two controllers on the same emulated link. Each controller sets the CAKE rate on the same link, with the same rates and reflectors. Both use one-way delays from ICMP timestamps (cake-autorate-go: `icmp-timestamp`, cake-autorate: `fping-ts`). All other values are the defaults of each program. Each test uses the same load in each block, and the same capacity changes.

The delay is the excess RTT under load: the RTT minus the minimum RTT. The throughput is a percentage of the mean capacity of the emulated link.

**Emulation of a 5G link** with [`scripts/emulate-link.sh`](emulation.md) on a fiber connection. A capacity trace with download 30 to 400 Mbit/s and upload 5 to 80 Mbit/s, with deep fades. A queue of 1000 packets in the emulated modem. 2 rounds.

| Load | Controller | Delay p50 / p95 / p99 | Throughput |
|---|---|---|---|
| Download | cake-autorate-go | 7–9 / 42–43 / 58–63 ms | 87–88 % |
| Download | cake-autorate-go with `repeat_decrease_on_rise` | 14–15 / 44–45 / 62–66 ms | 93 % |
| Download | cake-autorate | 13–17 / 59–60 / 80–109 ms | 93–95 % |
| Two directions | cake-autorate-go | 15–17 / 56–57 / 380–448 ms | DL 67–68 %, UL 78 % |
| Two directions | cake-autorate-go with `repeat_decrease_on_rise` | 21–22 / 63 / 475–505 ms | DL 69–70 %, UL 81–82 % |
| Two directions | cake-autorate | 21–23 / 85–89 / 251–425 ms | DL 75–78 %, UL 75–78 % |

**CI emulation** ([`scripts/ci-emulation.sh`](development.md), each CI run). A square wave: download 40 and 20 Mbit/s, upload 10 and 5 Mbit/s, 15 s each. Load in the two directions.

| Controller | Delay p95 | Throughput |
|---|---|---|
| cake-autorate-go | 43.7 ms | DL 85 %, UL 78 % |
| cake-autorate | 82.9 ms | DL 85 %, UL 77 % |

Results:

- cake-autorate-go has a much lower usual delay: the p95 is approximately 30 to 50 % lower.
- With download load, cake-autorate gets more throughput. `repeat_decrease_on_rise` closes this gap for approximately 2 ms more delay (p95).
- Under load in the two directions, the p99 of cake-autorate-go is higher. The cause is the deep fades of the trace (for example, the upload capacity decreases from 50 to 5 Mbit/s for 1.6 s). The two controllers have their delay peaks in the same seconds. cake-autorate-go keeps the rate nearer to the capacity, thus the modem queue fills a little more before the first decrease has an effect.
- These results are from emulations. The next section shows the results on a real 5G link.

**Real 5G link** (Telekom, 2026-10-21 and 2026-10-22, 24 h, 14 sessions). An OpenWrt VM on the 5G modem makes the load (8 curl workers for each direction) and shapes with CAKE. Calibrated rates: download 34/130/245 Mbit/s, upload 19/94.5/110 Mbit/s (min/base/max), delay thresholds 36 ms. At night, the cell gave more than the calibrated maximum. Thus, from the ninth session, the two controllers used maximum rates of 500/160 Mbit/s; all other values stayed the same. cake-autorate-go uses `icmp-timestamp` and `stall_recovery_ms` 500, the original uses `fping-ts`. Each round has one block of each controller and an unshaped block. The blocks are 120 s, only the first two sessions used 60 s. Without shaping, the excess RTT under load is 350 to 790 ms (p50).

The capacity of the cell changes much between blocks. Thus, the table compares pairs: the original minus cake-autorate-go in the same round, with a t-test (95 %, * = significant).

| Load | Pairs | Throughput | Delay p50 | Delay p99 |
|---|---|---|---|---|
| Download | 23 | DL +26 ± 13 Mbit/s * | +7.0 ± 1.3 ms * | +29 ± 32 ms |
| Two directions | 23 | DL +19 ± 9 Mbit/s * | +9.1 ± 3.5 ms * | +41 ± 29 ms * |
| Upload | 4 | UL -3 Mbit/s | +7.6 ms * | +24 ms * |

Means of the first sessions (excess RTT p50 / p95 / p99, download as a percentage of the unshaped block of the round):

| Load | Controller | Delay p50 / p95 / p99 | Throughput |
|---|---|---|---|
| Download | cake-autorate-go | 26 / 98 / 186 ms | 55 % |
| Download | cake-autorate | 33 / 117 / 213 ms | 66 % |
| Two directions | cake-autorate-go | 30 / 81 / 153 ms | DL 61 %, UL 74 Mbit/s |
| Two directions | cake-autorate | 47 / 121 / 179 ms | DL 80 %, UL 73 Mbit/s |
| Upload | cake-autorate-go | 22 / 49 / 62 ms | 94 % |
| Upload | cake-autorate | 30 / 68 / 86 ms | 91 % |

The 5G options move cake-autorate-go between the two (pairs with the default of cake-autorate-go):

| Options | Load | Pairs | Download | Delay p50 |
|---|---|---|---|---|
| `refractory_on_decrease` | Download | 32 | +9 ± 9 Mbit/s | +0.8 ms * |
| `refractory_on_decrease` | Two directions | 32 | +5 ± 7 Mbit/s | +1.6 ms * |
| `increase_to_delay_thr`, `refractory_on_decrease` | Download | 17 | +21 ± 15 Mbit/s * | +4.1 ms * |
| `increase_to_delay_thr`, `refractory_on_decrease` | Two directions | 17 | +13 ± 8 Mbit/s * | +6.4 ms * |

Results on the real 5G link:

- cake-autorate-go has a lower delay in all load types: p50 7 to 9 ms lower, p99 up to 41 ms lower.
- cake-autorate gets more download: approximately +20 % (19 to 26 Mbit/s). The upload is equal.
- With `increase_to_delay_thr` and `refractory_on_decrease`, cake-autorate-go gets most of this download, with less delay than the original: p50 +4.1 and +6.4 ms, against +7.0 and +9.1 ms (60 to 70 % of the delay cost). The delay cost of these options and of the original depends on the load of the cell: at night +2 ms p50, in the morning +16 ms p50 (two directions).
- At night, the cell gave up to 420 Mbit/s download, more than the calibrated maximum (245 Mbit/s). With a fixed maximum, all controllers then stay at the maximum. `auto_max_rate` increases the maximum when the link uses it (see [5G links](configuration.md#5g-links)).
- These results are from one 5G cell. Starlink is not tested yet.

## Resource use: Bash and Go

Measured on a **GL.iNet GL-MT6000** with OpenWrt, 60 seconds for each version, a sample each 5 seconds, with [`benchmark.sh`](../benchmark.sh). An earlier version of cake-autorate-go gave these values. A new measurement with the current version is planned.

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

On a router with 128 to 256 MB of RAM, 23 MB or 11 MB for one daemon is an important difference. Other services (dnsmasq, firewall, VPN) also need memory.

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
