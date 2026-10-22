#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0
#
# starlink-floor.py: examine the 15 s delay floor changes of Starlink in
# recordings of cake-autorate-go (--record, .jsonl or .jsonl.gz).
#
# Starlink reconfigures the satellite links every 15 s, at the seconds 12,
# 27, 42 and 57 of each minute (UTC). Then the delay floor can change. The
# script shows:
#
#   1. profile   the delay per half second of the minute (send time of the
#                ping, UTC): p10, p50 and p90 of the excess delay. The
#                excess delay of a ping is its delay minus the minimum of
#                the same reflector in the recording. Each reflector has a
#                different base delay (and, for the one-way delays, a
#                different clock offset). Thus, the script does not compare
#                delays of different reflectors directly.
#   2. steps     the floor step at each boundary (OFFSET + n x PERIOD): for
#                each reflector, the minimum delay in WINDOW before and
#                after the boundary (without GUARD at the boundary). The
#                step of the boundary is the median of the reflectors. The
#                table compares the boundaries with points in the middle
#                between two boundaries (control). If Starlink changes the
#                floor at the boundaries, the boundary steps are larger.
#                It also shows if the step is the same in the two
#                directions.
#   3. scan      the mean absolute step for each offset from 0 to PERIOD in
#                steps of 0.5 s. The largest values show where the floor
#                changes. Use it if the boundaries are not at 12/27/42/57.
#
# Without one-way delays (ping_method icmp), the script uses RTT/2.
#
# Use a recording without load: a queue also changes the delay, and makes
# the steps at the boundaries and in the control equally large. On a 5G
# day-pass session with load (no Starlink), the boundaries and the control
# have the same steps, and the scan has no peak.
#
# Usage:
#   scripts/starlink-floor.py [options] RECORDING...
#   scripts/starlink-floor.py --self-test
#
# Options:
#   --period S     time between two reconfigurations (default 15)
#   --offset S     time of the first reconfiguration in the minute (default 12)
#   --window S     time before and after a boundary for the floor (default 3)
#   --guard S      time at a boundary that the floor does not use (default 0.2)
#   --link NAME    use only the pings of this link (necessary if the
#                  recordings have more than one link)
#   --self-test    make a recording with known floor steps and check the
#                  result
#
# Requirements: python3.

import argparse
import bisect
import gzip
import json
import math
import random
import statistics
import sys
import tempfile
from datetime import datetime


def read_pings(paths, link=None):
    """Returns a list of (send time in s since the epoch, series, dl ms,
    ul ms, rtt ms) of the replies, and the set of the links. dl and ul are
    RTT/2 without one-way delays. The series is the reflector and the kind
    of the measurement: a one-way delay contains the clock offset of the
    reflector, RTT/2 does not. Thus, the two kinds are different series."""
    out = []
    links = set()
    for path in paths:
        opener = gzip.open if path.endswith('.gz') else open
        start = None
        with opener(path, 'rt') as f:
            for line in f:
                try:
                    ev = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if ev.get('type') == 'header':
                    start = datetime.fromisoformat(ev['start'].replace('Z', '+00:00')).timestamp()
                    continue
                if ev.get('type') != 'ping' or ev.get('timeout') or start is None:
                    continue
                if link and ev.get('link') != link:
                    continue
                links.add(ev.get('link'))
                rtt = ev['rtt_us'] / 1000
                sent = start + ev['t'] / 1e6 - rtt / 1000
                dl = ev.get('dl_owd_us')
                ul = ev.get('ul_owd_us')
                kind = 'owd'
                if dl is None or ul is None:
                    dl = ul = rtt * 1000 / 2
                    kind = 'rtt'
                out.append((sent, (ev['reflector'], kind), dl / 1000, ul / 1000, rtt))
    out.sort()
    return out, links


def excess(pings):
    """Returns the pings with the delays minus the minimum of the reflector."""
    mins = {}
    for _, r, dl, ul, rtt in pings:
        m = mins.setdefault(r, [dl, ul, rtt])
        m[0], m[1], m[2] = min(m[0], dl), min(m[1], ul), min(m[2], rtt)
    return [(t, r, dl - mins[r][0], ul - mins[r][1], rtt - mins[r][2]) for t, r, dl, ul, rtt in pings]


def quantile(v, p):
    if not v:
        return float('nan')
    v = sorted(v)
    return v[min(int(len(v) * p), len(v) - 1)]


def profile(pings):
    bins = {}
    for t, _, dl, ul, rtt in pings:
        b = int((t % 60) * 2) / 2
        bins.setdefault(b, ([], [], []))
        for i, v in enumerate((dl, ul, rtt)):
            bins[b][i].append(v)
    print('Excess delay per half second of the minute (UTC send time), ms')
    print('%5s %6s | %-17s | %-17s | %-17s' % ('s', 'pings', 'dl p10/p50/p90', 'ul p10/p50/p90', 'rtt p10/p50/p90'))
    for b in sorted(bins):
        cols = ['%5.1f %5.1f %5.1f' % tuple(quantile(v, p) for p in (0.1, 0.5, 0.9)) for v in bins[b]]
        print('%5.1f %6d | %s | %s | %s' % (b, len(bins[b][0]), *cols))


def boundary_steps(pings, period, offset, window, guard):
    """Returns a list of (boundary time, dl step, ul step, rtt step) for the
    boundaries offset + n x period. A step is the median over the reflectors
    of (minimum after - minimum before)."""
    if not pings:
        return []
    by_refl = {}
    for t, r, dl, ul, rtt in pings:
        by_refl.setdefault(r, []).append((t, dl, ul, rtt))
    # The pings are in time order: a binary search finds the windows.
    times = {r: [s[0] for s in v] for r, v in by_refl.items()}
    t0, t1 = pings[0][0], pings[-1][0]
    first = math.floor((t0 - offset) / period) * period + offset
    out = []
    b = first
    while b <= t1:
        steps = [[], [], []]
        for r, samples in by_refl.items():
            ts = times[r]
            before = samples[bisect.bisect_left(ts, b - window):bisect.bisect_left(ts, b - guard)]
            after = samples[bisect.bisect_left(ts, b + guard):bisect.bisect_left(ts, b + window)]
            if not before or not after:
                continue
            for i in range(3):
                steps[i].append(min(s[i + 1] for s in after) - min(s[i + 1] for s in before))
        if len(steps[0]) >= 2:
            out.append((b, *(statistics.median(s) for s in steps)))
        b += period
    return out


def summary(name, steps):
    if not steps:
        print('%-9s no boundary with 2 or more reflectors' % name)
        return
    cols = []
    for i in range(3):
        a = [abs(s[i + 1]) for s in steps]
        cols.append('%5.1f %5.1f %4.0f%%' % (quantile(a, 0.5), quantile(a, 0.9),
                                            100 * sum(1 for x in a if x >= 5) / len(a)))
    same = [abs(s[1] - s[2]) for s in steps]
    print('%-9s %5d | %s | %s | %s | %5.1f' % (name, len(steps), *cols, statistics.median(same)))


def steps_report(pings, period, offset, window, guard):
    print('Floor step at the boundaries (offset %.1f s, period %.1f s) and in the middle between them (control), ms' % (offset, period))
    print('|step|: p50, p90 and the part of the boundaries with 5 ms or more. dl-ul: median of |dl step - ul step|.')
    print('%-9s %5s | %-17s | %-17s | %-17s | %5s' % ('', 'n', 'dl p50 p90 >=5', 'ul p50 p90 >=5', 'rtt p50 p90 >=5', 'dl-ul'))
    summary('boundary', boundary_steps(pings, period, offset, window, guard))
    summary('control', boundary_steps(pings, period, offset + period / 2, window, guard))


def scan(pings, period, window, guard):
    print('Mean |step| of the RTT for each offset in the period (largest first), ms')
    res = []
    o = 0.0
    while o < period:
        s = boundary_steps(pings, period, o, min(window, period / 2 - guard), guard)
        if s:
            res.append((statistics.mean(abs(x[3]) for x in s), o, len(s)))
        o += 0.5
    res.sort(reverse=True)
    for m, o, n in res[:6]:
        secs = ' '.join('%g' % ((o + k * period) % 60) for k in range(int(60 / period)))
        print('offset %4.1f s (seconds %s): %5.1f ms (%d boundaries)' % (o, secs, m, n))
    if res:
        print('median of all offsets: %.1f ms' % statistics.median(r[0] for r in res))


def self_test():
    """A recording of 10 minutes: 6 reflectors with clock offsets, a floor
    that changes at the seconds 12, 27, 42 and 57 by up to +-15 ms (the
    same in the two directions), a random jitter, and some replies without
    one-way delays."""
    rng = random.Random(1)
    start = 1798761600.0  # 2027-01-01T00:00:00Z
    lines = [json.dumps({'type': 'header', 'start': '2027-01-01T00:00:00Z'})]
    floor = 10.0
    slot = None
    for i in range(6 * 10 * 60 * 1000 // 300):
        refl = i % 6
        sent = i * 0.05 + 0.01
        s = math.floor((sent - 12) / 15)
        if s != slot:
            slot = s
            floor = 10 + rng.uniform(0, 15)
        dl = floor + 5 * refl + rng.expovariate(1 / 2) + 1000 * refl  # clock offset
        ul = floor + 3 * refl + rng.expovariate(1 / 2) - 1000 * refl
        rtt = (dl - 1000 * refl) + (ul + 1000 * refl)
        ev = {'t': int((sent + rtt / 1000) * 1e6), 'type': 'ping', 'link': 'wan',
              'reflector': 'r%d' % refl, 'rtt_us': int(rtt * 1000)}
        # Some replies have no one-way delays: they must not make steps.
        if i % 7:
            ev['dl_owd_us'], ev['ul_owd_us'] = int(dl * 1000), int(ul * 1000)
        lines.append(json.dumps(ev))
    with tempfile.NamedTemporaryFile('w', suffix='.jsonl') as f:
        f.write('\n'.join(lines) + '\n')
        f.flush()
        pings = excess(read_pings([f.name])[0])
    on = boundary_steps(pings, 15, 12, 3, 0.2)
    off = boundary_steps(pings, 15, 19.5, 3, 0.2)
    mean_on = statistics.mean(abs(s[3]) for s in on)
    mean_off = statistics.mean(abs(s[3]) for s in off)
    best = max(((statistics.mean(abs(x[3]) for x in boundary_steps(pings, 15, o / 2, 3, 0.2)), o / 2)
                for o in range(30)))
    ok = mean_on > 3 * mean_off and best[1] == 12 % 15 and max(abs(x[1]) for x in off) < 5
    print('self-test: mean |rtt step| %.1f ms at the boundaries, %.1f ms in the middle, best offset %.1f s: %s'
          % (mean_on, mean_off, best[1], 'ok' if ok else 'FAILED'))
    return 0 if ok else 1


def main():
    ap = argparse.ArgumentParser(description='Examine the 15 s delay floor changes of Starlink in recordings.')
    ap.add_argument('recordings', nargs='*')
    ap.add_argument('--period', type=float, default=15)
    ap.add_argument('--offset', type=float, default=12)
    ap.add_argument('--window', type=float, default=3)
    ap.add_argument('--guard', type=float, default=0.2)
    ap.add_argument('--link')
    ap.add_argument('--self-test', action='store_true')
    a = ap.parse_args()
    if a.self_test:
        return self_test()
    if not a.recordings:
        ap.error('no recording')
    for name in ('period', 'window'):
        v = getattr(a, name)
        if not math.isfinite(v) or v <= 0:
            ap.error('--%s must be a number above 0' % name)
    if not math.isfinite(a.offset):
        ap.error('--offset must be a number')
    if not math.isfinite(a.guard) or a.guard < 0 or a.guard >= a.window:
        ap.error('--guard must be 0 or more and less than --window')
    raw, links = read_pings(a.recordings, a.link)
    # The pings of different links have different delays: do not mix them.
    if len(links) > 1:
        ap.error('the recordings have more than one link (%s): select one with --link' % ', '.join(sorted(map(str, links))))
    pings = excess(raw)
    if not pings:
        print('no ping replies', file=sys.stderr)
        return 1
    print('%d ping replies, %d reflectors, %.0f s' % (len(pings), len({p[1][0] for p in pings}), pings[-1][0] - pings[0][0]))
    print()
    profile(pings)
    print()
    steps_report(pings, a.period, a.offset, a.window, a.guard)
    print()
    scan(pings, a.period, a.window, a.guard)
    return 0


if __name__ == '__main__':
    sys.exit(main())
