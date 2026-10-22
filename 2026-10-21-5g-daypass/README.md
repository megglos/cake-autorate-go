# 5G day pass, 2026-10-21

Link: 5G modem (Telekom) on the host, WAN of the OpenWrt VM. The VM makes
the load (8 curl workers in each direction) and shapes it with CAKE on
`eth1` and `ifb4eth1`. cake-autorate-go 1.0.0-alpha.1-386-g4e997cb.
Original cake-autorate: commit ac75f49, `pinger_method=fping-ts`.

Configs: `configs/`. Each session directory also has the configs that it
used (`config*.yaml`, `upstream-*.sh`).

| Session | Policies | Modes | Block | Rounds |
|---|---|---|---|---|
| `dp1` | saturation check, then unshaped, autorate, lcd, lcdup20 | dl | 60 s | stopped at block 5 (1) |
| `dp2` | unshaped, autorate, lcd, lcdup20, upstream-fts | dl ul both | 60 s | 2 |
| `dp3` | unshaped, autorate, up20, up30, upstream-fts | dl ul both | 120 s | 2 |
| `dp4` | unshaped, autorate, rod, rdr, rodlcd, upstream-fts | dl both | 120 s | 2 |
| `dp5` | unshaped, autorate, rod, rdr | dl both | 120 s | 8 |
| `dp6` | unshaped, autorate, rod, inc, incrod, upstream-fts | dl | 120 s | 12 |
| `dp7` | unshaped, autorate, rod, incrod, rodlcd, upstream-fts | both | 120 s | 12 |
| `dp8` | unshaped, autorate, rod, incrod, upstream-fts | dl ul both | 120 s | 5 (2) |
| `dp11` | unshaped, autorate, pi200, base180 | dl both | 120 s | 3 (3) |
| `dp12` | unshaped, autorate (max 245), amax, max500, incrodamax | dl both | 120 s | 3 (4) |
| `dp11b` | unshaped, autorate, pi200, base180 | dl both | 120 s | 3 (5) |
| `dp10` | unshaped, autorate, rod, incrod, upstream-fts | dl both | 120 s | 5 (4) |
| `dp9` | unshaped, autorate, srm0, rod, rodsrm0 | dl both | 120 s | 5 (4) |

(1) The config of the original had integer delay thresholds. The original
accepts only float values (`36.0`).

(2) Stopped at block 77 of 90: the VM root file system was full. At night
the cell gave 420 Mbit/s DL and 141 Mbit/s UL unshaped. This is more than
the maximum rates of the configs (245000 and 110000 kbit/s), thus all
shaped policies were at the maximum rate and are equal. Sessions after dp8
use the configs in `configs-max500` (maximum rates 500000 and 160000 kbit/s,
all other values unchanged).

(3) Stopped after round 3 to run dp12 at night. Session dp11b has the
remaining rounds.

(4) Exploration build 1.0.0-alpha.1-zzamax (local branch, not published).
It adds the config key `zz_auto_max`: while the download rate is at the
maximum rate for 2 s, the achieved rate is at least 90 % of it and there
is no bufferbloat, the maximum increases by 10 % (up to 3 times the
configured value). At bufferbloat, the maximum decreases to 1.5 times the
rate of the decrease, not below the configured value. Without the key, the
build is equal to the build of dp6 to dp11. Session `autorate` here uses
`5g-max245.yaml` (maximum rates 245000 and 110000 kbit/s), `max500` uses
`5g.yaml`. dp11b, dp10 and dp9 use this build too.

(5) The rounds 4 to 6 of dp11, after dp12 (build of note 4, without
`zz_auto_max`).

Policy names:

- `autorate`: `5g.yaml` (defaults, `stall_recovery_ms` 500).
- `lcd`: `learn_cross_delay: true`.
- `lcdup20`: `lcd` and upload `avg_owd_delta_max_adjust_up_thr_ms: 20`.
- `up20`, `up30`: `avg_owd_delta_max_adjust_up_thr_ms` 20 or 30 ms in the
  two directions.
- `rod`: `refractory_on_decrease: true`.
- `rdr`: `repeat_decrease_on_rise: true`.
- `rodlcd`: `rod` and `lcd`.
- `inc`: `increase_to_delay_thr: true` (branch `feat/increase-to-threshold`,
  build 1.0.0-alpha.1-395-g6e8dbca-inc; dp6 and the sessions after it use
  this build for all policies).
- `incrod`: `inc` and `rod`.
- `srm0`: `stall_recovery_ms: 0` (no stall recovery).
- `rodsrm0`: `rod` and `srm0`.
- `pi200`: `ping_interval_ms: 200`.
- `base180`: download base rate 180000 kbit/s.
- `amax`: `5g-max245.yaml` and `zz_auto_max: true`.
- `incrodamax`: `incrod`, `amax`.
- `max500`: `5g.yaml` (maximum rates 500000 and 160000 kbit/s).
- `upstream-fts`: the original cake-autorate.

Saturation check (dp1): dl 169/234/248 Mbit/s with 4/8/16 workers, ul
96/94/94 Mbit/s.
