# Recording

With the `--record` option, cake-autorate-go writes all controller events to a file. Use a recording to:

- Replay a real session through the controller. The replay gives the same decisions as the real session.
- Compare the link simulator with a real link, and calibrate the simulator.
- Make test traces for the tuning of the controller.

## Options

| Option | Description |
|---|---|
| `--record FILE` | Write all controller events to `FILE` (JSONL). If `FILE` ends with `.gz`, the program writes gzip. |
| `--record-max-mb N` | Stop the recording when the file gets to `N` MB. The controller continues. `0` (default) is no limit. |
| `--no-adjust` | Do not change the shaper rates. This option overrides `adjust` in the config. Use it with `--record` to measure a link without autorate. |

The recording does not change the behavior of the controller.

## File size

A link with load gives approximately 40 events each second (pings, throughput samples and shaper changes).

| Format | Size for each hour |
|---|---|
| `.jsonl` | approximately 13 MB |
| `.jsonl.gz` | approximately 1.5 MB |

On a router with little storage, use `.gz` and `--record-max-mb`. Do not write the file to flash memory for a long time. Write it to `/tmp` (RAM) or to a USB drive.

## File format

Each line is one JSON object. The first line is the header. The header contains the format version, the program version, the start time (wall clock, RFC 3339) and the full config.

All other lines have these fields:

- `t`: the time of the event in microseconds after the start. The value uses the monotonic clock. Thus, a change of the wall clock (for example from NTP) does not change it.
- `type`: the event type.
- `link`: the name of the link.

| Type | Fields | Description |
|---|---|---|
| `start` | — | The controller started for the link. |
| `ping` | `reflector`, `rtt_us`, `timeout` | A ping reply or a ping timeout. |
| `rate` | `dl_kbps`, `ul_kbps` | A sample of the measured throughput. |
| `reflector_reset` | `reflector` | The controller removed the delay data of a reflector (a spare reflector became active). |
| `shaper` | `dir`, `rate_kbps` | The controller set a shaper rate. |
| `state` | `state` | The controller changed its state (`RUNNING`, `IDLE`, `STALL`). |

The `start`, `ping`, `rate` and `reflector_reset` events are the inputs of the controller. The `shaper` and `state` events are the decisions of the controller.

When `adjust` is `false` for a direction, the controller does not set the shaper rate for that direction. Thus, the recording has no `shaper` events for that direction.

## Procedure: record a 5G link

Do two types of recordings:

1. **Link recordings without autorate.** They show the capacity and the bufferbloat of the link.
2. **Autorate recordings.** They show the behavior of the controller on the link.

### Equipment

- A router with CAKE on the 5G link, and cake-autorate-go. If you do not have a router, use a Linux PC that connects directly to the modem. See [Record from a PC without a router](#record-from-a-pc-without-a-router).
- A PC on the LAN of the router, with `load-gen.sh` (in this repository) and `curl`.
- Optional: access to the signal data of the modem (RSRP, SINR, band, cell ID).

### Preparation

1. Make sure that no other device uses the link during the test.
2. Write down the modem model, the provider, the location and the time of each recording.
3. Make sure that the clock of the router is correct (NTP).

### Link recordings without autorate

For these recordings, the shaper must not limit the link. Thus, the measured throughput is the capacity of the link.

1. In the config, set `adjust: false` for download and upload.
2. Set the CAKE bandwidth of the two interfaces to `unlimited`:

   ```sh
   tc qdisc change dev <upload-interface> root cake unlimited
   tc qdisc change dev <download-interface> root cake unlimited
   ```

3. Start the recording:

   ```sh
   cake-autorate-go --config <config> --record /tmp/5g-link-<date>.jsonl.gz
   ```

4. On the PC, do these steps one after the other. Use 5 minutes for each step:

   | Step | Command | Measures |
   |---|---|---|
   | a | No load. Wait 5 minutes. | Base RTT and jitter without load |
   | b | `./load-gen.sh -m dl -d 300` | Download capacity and download bufferbloat |
   | c | `./load-gen.sh -m ul -d 300` | Upload capacity and upload bufferbloat |
   | d | `./load-gen.sh -m both -d 300` | The two directions together |

5. Stop cake-autorate-go with Ctrl+C. The program writes all events to the file before it stops.
6. Set the CAKE bandwidth back to the usual values.

### Autorate recordings

1. Use your usual config, with `adjust: true`.
2. Start the recording:

   ```sh
   cake-autorate-go --config <config> --record /tmp/5g-autorate-<date>.jsonl.gz
   ```

3. Do the same steps (a to d) as for the link recordings.
4. Stop cake-autorate-go with Ctrl+C.

### Repeat at different times

The capacity of a 5G cell changes during the day. If possible, do the recordings at these times:

- In the morning or at midday (low use of the cell).
- In the evening (high use of the cell).

### Optional: signal data

If the modem uses ModemManager, record the signal data each second in a different file:

```sh
mmcli -m 0 --signal-setup=1
while true; do
  echo "$(date +%s.%N) $(mmcli -m 0 --signal-get --output-keyvalue | tr '\n' ' ')"
  sleep 1
done > /tmp/5g-signal-<date>.txt
```

For other modems, use the tool of the modem manufacturer. Make sure that each line has a timestamp.

A modem that connects through Ethernet (a 5G CPE or a router with a 5G modem) does not use ModemManager. Get the signal data from the web interface or from the API of the modem. If this is not possible, write down the signal values from the web interface at the start and at the end of each recording.

### Record from a PC without a router

You can connect the modem directly to a Linux PC (USB or Ethernet). The PC then does the work of the router: it runs CAKE, cake-autorate-go and the load generator.

#### Requirements

- Linux with the kernel modules `sch_cake`, `ifb` and `act_mirred`.
- `iproute2` (`tc`), `curl`, root access.
- cake-autorate-go (`go build .` in this repository).

#### Preparation

1. Find the interface of the modem:

   ```sh
   ip -br link
   ```

   Examples: `wwan0` (QMI or MBIM), `usb0` or `enx...` (RNDIS or ECM), `enp...` (Ethernet).

2. Make sure that all internet traffic of the PC goes through the modem. Disconnect the other networks (Wi-Fi, Ethernet), or give the route of the modem the lowest metric. Then, make sure that this command shows the modem interface:

   ```sh
   ip route get 1.1.1.1
   ```

3. Set up CAKE. For link recordings without autorate, use no rates (the default is `unlimited`):

   ```sh
   sudo ./scripts/cake-pc.sh up <modem-interface>
   ```

   For autorate recordings, give the start rates for download and upload:

   ```sh
   sudo ./scripts/cake-pc.sh up <modem-interface> 200mbit 30mbit
   ```

   The script shows the interface names for the config. The download interface is `ifb4<modem-interface>`.

4. Write a config with these interface names. Set `ping_interface` to the modem interface. Example for the interface `enp7s0`:

   ```yaml
   links:
     - name: test
       download:
         interface: ifb4enp7s0
         adjust: true
         min_rate_kbps: 20000      # change these rates for your link
         base_rate_kbps: 200000
         max_rate_kbps: 600000
       upload:
         interface: enp7s0
         adjust: true
         min_rate_kbps: 5000
         base_rate_kbps: 30000
         max_rate_kbps: 100000
       ping_interface: enp7s0
   log_to_file: false
   ```

   The other settings and the reflectors use the default values. To see them, run `./cake-autorate-go --defaults`.

#### Recordings with the session script (recommended)

`scripts/record-session.sh` does all steps automatically. It measures the bufferbloat and the throughput with different shaper policies:

| Policy | CAKE bandwidth | Shows |
|---|---|---|
| `unshaped` | unlimited | The capacity of the link (the bandwidth reference) and the bufferbloat without a shaper |
| `fixed` (optional, `-F`) | a fixed percentage of the capacity | A static shaper |
| `autorate` | set by cake-autorate-go | The result of the controller |

The goal of autorate is low bufferbloat and a throughput near the capacity. Thus, compare `autorate` with `unshaped` for the throughput, and with `unshaped` and `fixed` for the bufferbloat.

The capacity of a link can change during the session. Thus, the script alternates the policies in short blocks. Each block has a time without load (for the base RTT) and a time with load.

Before the blocks, the script does a saturation check. It applies the load with 4, 8 and 16 workers, and finds the number of workers that fills the link. If the throughput still increases from 8 to 16 workers, the script shows a warning. Then, the link is possibly not full, and the measured capacity is too low.

1. Build the binary: `go build .`
2. Do the steps in [Preparation](#preparation-1). You do not have to run `cake-pc.sh up`. The session script does it.
3. Start the session:

   ```sh
   sudo ./scripts/record-session.sh -i <interface> -c <config> -F 85
   ```

   With the default values, the session has 2 rounds x 3 load modes (`dl`, `ul`, `both`) x 3 policies = 18 blocks. It takes approximately 55 minutes. For a shorter session, use `-r 1` or `-m "dl ul"`.

4. Do not use the network during the session.
5. The script writes all files to `recordings/<date>-<interface>/`:
   - `session.txt`: the settings, the saturation check and system data.
   - `blocks.tsv`: one line for each block, with the start and end times of the load (Unix time).
   - `NN-MODE-POLICY.jsonl.gz`: the recording of each block.
   - `NN-MODE-POLICY.log`: the log of cake-autorate-go and load-gen.sh.

If you stop the script with Ctrl+C, it stops all programs and removes CAKE.

#### Test the procedure on a different connection first

Before you record the 5G link, do a short session on the usual connection of the PC (for example, fiber through the home router). This makes sure that the tools operate correctly:

```sh
sudo ./scripts/record-session.sh -i <interface> -c <config> -r 1 -m "dl ul" -b 60 -F 85
```

This session takes approximately 10 minutes. On a connection through a home router, the bottleneck can be the home router or the ISP. If the home router has a shaper (SQM, QoS), the `unshaped` blocks measure that shaper.

#### Manual recordings

You can also do the steps in [Link recordings without autorate](#link-recordings-without-autorate) and [Autorate recordings](#autorate-recordings) manually. Run `load-gen.sh` on the same PC, with `-i <interface>` to show the throughput. Do not do the `tc qdisc change` commands. `cake-pc.sh` sets the CAKE bandwidth. For the link recordings, use `cake-autorate-go --no-adjust`. Then you do not have to change `adjust` in the config.

When you complete the recordings, remove CAKE:

```sh
sudo ./scripts/cake-pc.sh down <modem-interface>
```

#### Differences from a router

- On a PC, the load generator and CAKE are on the same computer. For upload, the TCP sender gets back pressure from the local queue. On a router, the sender is on a different computer and CAKE drops packets. Thus, the upload behavior with autorate can be a little different.
- The download behavior is the same as on a router.
- If the PC has only one network interface, all traffic of the PC goes through the modem during the test. Stop all other programs that use the network (browser, cloud sync, updates). Their traffic changes the measurements.
- The link recordings without autorate are not affected. They measure the modem and the 5G network.

### Send the recordings

Send these items:

- The recording files (`.jsonl.gz`).
- The signal files (optional).
- The notes from the preparation (modem, provider, location, times).

The recordings contain the IP addresses of the reflectors and the config. They do not contain the contents of your traffic.
