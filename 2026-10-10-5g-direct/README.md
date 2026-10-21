# 5G sessions on the PC, 2026-10-10 and 2026-10-11

Link: 5G modem (Telekom) directly on the PC (`enp7s0`).
`scripts/record-session.sh` ran on the PC with sudo. The binary was the
development version at the commit in `session.txt` of each session.

Configs: `configs/`. `trace-5g.csv` is a capacity trace made from these
sessions. The names of the variants (`autorate-NAME`) are in
`session.txt` of each session.

| Session | Config | Policies | Modes | Rounds |
|---|---|---|---|---|
| `20261010-2138-enp7s0` | `5g.yaml` (RTT) | unshaped, fixed, autorate | dl ul both | 2 |
| `20261010-2241-enp7s0` | `5g.yaml` (RTT) | unshaped, fixed, autorate | dl ul both | 1 |
| `20261010-2326-enp7s0` | `5g-owd.yaml` | unshaped, fixed, autorate | dl ul both | 2 |
| `20261011-0504-enp7s0` | `5g-owd.yaml` | unshaped, fixed, autorate, f2, f1f2 | dl ul both | 2 |
| `20261011-0656-enp7s0` | `5g-owd-f1.yaml` | unshaped, fixed, autorate, rb | dl ul both | 2 |
| `20261011-0808-enp7s0` | `5g-owd-f1.yaml` | unshaped, autorate, f8, x, t15x, t15xf8 | dl ul both | 2 |
| `20261011-1002-enp7s0` | `5g-owd-f1.yaml` | unshaped, autorate, t15, d15u25, d15u30 | dl ul both | 2 |
| `20261011-1245-enp7s0` | `5g-owd-f1.yaml` | unshaped, autorate, learn | dl ul both | 3 |
| `20261011-1413-enp7s0` | `5g-owd-f1.yaml` | unshaped, autorate, learn | both | 4 |
| `20261011-1510-enp7s0` | `5g-owd-f1.yaml` | unshaped, autorate, learn30, learn20 | both | 4 |
| `20261011-1620-enp7s0` | `5g-s5-learn.yaml` | unshaped, autorate, target60 | ul both | 3 |
