# Recordings of cake-autorate-go

This branch keeps the raw data of live test sessions. It has no code. The
sessions come from `scripts/record-session.sh` and
`scripts/openwrt-vm-session.sh` on the `main` branch.

Each block of a session has these files:

- `NN-MODE-POLICY.jsonl.gz`: the recording of cake-autorate-go (`--record`).
  It has the pings, the achieved rates and the shaper rates.
- `NN-MODE-POLICY.log`: the log of cake-autorate-go and of the load.
- `NN-MODE-upstream-NAME.upstream/`: the config and the log of the original
  cake-autorate (only for the policy `upstream-NAME`).

Each session also has `blocks.tsv` (the times of the blocks),
`session.txt` (the settings) and the configs that it used.

To show the results of a session, use:

    scripts/summarize-session.sh DIR

To make a capacity trace for the simulator, use
`scripts/trace-from-session.sh`.

## Sessions

| Directory | Link | Content |
|---|---|---|
| `2026-10-21-5g-daypass` | 5G (Telekom), OpenWrt VM | cake-autorate-go variants against the original cake-autorate |
