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

(1) The config of the original had integer delay thresholds. The original
accepts only float values (`36.0`).

Policy names:

- `autorate`: `5g.yaml` (defaults, `stall_recovery_ms` 500).
- `lcd`: `learn_cross_delay: true`.
- `lcdup20`: `lcd` and upload `avg_owd_delta_max_adjust_up_thr_ms: 20`.
- `up20`, `up30`: `avg_owd_delta_max_adjust_up_thr_ms` 20 or 30 ms in the
  two directions.
- `rod`: `refractory_on_decrease: true`.
- `rdr`: `repeat_decrease_on_rise: true`.
- `rodlcd`: `rod` and `lcd`.
- `upstream-fts`: the original cake-autorate.

Saturation check (dp1): dl 169/234/248 Mbit/s with 4/8/16 workers, ul
96/94/94 Mbit/s.
