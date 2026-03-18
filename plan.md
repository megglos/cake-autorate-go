# Multi-Interface (Multi-WAN) Support Plan

## Goal

Support multiple WAN links in a single Go process, where the bash version requires
a completely separate process tree (5+ processes) per link.

**Resource comparison for 2 WANs:**
- Bash: 10-12 processes, ~20 MB RSS
- Go (proposed): 1 process, ~11-12 MB RSS (extra goroutines + a few KB state per link)

## Current Architecture

```
main → Controller (1 instance)
         ├─ Monitor (reads DL + UL interface stats)
         ├─ PingerManager (shared reflectors, shared baseline/delta EWMA)
         ├─ Shaper (applies tc commands)
         └─ dirState × 2 (dl, ul — rate, load, delay window, timers)
```

Config has top-level `download:` and `upload:` — exactly one of each.

## Proposed Architecture

```
main → LinkController × N (one per WAN link, each a goroutine)
         ├─ Monitor (reads this link's DL + UL interface stats)
         ├─ PingerManager (this link's reflectors, own baseline/EWMA)
         ├─ Shaper (shared instance, stateless — just runs tc)
         └─ dirState × 2 (dl, ul)
```

Each WAN link is independent: its own interfaces, its own latency path, its own
rate decisions. This maps cleanly to one `LinkController` per link, all running
concurrently in one process.

## Design Decisions

### Each link gets its own PingerManager
Different WAN links route through different paths with different latencies.
Sharing reflectors/baselines across links would corrupt the measurements.
The ping source address and interface binding already exist in config
(`ping_source_addr`, `ping_interface`) — these become per-link.

### Shaper is shared
The Shaper is stateless (just execs `tc`). A single Shaper instance can be
safely used by all links concurrently (no mutex needed — each call is independent).

### Each link gets its own state machine
Idle/Running/Stall transitions depend on that link's throughput and ping health.
One WAN being idle shouldn't affect another.

### Logger is shared
Single log output, but each link prefixes its messages with its link name
(e.g., `[primary]`, `[secondary]`).

## Implementation Steps

### Step 1: Config restructuring

**config.go** — Add a `Link` struct and support a `links:` list:

```yaml
# New config format
links:
  - name: primary
    download:
      interface: ifb4eth1
      min_rate_kbps: 25000
      base_rate_kbps: 200000
      max_rate_kbps: 200000
    upload:
      interface: eth1
      min_rate_kbps: 25000
      base_rate_kbps: 35000
      max_rate_kbps: 35000
    reflectors:
      - 1.1.1.1
      - 8.8.8.8
      - 9.9.9.9
    ping_source_addr: ""
    ping_interface: ""

  - name: secondary
    download:
      interface: ifb4eth2
      ...
    upload:
      interface: eth2
      ...
    reflectors:
      - 1.0.0.1
      - 8.8.4.4
```

- `LinkConfig` struct: `Name`, `Download`, `Upload`, `Reflectors`, `PingSourceAddr`, `PingInterfaceName`
- Top-level `Config` keeps shared settings: timing, thresholds, EWMA alphas, logging, etc.
- **Backward compatibility**: If `links:` is empty but `download:`/`upload:` exist at top level,
  auto-create a single link named "default" from the old-style config. This avoids breaking
  existing deployments.

### Step 2: Rename Controller → LinkController

**controller.go**:
- Rename `Controller` to `LinkController`
- Add `name string` field (e.g., "primary") used as log prefix
- Constructor: `NewLinkController(name string, linkCfg LinkConfig, sharedCfg Config, shaper *Shaper, logger *Logger)`
- Each `LinkController` creates its own `PingerManager` and `Monitor`
- Logger methods become `logger.Infof("[%s] ...", lc.name, ...)`

### Step 3: Add top-level orchestrator in main.go

**main.go**:
- Parse config, create shared Shaper and Logger
- For each `LinkConfig` in `cfg.Links`:
  - `NewLinkController(link.Name, link, cfg, shaper, logger)`
  - Launch `lc.Run(ctx)` in a goroutine
- Wait for all goroutines (use `errgroup` or `sync.WaitGroup`)
- If any link controller exits with an error, log it but keep others running

### Step 4: Update PingerManager to accept per-link config

**pinger.go**:
- `NewPingerManager` takes reflector list, ping source, ping interface from `LinkConfig`
  instead of from the global config
- No structural changes needed — it's already self-contained

### Step 5: Update Monitor to accept per-link interfaces

**monitor.go**:
- Already takes interface names as constructor args — no change needed
- Just wire from `LinkConfig.Download.Interface` / `LinkConfig.Upload.Interface`

### Step 6: Update config.example.yaml and --defaults output

- Show multi-link config example
- Show single-link backward-compatible example
- Document that all links share timing/threshold settings

### Step 7: Update README

- Document multi-WAN support
- Update benchmark comparison (projected numbers for 2+ WANs)

## Files Changed

| File | Change |
|------|--------|
| `config.go` | Add `LinkConfig`, `links:` list, backward compat migration |
| `controller.go` | Rename to `LinkController`, add name prefix, take `LinkConfig` |
| `main.go` | Loop over links, launch goroutines, wait for all |
| `pinger.go` | Constructor takes per-link reflector/ping settings |
| `monitor.go` | Minimal — already parameterized |
| `shaper.go` | No changes |
| `logger.go` | No changes (link name prefixing done in controller) |
| `config.example.yaml` | Multi-link example |
| `README.md` | Document multi-WAN support |

## Migration / Backward Compatibility

Existing single-link configs continue to work unchanged. The migration logic:

```go
if len(cfg.Links) == 0 && cfg.Download.Interface != "" {
    cfg.Links = []LinkConfig{{
        Name:     "default",
        Download: cfg.Download,
        Upload:   cfg.Upload,
        Reflectors: cfg.Reflectors,
        PingSourceAddr: cfg.PingSourceAddr,
        PingInterfaceName: cfg.PingInterfaceName,
    }}
}
```

## Risk / Considerations

- **tc contention**: Multiple links calling `tc` concurrently is fine — they target
  different interfaces. No serialization needed.
- **ICMP socket limits**: Each link spawns its own pingers. On routers with many WANs,
  this increases open ICMP sockets. Unlikely to be a problem for 2-3 links.
- **Log noise**: Multiple links logging concurrently. The name prefix keeps things readable.
  Could add per-link log files later if needed.
