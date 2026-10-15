# Plan: OpenWrt package and LuCI app

On the branch `plans`, like PLAN.md. This is roadmap step 9 in PLAN.md. It runs **in parallel** with the core tuning and the comparison with upstream (step 8), in a separate session (user decision 2026-10-15; before: last step after step 8). darkmoon is not in the comparison (user decision 2026-10-12), but its packaging stays a reference here.

## 1. Goals

1. Install cake-autorate-go on OpenWrt with one package (`apk add`), without SSH edits.
2. Configure all relevant parameters in LuCI, for **any number of links**.
3. Show the live state of each link (rates, delays, state) in LuCI.
4. Calibration: one button for each link. It measures the link, shows the proposed values, and applies them on confirmation.
5. Enable and disable autorate for each link, and for each direction of a link, from the UI (user request 2026-10-11).
6. Keep the YAML config as the single format of the program. The tests, the simulator and the recordings do not change.

Out of scope (for now): a graph with history, Starlink-specific UI before step 5 is done, other platforms than OpenWrt.

## 2. Reference: darkmoon

github.com/kamikaonashi/openwrt-package-darkmoon (MIT, C, commit 5434c96, 2026-03-14). A C port of upstream for OpenWrt. Its packaging is a good model:

| Part | darkmoon | Use for us |
|---|---|---|
| Config | UCI `/etc/config/darkmoon`, one `config darkmoon '<name>'` section for each instance | Yes, with a typed section for each link |
| Service | procd, one instance for each section, reload trigger on UCI change | Partly: we run **one process for all links** (our multi-link design). One procd instance. |
| LuCI view | `view/darkmoon.js`, `form.NamedSection` with tabs General, CAKE Qdisc, Advanced, Reflector Health | Same tabs idea, but `form.GridSection` / `TypedSection` with add/remove for links |
| Status | Daemon writes `/var/run/darkmoon.json` every 200 ms. `status/include/75_darkmoon.js` shows it on the Overview page (poll 2.5 s) | Yes, same pattern |
| ACL | `rpcd/acl.d/luci-app-darkmoon.json`: uci read/write, file read of the status JSON, `luci.setInitAction` | Yes, plus the calibration call |
| CAKE setup | The daemon creates IFB and CAKE (also cake-mq) | Optional phase (see 9) |
| Build | GitHub workflow builds `.apk` | Yes |

Limit of darkmoon's UI: it shows the fixed section `primary`. It cannot add or remove links in the UI. We must do better here.

Credit darkmoon in the README if we copy structure or code (MIT: keep the copyright note for copied parts).

## 3. Architecture

```
LuCI (browser)
  │  view/cake-autorate/config.js   (form, UCI)
  │  view/cake-autorate/status.js   (poll status JSON)
  │  view/cake-autorate/calibrate.js
  ▼
rpcd ── uci ──► /etc/config/cake-autorate
  │                 │
  │                 ▼  init script (on start/reload)
  │            /var/etc/cake-autorate.yaml  (generated)
  │                 │
  │                 ▼
  │            cake-autorate-go -config /var/etc/cake-autorate.yaml   (procd, 1 instance)
  │                 │
  │                 ├─► /var/run/cake-autorate/status.json   (every 1 s)
  │                 └─► /var/run/cake-autorate/calibrate-<link>.json
  │
  └── ucode rpc plugin `cake-autorate` (calibrate start/status, validate)
```

### Decision: UCI → YAML generation, not UCI in Go

| Option | Advantages | Disadvantages |
|---|---|---|
| A. Init script generates YAML from UCI | Program and tests stay the same. YAML stays the reference. Also works without LuCI. | Two formats. A converter to maintain. |
| B. Go reads UCI directly | One format on OpenWrt | Needs a UCI parser in Go or libuci (cgo). Breaks the "same binary everywhere" design. |

Decision: **A**. Write the converter in **ucode** (OpenWrt standard, has a UCI and JSON API). It is easier to test than shell. The converter is small: one key = one value.

Also add `cake-autorate-go -check-config FILE`. The converter result is validated before the restart. Errors (also unknown keys, see PR #12) go to the LuCI notification.

## 4. UCI schema

File `/etc/config/cake-autorate`. Option names = YAML keys. Then the converter needs no table.

```
config global 'global'
	option enabled '1'
	option ping_method 'icmp-timestamp'
	option pinger_count '6'
	option ping_interval_ms '300'
	option learn_cross_delay '0'
	option refractory_on_decrease '0'
	option enable_sleep_function '1'
	option debug '0'
	# ... all global keys (section 5, Advanced)

config link 'wan5g'
	option enabled '1'
	option name '5g'
	option ping_interface 'wan'
	list reflectors '1.1.1.1'
	# download
	option dl_interface 'ifb4wan'
	option dl_adjust '1'
	option dl_min_rate_kbps '30000'
	option dl_base_rate_kbps '150000'
	option dl_max_rate_kbps '400000'
	option dl_owd_delta_delay_thr_ms '30'
	option dl_avg_owd_delta_max_adjust_up_thr_ms '10'
	option dl_avg_owd_delta_max_adjust_down_thr_ms '60'
	# upload: same keys with ul_
	option ul_interface 'wan'
	...
```

- UCI has no nested sections. Thus the direction keys get the prefix `dl_` / `ul_`. The converter maps them to `download:` / `upload:`.
- An empty option = the program default. The converter does not write it. Thus a later change of a default also applies to old configs.
- `enabled '0'` on a link: the converter leaves out the link. If no link is enabled: the service does not start.

### Open question: global keys that must be per link

Some keys are global in `Config`, but on a multi-link router (for example 5G + fiber) they need different values:

| Key | Why per link |
|---|---|
| `ping_method` | Fiber reflectors may not answer timestamps; 5G needs timestamps |
| `learn_cross_delay` | Useful on 5G; on starlink-like it costs p95 (+8 ms in the simulator) |
| `pinger_count`, `ping_interval_ms` | Data volume on metered links |
| thresholds, rate factors | Different link types |

Proposal: before the UI work, add per-link overrides in Go (`links[].overrides` or direct keys in the link, with a fallback to the global value). Then the UI shows these keys in the link section with the placeholder "global value". This is a core change: do it in step 5/6 if the Starlink work needs it, else at the start of step 9.

## 5. LuCI pages

Menu: **Services → CAKE Autorate (Go)**, with three tabs (LuCI menu entries):

### 5.1 Configuration

**Section "Service"** (`NamedSection global`), tabs:

| Tab | Fields |
|---|---|
| General | enabled, ping_method (list: icmp-timestamp, icmp, fping, tsping), pinger_count, ping_interval_ms, learn_cross_delay, refractory_on_decrease |
| Idle and stall | enable_sleep_function, connection_active_thr_kbps, sustained_idle_sleep_thr_s, min_shaper_rates_enforcement, stall_detection_thr, connection_stall_thr_kbps, global_ping_response_timeout_s |
| Advanced | bufferbloat_detection_window/thr, alpha_* , shaper_rate_*, high_load_thr, monitor_interval_ms, bufferbloat_refractory_period_ms, decay_refractory_period_ms, reflector_* , startup_wait_s |
| Logging | log_to_file, log_file_path, log_file_max_size_kb, debug |

Advanced tab: a note "change only if you know why; use calibration".

**Section "Links"** (`GridSection link`, `addremove = true`, `anonymous = false`, `sortable = true`):

- Grid row (overview): enabled, name, DL interface, UL interface, DL base/max, UL base/max, Calibrate button.
- Edit modal (pencil button) with tabs:

| Tab | Fields |
|---|---|
| Interfaces | name, dl_interface (widget `network.DeviceSelect`, also IFB), ul_interface, ping_interface, ping_source_addr, dl_adjust, ul_adjust |
| Rates | dl/ul min, base, max (kbit/s; show Mbit/s next to it) |
| Delay | dl/ul owd_delta_delay_thr_ms, avg_owd_delta_max_adjust_up_thr_ms, avg_owd_delta_max_adjust_down_thr_ms |
| Reflectors | `DynamicList reflectors`, button "use default list for the ping method" (empty list = defaults) |
| Overrides | per-link keys (section 4, if implemented) |

Validation in the browser (`o.validate`), the same rules as `Config.Validate`:
- min ≤ base ≤ max, all > 0.
- avg_owd_delta_max_adjust_up_thr_ms < owd_delta_delay_thr_ms (PR #18 review).
- Unique link name and unique interfaces over all links.
- bufferbloat_detection_thr between 1 and the window.
- On 5G hint (not an error): UL delay threshold < 25 ms → warning "below the 5G grant delay, upload can collapse" (finding of sessions 3 and 4, PR #22).

After Save & Apply: the converter runs, `-check-config` validates, procd restarts the service. On an error: LuCI shows the message, the old YAML stays.

### 5.1b Autorate on/off for each link

User request: control autorate for each link from the UI. Two levels:

| Level | Effect | Where |
|---|---|---|
| Link `enabled` | Off: the program does not run a controller for this link. CAKE keeps the last rate (or the base rate, option). | Grid row toggle (`form.Flag`, `editable = true`), saved in UCI, applied with Save & Apply (restart) |
| Direction `dl_adjust` / `ul_adjust` | Off: Core measures, but does not change the CAKE rate of this direction (exists: `adjust`). | Link modal, tab Interfaces; also in the grid |
| Runtime pause (no restart) | Button "Pause / Resume" on the status card of a link. The shaper goes to the base rate (or stays) and Core stops the adjustments, until "Resume" or a restart. Not saved in UCI. | Status page |

The runtime pause needs a control channel into the daemon (Go change, phase L0):
- A control file or a Unix socket `/var/run/cake-autorate/control.sock` with the commands `pause LINK [dl|ul]`, `resume LINK [dl|ul]`, `status`. The ucode rpc plugin sends the commands. The ACL allows them only for the LuCI admin.
- In Core: a `paused` flag for each direction. Core still processes pings (baselines stay current), but `applyRate` sends nothing. On resume: start from the base rate (safe), then normal control.
- The status JSON shows `paused` for each direction. The status card shows it.
- The recorder records pause and resume events (replay stays deterministic).

### 5.2 Status

Page with one card for each link (poll every 2 s through `fs.read` of the status JSON, or an rpc call):

| Field | Source |
|---|---|
| State | running / idle / stall |
| DL / UL shaper rate, achieved rate, load % | Core state |
| DL / UL delay delta (window average), baseline | Core |
| Bufferbloat count in the window | Core |
| Learned UL cross delay | Core (if learn_cross_delay) |
| Reflectors: address, last RTT, baseline, misbehaving count | Pinger / Core |
| Rate changes in the last minute | Counter |

Optional: a small sparkline of the last 60 s (shaper rate and delta), from a ring buffer in the status JSON. No external chart library: LuCI has none; use SVG.

Overview widget `status/include/70_cake-autorate.js`: one row for each link (state, DL/UL shaper, DL/UL delta), like darkmoon.

### 5.3 Calibration

See section 7.

## 6. Status interface of the daemon (Go change)

New option `status_file` (default on OpenWrt: `/var/run/cake-autorate/status.json`, empty = off).

- Write every 1 s (not 200 ms like darkmoon: less flash and CPU use; /var/run is tmpfs anyway). Atomic: write to `.tmp`, then rename.
- Content: version, start time, for each link the fields of 5.2. Format with a `schema` number for compatibility.
- The writer reads a snapshot of Core under the existing lock. Core stays without I/O (the controller goroutine writes the file).
- Remove the file on a clean stop (the UI then shows "stopped").

Alternative: a ubus object (`ubus call cake-autorate status`). Needs libubus (cgo) or a ubus protocol in Go. Not now; the JSON file is enough.

## 7. Calibration button

Depends on roadmap step 6b (`cake-autorate-go calibrate`). The UI part:

1. Grid row button "Calibrate" (also on the link edit modal).
2. Dialog: warning text: "The calibration saturates the link for approximately N minutes and uses approximately X GB (5G: 3 to 5 GB). The shaping of this link is off during the calibration." Fields: duration (short / normal), and "measure the upload delay from download activity" (default on).
3. Start: rpc `cake-autorate.calibrate_start {link}`. The ucode plugin starts `cake-autorate-go calibrate -link NAME -out /var/run/cake-autorate/calibrate-NAME.json` in the background (procd instance `calibrate`, so that it does not stop on browser close). The main daemon stops shaping this link during the run (signal or a control file), or the service stops (simpler, first version).
4. Progress: rpc `calibrate_status {link}` every 2 s: phase (idle RTT, saturation DL, saturation UL, both, cross delay), percent, partial results. Cancel button → `calibrate_cancel`.
5. Result: table "current value | proposed value | measured data" for each key (rates, thresholds, reflectors, ping method, learn_cross_delay). A checkbox for each row (default: all on).
6. Apply: write the selected values to UCI for this link (`uci.set` in the view, then `ui.changes.apply`). The usual LuCI changes flow: the user sees the diff and can revert.
7. Keep the last result file. The dialog can show it again ("last calibration: date").

Safety:
- Only one calibration at a time (lock file).
- Hard limit for the duration and the data volume. Stop on a stall.
- The calibration needs load servers. Default: the same servers as `load-gen.sh`. Configurable in the global section (`calibration_servers`).

## 8. Package

Two packages, as usual on OpenWrt:

| Package | Content |
|---|---|
| `cake-autorate-go` | binary `/usr/sbin/cake-autorate-go`, `/etc/init.d/cake-autorate`, `/etc/config/cake-autorate` (default: disabled, one example link), ucode converter `/usr/share/cake-autorate/uci2yaml.uc`, `-check-config` |
| `luci-app-cake-autorate-go` | views, status include, menu.d, acl.d, ucode rpc plugin `/usr/share/rpcd/ucode/cake-autorate.uc` |

- Build: OpenWrt SDK with `golang-package.mk` (feeds/packages/lang/golang), or a prebuilt Go binary in the package (simpler; `GOARCH=arm64` for the GL-MT6000, also `amd64`, `arm`, `mipsle`). Start with prebuilt binaries in a GitHub workflow: build per arch, then `apk` packaging with the SDK.
- Package format: OpenWrt 25.12 uses `apk`. Older releases use `opkg/ipk`. Build both if the effort is small.
- Dependencies: `kmod-sched-cake`, `kmod-ifb` (if the package sets up CAKE), `ucode`, `ucode-mod-uci`, `ucode-mod-fs`. `luci-base` for the LuCI app.
- Migration: if `/etc/cake-autorate/config.yaml` from `setup.sh` exists, a one-time script converts it to UCI (or tells the user). Then `setup.sh` is only for non-package installs.
- Init script: `procd_add_reload_trigger cake-autorate`, `procd_set_param respawn`, `procd_add_interface_trigger` for the WAN interfaces (restart when an interface comes up; the netlink retry from PR #13 also helps).

## 9. CAKE setup (optional phase)

Today the user must set up CAKE (for example with sqm-scripts). darkmoon sets up IFB and CAKE itself.

Options:
1. Keep sqm-scripts. LuCI shows a check: "CAKE is not on interface X" with a link to the SQM page. Lowest risk.
2. Own CAKE setup in the init script (tc commands, like `scripts/cake-pc.sh`), with the CAKE options per link (overhead, mpu, nat, wash, diffserv, ack-filter, flow mode, cake-mq on 25.12+).

Decision: first 1, with the check. 2 later if users ask; then a new tab "CAKE" in the link modal.

## 10. Test environment (no router at hand)

- OpenWrt x86-64 image in QEMU/KVM: real OpenWrt kernel (CAKE, IFB, netlink), procd, rpcd, UCI, LuCI, apk. Two virtual NICs (WAN = user net or tap, LAN). LuCI through hostfwd (host 8080 → VM 80), SSH through hostfwd 2222 → 22.
- Host preparation (user, with sudo): `sudo modprobe kvm_amd` (CPU has svm, module not loaded on 2026-10-11) and `sudo pacman -S qemu-base`. If /dev/kvm does not appear: SVM is off in the BIOS.
- A script `scripts/openwrt-vm.sh` (download the image with a checksum, resize, start with the port forwards, copy and install the packages over SSH).
- Optional: OpenWrt armsr/armv8 image with `qemu-system-aarch64` (no KVM, slow) to test the arm64 binary.
- Docker (`openwrt/rootfs`) is lighter, but uses the host kernel: only for UI work.
- Multi-link test in the VM: two WAN NICs, each with CAKE, and a `netem` delay/rate on the host side (tap) to make one "5G-like" link. The same setup can serve roadmap step 7 (CI emulation).

Tests:
- Converter: ucode unit tests (UCI fixture → expected YAML), run in CI with the ucode host tool or in the VM.
- Go: `-check-config` and the status writer (unit tests).
- LuCI: manual test list (add/remove/rename link, validation errors, apply, status with 2 links, calibration with a fake `calibrate` that writes a fixed result). Optional later: Playwright against the VM.

## 11. Phases

| Phase | Content | Depends on | Size |
|---|---|---|---|
| L0 | 2026-10-15: #40 per-link overrides, #41 `-check-config`, #42 `status_file`, #43 `control_socket`. All merged. |
| L1 | Package `cake-autorate-go`: Makefile, init script, UCI default, ucode converter + tests, CI build (amd64, arm64) | L0 | M |
| L2 | VM script, install and run in the VM | L1 | S |
| L3 | LuCI config page: global section + link grid with modal, validation, apply | L1 | M |
| L4 | LuCI status page (with pause/resume for each link) + Overview widget | L0, L3 | S–M |
| L5 | Calibration: ucode rpc plugin, dialog, progress, result, apply | step 6b, L3 | M |
| L6 | Optional: CAKE setup by the package, CAKE tab | L3 | M |
| L7 | Docs (README section, screenshots), release workflow | all | S |

Each phase is a PR, with the Copilot review cycles.

## 12. Decisions (user answers 2026-10-15)

1. Per-link overrides: **yes**, in phase L0, as a separate PR. Keys: ping_method, learn_cross_delay, pinger_count, ping_interval_ms. An empty link value uses the global value.
2. UCI to YAML in **ucode** (proposal accepted by default).
3. Status interval **1 s** (proposal accepted by default).
4. Calibration stops the **whole service** in the first version.
5. Own CAKE setup: **later**. First version keeps sqm-scripts and shows a check.
6. Package format: **apk only** (OpenWrt 25.12). Add ipk later on request.
7. Done: packages `cake-autorate-go` and `luci-app-cake-autorate-go` (user decision 2026-10-11). Menu entry: Services → CAKE Autorate (Go).

## 13. Status

| Phase | Status |
|---|---|
| L0 | Done (2026-10-15): #40, #41, #42, #43 merged. |
| L1+L2 | Done (2026-10-15): #44 merged (package and VM test in one PR, for CI feedback). 8 Copilot cycles, last one approval. CI job `vm` runs 22 service checks in OpenWrt 25.12.5 x86-64. PKG_MAINTAINER stays empty (user decision). |
| L3 | In review (2026-10-15): #45. |

### L0 decisions and results

- Overrides: `ping_method`, `pinger_count`, `ping_interval_ms`, `learn_cross_delay` are pointer fields in the link. `Config.ForLink` gives the effective config. The calibration also uses it.
- `-check-config` and `-strict`: exit code 1 for an invalid file. With `-strict`, a warning also gives exit code 1.
- Status file: JSON, `schema` 1, one write each second, atomic (random temp name + rename), removed at stop. Path set by the package: `/var/run/cake-autorate/status.json` (RAM). The `paused` field is not yet in the status: add it after #42 and #43 are merged.
- Control socket: Unix socket mode 0600, commands `pause LINK [dl|ul]`, `resume LINK [dl|ul]`, `list`. A pause sets the base rate. The recording has a `pause` event.
- Validation: a link name must be unique. With `control_socket`, it must have 1 to 64 characters of `[A-Za-z0-9_.-]`, and the socket path not more than 90 bytes.
- Socket creation: in a temporary directory with mode 0700, then chmod 0600 and rename to the path. No umask change (the umask applies to all goroutines).
- Follow-up for L0: add `paused` to the status file (small PR after #43).
- Open on #43 (finding after the rebase onto #42): `syscall.Umask` changes the mask of the whole process. The status writer can create its directory in that time with mode 0600. Fix: split the socket setup from the accept loop, and open the socket in `main` before the status writer and the controllers start.
- Conflicts: resolve with a rebase onto main (user preference 2026-10-15).

### L1 decisions and results

- Source in `openwrt/cake-autorate-go/`. The Makefile copies the Go source from the repository root (no download). Build with `golang-package.mk` of the SDK packages feed (Go 1.26 in 25.12).
- Converter output is JSON (JSON is YAML). `schema.json` has the type of each option: `global`, `global_direction` (thresholds with `dl_`/`ul_`), `link`, `direction`. Exit code 3 = disabled, then the init script does not start the service.
- The section name is the link name, if `name` is not set. An anonymous link section must set `name`.
- Fixed keys set by the package: `status_file` `/var/run/cake-autorate/status.json`, `control_socket` `/var/run/cake-autorate/control.sock`.
- Local build: `scripts/openwrt-build.sh [ARCH]` keeps an SDK container `cake-autorate-sdk-ARCH`. Local Docker needs the docker group (or an ACL on /var/run/docker.sock).
- Local ucode for tests: built by hand from jow-/ucode with `-D_GNU_SOURCE`, `fs` as a module (.so). CI builds ucode with cmake.
- Review fixes: depends on `tc` (shaper fallback); converter output goes to logread; `procd_set_param file` (reload restarts on change); `adjust` default true for a link direction (Go sets false for a link that omits it); global `ping_interface`/`ping_source_addr` copied to links (Go applies them only to the legacy config); UCI read error = exit 1, not "disabled".
- Core gaps found (for a later core PR, not the package): a YAML link that omits `adjust` gets false; global `ping_interface`/`ping_source_addr` do not apply to `links`.
- Versions: CI writes the apk version to the Makefile. Release tag `1.2.0-rc.1` gives `1.2.0_rc1`; other builds `PKG_VERSION_p<commit count>`. Binary version = `git describe`. Release files get the arch in the name.
- CI: the `converter` job also runs `-check-config -strict` on a converted sample (tests/sample.uc). The `package` job needs `converter`.
- Not done: `procd_add_interface_trigger` for the WAN interfaces (the netlink retry handles a late interface), migration from `setup.sh` YAML, PKG_MAINTAINER (stays empty, user decision 2026-10-15).

### L2 decisions and results (in #44)

- `scripts/openwrt-vm.sh start|ssh|install|stop|reset`: OpenWrt 25.12.5 x86-64 ext4 image, QEMU/KVM, user network. LAN eth0 with hostfwd SSH 2222 and HTTP 8080, WAN eth1 with internet (ICMP echo through host ping sockets; ICMP timestamps do not go through, thus the test uses `ping_method icmp`). SSH key is set through the serial console (python3 socket). State in `~/.cache/cake-autorate-vm`.
- `scripts/openwrt-vm-test.sh`: CAKE on eth1 (upload) and br-lan (download), then start, adjust default, status file, socket mode 0600 and dir 0755, list/pause/resume/unknown link (control socket via ucode-mod-socket), reload without change keeps the PID, UCI change + reload_config restarts, converter error and check-config error in the log, disabled link, stop removes the files.
- Finding: `reload_config` sends `config.change` only for files from its last run. After a package install, run `reload_config` once (the test does it). LuCI apply uses ubus, thus not affected.
- CI: job `vm` after `package`; job `release` (release events only) after `package` and `vm` attaches the apk files.
- KVM works on GitHub runners with the udev rule.
- CI time: from 17-25 min to about 5 min. The CI builds a static program with setup-go into `prebuilt/`; the Makefile then skips golang (SDK bootstrap was ~10 min) and uses `EXTRA_DEPENDS` with `(>=0)` (apk needs a constraint) so the SDK does not compile tc/iproute2/libbpf/elfutils (~6 min). `NO_DEFAULT_FEEDS: 1`. Remaining time is mostly the SDK download (mirror speed varies). Without `prebuilt/` (local script), the full SDK Go build is used.

### L3 decisions and results (in #45)

- One view `view/cake-autorate/config.js`: `NamedSection global` (tabs General, Idle and stall, Delay thresholds, Advanced, Logging) and `GridSection link` (modal tabs Interfaces, Rates, Delay thresholds, Pings). The per-link overrides are in the tab Pings.
- No `luci.mk`: the SDK build needs no LuCI feed. `luci-base (>=0)` only in `EXTRA_DEPENDS`. No translations yet.
- Placeholders show the program defaults (hard-coded in the view, from `-defaults`). Flags that are on by default write "0"/"1" (`rmempty = !def`).
- Errors after apply: the init script writes `/var/run/cake-autorate/config-error` (converter or check-config output), a valid start removes it. The page polls it every 5 s (ACL: file read).
- Browser test: Playwright with Chromium in the scratchpad against the VM (login root, empty password). Not in CI (download of Chromium was slow and hung at the extraction).
- The denied `uci get luci` call in the browser log is from the login page (session 0), not from the app.
