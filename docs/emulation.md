# Link emulation

The link emulation makes a variable link (for example 5G) on a fast and
stable connection (for example fiber). Use it to test the controller with
real TCP traffic, real CAKE and real bufferbloat, without a 5G modem or a
Starlink dish.

The emulation does not replace recordings of real links. It shows how the
controller operates when the capacity changes as the trace specifies. A real
link can have other behavior (for example a different queue size in the
modem, packet loss, or a change of the base RTT).

## Function

`scripts/emulate-link.sh` makes this setup:

```
emu-router namespace                root namespace ("modem")
load-gen, cake-autorate-go,         download bottleneck: tbf + netem on
CAKE on emu-wan            ──veth── emu-modem egress
                                    upload bottleneck: tbf + netem on
                                    ifb-emu-ul (emu-modem ingress)
                                    NAT ──► UPLINK ──► internet
```

- The namespace `emu-router` operates as the router. It runs CAKE,
  cake-autorate-go and the load generator.
- The root namespace operates as the modem. Each direction has a bottleneck
  with a large queue (default: 1000 packets), as a 5G modem has. Thus, a
  load causes bufferbloat.
- netem adds an extra base RTT (default: 20 ms).
- A trace player changes the bottleneck rates as the capacity trace
  specifies. `tc qdisc change` keeps the packets in the queue.
- At the start of each block, the trace starts again from t=0. Thus, each
  policy (unshaped, fixed, autorate) gets the same capacity changes.
- The script writes the applied capacity to `DIR/capacity.tsv`.
- Each second, the script writes counters to `DIR/stats.tsv`: the queue
  size (packets) and the drops of the two bottlenecks, and the entries,
  invalid packets, drops and insert failures of conntrack. Use them to find
  the cause of a link outage in a block.
- During the emulation, the script turns off GRO on the uplink, and turns
  it on again at the end. With GRO, the download bottleneck gets packets of
  up to 64 KB. tbf does not send a packet that is larger than its burst.
  When the trace decreases the rate (and thus the burst), the queue stops
  until the rate increases again. This causes link outages that are not in
  the trace.
- The script calls `scripts/record-session.sh` for the test session.
- At the end, or after Ctrl+C, the script removes the namespace, the
  interfaces and the iptables rules.

## Requirements

- Linux with the kernel modules `sch_cake`, `sch_tbf`, `sch_netem`, `ifb`
  and `act_mirred`.
- bash, iproute2, iptables, ethtool, curl, awk, jq, gzip.
- Root access.
- A real connection that is much faster than the trace. Keep the maximum
  of the trace at 70 % or less of the real capacity. If not, the real
  connection is also a bottleneck, and the results are not correct.
- Build the binary first: `go build .`

## Procedure

1. Make a capacity trace:

   ```
   ./scripts/gen-trace.sh -d 300 -r 1 -D 30000,400000 -U 5000,80000 > recordings/trace-5g.csv
   ```

   The trace is a random walk between the minimum and the maximum, with
   deep fades of 1 to 3 s to the minimum. The same seed (`-r`) gives the
   same trace. For all options, see `./scripts/gen-trace.sh -h`.

   The trace format is CSV with the header `t_ms,dl_kbps,ul_kbps`. Each
   line gives the capacity from `t_ms` until the next line. You can also
   use a trace from a different source in this format.

2. Start the emulation and the session:

   ```
   sudo ./scripts/emulate-link.sh -u enp7s0 -t recordings/trace-5g.csv -- -r 1 -m "dl ul" -F 85
   ```

   - `-u` is the interface of the real connection.
   - The options after `--` go to `scripts/record-session.sh`. The script
     already gives `-i`, `-c`, `-o`, `-s`, `-w 8` and `-C`.
   - With `-r 1 -m "dl ul"` and the default block time (150 s), the
     session has 6 blocks and takes approximately 17 minutes.
   - Do not use the connection for other traffic during the session.

3. Show the summary:

   ```
   ./scripts/summarize-session.sh recordings/<date>-emu
   ```

If the script stops before the cleanup (for example after a crash), remove
the old setup:

```
sudo ./scripts/emulate-link.sh -k
```

## Controller configuration

If you do not give `-c CONFIG`, the script makes `DIR/emu-config.yaml` from
the trace. The rates are what a user can know about a real link:

| Value | From the trace |
|---|---|
| `min_rate_kbps` | 80 % of the 5th percentile |
| `base_rate_kbps` | median |
| `max_rate_kbps` | maximum |

All other values are the defaults. To test other values, copy the file,
change it and give it with `-c`. The interfaces must stay `ifb4emu-wan`
(download) and `emu-wan` (upload).

## Summary values

`scripts/summarize-session.sh` shows one line for each block. It uses the
loaded time of the block without the first 10 s (TCP ramp-up).

| Column | Meaning |
|---|---|
| excess RTT p50/95/99 | The RTT of each ping minus the minimum RTT of the same reflector in the block, in ms. This is the bufferbloat. |
| dl, ul Mbit/s | The mean throughput of the loaded direction. |
| % ref | The throughput as a percentage of the mean capacity of the trace in the same time. |

A good controller has a low excess RTT and a high % ref. The two values
are a trade-off: a low fixed shaper rate gives a low excess RTT but a low
throughput. No shaper gives a high throughput but a high excess RTT.
Always compare the two values.

## Limits

- The queue size of a real modem is not known. A different queue size
  changes the maximum bufferbloat. Change it with `-q`.
- The emulation has no random packet loss and no change of the base RTT.
- The servers of the load generator are on the internet. Their
  throughput can change. Thus, repeat a session to compare two versions of
  the controller.
