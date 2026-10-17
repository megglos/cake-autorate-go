// SPDX-License-Identifier: GPL-2.0
//
// The bufferbloat test of the status page. The browser makes the load (the
// load of the calibration: downloads from and uploads to the speed test of
// Cloudflare) in 4 phases: no load, download, upload, both. The delay comes
// from the program: the delay delta of its reflectors in the status file,
// each second. Optionally, the test runs a second time without the shaper
// ("unshape" with a time), so that the user can compare the two results.

'use strict';
'require baseclass';
'require fs';
'require rpc';
'require ui';
'require cake-autorate.calibration as calibration';

const STATUS_FILE = '/var/run/cake-autorate/status.json';

// The phases of a run: the load and the time (s).
const PHASES = [
	{ key: 'idle', load: '', seconds: 6 },
	{ key: 'dl', load: 'dl', seconds: 12 },
	{ key: 'ul', load: 'ul', seconds: 12 },
	{ key: 'both', load: 'both', seconds: 12 }
];

// The first seconds of a load phase are not in the result: TCP increases
// its rate, and the program can leave Idle.
const WARMUP_S = 3;

// The rate of the link (from the program) must be at least this share of
// the rate of the browser: else the browser traffic uses another link.
const LINK_SHARE = 0.5;

// The maximum time until the status shows the shaping off (s).
const SHAPING_WAIT_S = 5;

// The time between two runs (s): the queues become empty.
const PAUSE_S = 3;

// The grades of the delay increase (ms), as on common bufferbloat tests:
// the first grade with an increase below its limit.
const GRADES = [ [ 5, 'A+' ], [ 30, 'A' ], [ 60, 'B' ], [ 200, 'C' ], [ 400, 'D' ], [ Infinity, 'F' ] ];

const callUnshape = rpc.declare({
	object: 'cake-autorate',
	method: 'unshape',
	params: [ 'link', 'seconds' ]
});

const callShape = rpc.declare({
	object: 'cake-autorate',
	method: 'shape',
	params: [ 'link' ]
});

// Returns a text node. The values come from the program, and E() reads a
// string as HTML.
function text(v) {
	return document.createTextNode(v == null ? '' : String(v));
}

function mbit(kbps) {
	return '%.1f Mbit/s'.format((+kbps || 0) / 1000);
}

function volume(bytes) {
	return bytes >= 1e9 ? '%.1f GB'.format(bytes / 1e9) : '%.0f MB'.format(bytes / 1e6);
}

// Returns the value at the quantile q (0 to 1) of the values, or null.
function quantile(values, q) {
	if (!values.length)
		return null;
	const s = values.slice().sort((a, b) => a - b);
	return s[Math.min(s.length - 1, Math.floor(q * s.length))];
}

// Returns the grade of a delay increase in ms.
function grade(ms) {
	return GRADES.find((g) => ms < g[0])[1];
}

function sleep(ms) {
	return new Promise((resolve) => setTimeout(resolve, ms));
}

// Throws the error of an rpcd answer.
function checkAnswer(res) {
	if (!res || res.error)
		throw new Error((res && res.error) || _('No answer from the service'));
}

return baseclass.extend({
	__init__: function() {
		this.node = null;
		this.running = false;
		this.cancelled = false;
		this.load = null;
		this.unshaped = false;
		// The phases and the warm-up time (tests make them shorter).
		this.phases = PHASES;
		this.warmup = WARMUP_S;
		this.onUnload = (ev) => {
			ev.preventDefault();
			ev.returnValue = '';
		};
	},

	// The time of one run (s).
	runSeconds: function() {
		return this.phases.reduce((s, p) => s + p.seconds, 0);
	},

	// The data volume of one run with the maximum rates of the link.
	estimate: function(ls) {
		const dl = (+(ls.download || {}).max_kbps || 0) * 1000 / 8;
		const ul = (+(ls.upload || {}).max_kbps || 0) * 1000 / 8;
		let bytes = 0;
		for (const p of this.phases) {
			if (p.load == 'dl' || p.load == 'both')
				bytes += p.seconds * dl;
			if (p.load == 'ul' || p.load == 'both')
				bytes += p.seconds * ul;
		}
		return bytes;
	},

	// Opens the dialog of the test for the link with the status ls.
	open: function(ls) {
		const compare = E('input', { 'type': 'checkbox', 'id': 'cake-autorate-bufferbloat-compare' });
		const limit = E('input', { 'type': 'number', 'class': 'cbi-input-text', 'id': 'cake-autorate-bufferbloat-limit', 'min': 1, 'step': 1 });
		const info = E('p', { 'class': 'cake-autorate-estimate' });
		const problem = E('div', { 'class': 'cake-autorate-problem' });
		let edited = false;
		limit.addEventListener('input', () => { edited = true; });
		const update = () => {
			const runs = compare.checked ? 2 : 1;
			const estimate = runs * this.estimate(ls);
			info.replaceChildren(text(_('Time: approximately %d s. Data volume: approximately %s (with the maximum rates of the link).')
				.format(runs * this.runSeconds() + (runs - 1) * PAUSE_S, volume(estimate))));
			// Without the shaper, the link can be faster than its maximum
			// rate: 1.5 times the volume, up to the next 100 MB.
			if (!edited)
				limit.value = Math.max(1, Math.ceil(1.5 * estimate / 1e8)) * 100;
		};
		compare.addEventListener('change', update);
		update();
		ui.showModal(_('Bufferbloat test of the link %s').format(ls.name), [
			E('p', {}, text(_('The test measures the delay of the link without load, with download, with upload and with both. This browser makes the load: it downloads from and uploads to the speed test of Cloudflare (speed.cloudflare.com). The delay comes from the reflectors of the service. The traffic of this browser must go through this link (on a router with more than one WAN link, check the routing). Do not use the network during the test.'))),
			E('div', { 'class': 'cbi-value' }, [
				E('label', { 'class': 'cbi-value-title', 'for': 'cake-autorate-bufferbloat-compare' }, _('Compare without shaping')),
				E('div', { 'class': 'cbi-value-field' }, [
					compare,
					E('div', { 'class': 'cbi-value-description' },
						_('Run the test a second time with the shaping off. The shaping comes back after the test, also if this page closes (after a time limit).'))
				])
			]),
			info,
			E('div', { 'class': 'cbi-value' }, [
				E('label', { 'class': 'cbi-value-title', 'for': 'cake-autorate-bufferbloat-limit' }, _('Data limit (MB)')),
				E('div', { 'class': 'cbi-value-field' }, [
					limit,
					E('div', { 'class': 'cbi-value-description' }, _('The test stops when the load of this browser reaches this limit.'))
				])
			]),
			problem,
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.hideModal }, _('Cancel')),
				' ',
				E('button', {
					'class': 'cbi-button cbi-button-positive',
					'click': ui.createHandlerFn(this, function() {
						const mb = Math.floor(+limit.value);
						problem.replaceChildren(...(mb >= 1 ? [] : [ E('p', { 'class': 'alert-message danger' }, text(_('The data limit must be at least 1 MB.'))) ]));
						if (mb >= 1)
							this.start(ls.name, compare.checked, mb * 1e6);
					})
				}, _('Start'))
			])
		]);
	},

	// Runs the test and shows the result. The dialog stays open.
	start: function(link, compare, limit) {
		this.running = true;
		this.cancelled = false;
		// Only the shaping that this test turns off goes on again.
		this.unshaped = false;
		this.load = new calibration.Load();
		this.load.limit = limit;
		this.load.onLimit = () => { this.cancelled = true; };
		this.node = E('div', { 'id': 'cake-autorate-bufferbloat' }, E('p', {}, E('em', { 'class': 'spinning' }, _('Starting…'))));
		ui.showModal(_('Bufferbloat test of the link %s').format(link), [ this.node ]);
		window.addEventListener('beforeunload', this.onUnload);

		const runs = [];
		let error = null, shapeError = null;
		const total = compare ? 2 : 1;
		return (async () => {
			try {
				runs.push(await this.run(link, _('With shaping'), 1, total, false));
				if (compare && !this.cancelled)
					await this.wait(link, PAUSE_S, _('Pause between the runs'));
				// A stop in the pause: no change of the shaping.
				if (compare && !this.cancelled) {
					// The time limit: the run, and a margin for slow status
					// requests. Then the shaping comes back without this page.
					// Set the flag first: a failed rate change of "unshape"
					// can leave the shaping off, and the end of the test
					// must turn it on again.
					this.unshaped = true;
					checkAnswer(await callUnshape(link, this.runSeconds() + 60));
					await this.waitForShaping(link, true);
					runs.push(await this.run(link, _('Without shaping'), 2, total, true));
				}
			}
			catch (e) {
				error = e.message || String(e);
			}
			finally {
				this.load.stop();
				if (this.unshaped) {
					try {
						checkAnswer(await callShape(link));
						this.unshaped = false;
					}
					catch (e) {
						shapeError = e.message || String(e);
					}
				}
				this.running = false;
				window.removeEventListener('beforeunload', this.onUnload);
			}
			this.renderResult(runs, error, shapeError);
		})();
	},

	// Waits until the status shows the link with the shaping off (unshaped
	// true) or on, at most SHAPING_WAIT_S: the program writes the status
	// each second.
	waitForShaping: async function(link, unshaped) {
		const end = performance.now() + SHAPING_WAIT_S * 1000;
		while (!this.cancelled && performance.now() < end) {
			this.renderRunning(_('Turning the shaping off'), Math.max(0, Math.round((end - performance.now()) / 1000)), null);
			const st = await this.readLink(link, end - performance.now());
			if (st && !!st.ls.unshaped == unshaped)
				return;
			await sleep(500);
		}
		if (!this.cancelled)
			throw new Error(_('The status does not show the shaping off.'));
	},

	// Stops the load at once; the run ends at its next check.
	stop: function() {
		this.cancelled = true;
		if (this.load)
			this.load.stop();
	},

	// Reads the status of the link, or null. A request that takes longer
	// than ms gives null.
	readLink: function(link, ms) {
		const timeout = new Promise((resolve) => setTimeout(() => resolve(null), Math.max(0, ms)));
		return Promise.race([ L.resolveDefault(fs.read(STATUS_FILE), null), timeout ]).then((data) => {
			try {
				const s = JSON.parse(data);
				const ls = (s.links || []).find((l) => l.name == link);
				return ls ? { updated: s.updated, ls: ls } : null;
			}
			catch (e) {
				return null;
			}
		});
	},

	// Waits seconds without load and shows the reason.
	wait: async function(link, seconds, what) {
		for (let i = seconds; i > 0 && !this.cancelled; i--) {
			this.renderRunning(what, i, null);
			await sleep(1000);
		}
	},

	// Runs the phases once. Returns { title, phases: [ { key, samples,
	// dl, ul, seconds } ] }.
	// Runs the phases once. Returns { title, phases: [ { key, dl, ul, idle,
	// bytes, seconds } ] }. The phases end at their time: a slow status
	// request does not make a phase longer. unshaped is true for the run
	// without shaping: then each status must show the unshaped link.
	run: async function(link, title, number, total, unshaped) {
		const result = { title: title, phases: [] };
		// The number of ping replies before the run: a status with more
		// replies has a new delay. The count goes on across the phases.
		// The Stop button shows before the first status request.
		this.renderRunning(_('Run %d of %d (%s)').format(number, total, title), this.runSeconds(), null);
		const first = await this.readLink(link, 1000);
		this.checkState(first, unshaped);
		let lastReplies = (first && !isNaN(+first.ls.replies)) ? +first.ls.replies : null;
		for (const p of this.phases) {
			if (this.cancelled)
				break;
			// linkKbps is the largest rate of the link (from the program)
			// in the measurement window of each direction.
			const ph = { key: p.key, load: p.load, dl: [], ul: [], idle: 0, bytes: null, seconds: 0, linkKbps: { dl: 0, ul: 0 } };
			result.phases.push(ph);
			this.load.set(p.load);
			const start = performance.now();
			const end = start + p.seconds * 1000;
			const warmEnd = start + (p.load ? this.warmup * 1000 : 0);
			// The rates count from the end of the warm-up, independent of the
			// status requests.
			let warm = null;
			// The first status after the warm-up sets the reply count of the
			// phase: the program writes the status up to 2 s late, thus its
			// replies can be from the warm-up. Samples come only after it.
			let baseline = !p.load || this.warmup == 0;
			const takeWarm = () => {
				if (warm == null)
					warm = { t: performance.now(), dl: this.load.sent.dl, ul: this.load.sent.ul };
			};
			const warmTimer = setTimeout(takeWarm, Math.max(0, warmEnd - performance.now()));
			try {
				const what = _('Run %d of %d (%s): %s').format(number, total, title, this.phaseTitle(p.key));
				// The Stop button shows before the first status request.
				this.renderRunning(what, p.seconds, null);
				for (let s = 1; !this.cancelled; s++) {
					await sleep(Math.max(0, Math.min(start + s * 1000, end) - performance.now()));
					// A failed load request (for example, the speed test is
					// blocked): the delay without load would get a good grade.
					if (this.load.errors > 0)
						throw new Error(_('The load failed: %s').format(this.load.lastError));
					if (performance.now() >= end - 50)
						break;
					// The time of the request, not of the answer: a status that
					// comes late can be from the warm-up.
					const asked = performance.now();
					const st = await this.readLink(link, end - asked);
					// A stop during the request: no sample (the load stopped).
					if (this.cancelled)
						break;
					// Also a load error during the last status request.
					if (this.load.errors > 0)
						throw new Error(_('The load failed: %s').format(this.load.lastError));
					// The state also of a status that comes after the end.
					this.checkState(st, unshaped);
					if (performance.now() >= end)
						break;
					const measuring = asked >= warmEnd;
					if (st && measuring) {
						ph.linkKbps.dl = Math.max(ph.linkKbps.dl, +st.ls.download.achieved_kbps || 0);
						ph.linkKbps.ul = Math.max(ph.linkKbps.ul, +st.ls.upload.achieved_kbps || 0);
					}
					// Only a status with a new ping reply has a new delay delta
					// ("replies" increases). Without new replies (Idle, Stall,
					// reflectors that do not reply), the phase gets no sample.
					const replies = st ? +st.ls.replies : NaN;
					// Only in RUNNING: before IDLE or STALL, the counter can
					// increase, and the delay delta stays from before.
					if (st && measuring && !baseline) {
						baseline = true;
					}
					else if (st && measuring && st.ls.state == 'RUNNING' && lastReplies != null && replies > lastReplies) {
						ph.dl.push(+st.ls.download.delta_ms || 0);
						ph.ul.push(+st.ls.upload.delta_ms || 0);
					}
					else if (st && measuring && lastReplies != null) {
						ph.idle++;
					}
					if (st && !isNaN(replies))
						lastReplies = replies;
					this.renderRunning(what, Math.max(0, Math.round((end - performance.now()) / 1000)), st && st.ls);
				}
				if (warm) {
					ph.seconds = (performance.now() - warm.t) / 1000;
					ph.bytes = { dl: this.load.sent.dl - warm.dl, ul: this.load.sent.ul - warm.ul };
				}
			}
			finally {
				clearTimeout(warmTimer);
			}
			// A stopped phase is not complete: its samples can be from the
			// time after the load stopped. It gets no result.
			if (this.cancelled) {
				result.phases.pop();
				break;
			}
			// A load phase without data in a direction of the load (the
			// requests hang): its delay is not a delay under load.
			if (p.load && ph.bytes) {
				for (const dir of [ 'dl', 'ul' ])
					if ((p.load == dir || p.load == 'both') && ph.bytes[dir] == 0)
						throw new Error(_('The load did not transfer data (%s).').format(dir == 'dl' ? _('download') : _('upload')));
			}
		}
		this.load.set('');
		return result;
	},

	// Throws an error if the status st of the link does not allow the run:
	// a calibration changes the shaper rates, and the shaping must be off
	// (unshaped) or on as the run needs it. A null status allows it.
	checkState: function(st, unshaped) {
		if (!st)
			return;
		if (st.ls.calibrating)
			throw new Error(_('A calibration of the link runs.'));
		if (!!st.ls.unshaped != unshaped)
			throw new Error(unshaped
				? _('The shaping came back before the end of the run without shaping.')
				: _('The shaping is off: the run with shaping needs it on.'));
	},

	phaseTitle: function(key) {
		return {
			idle: _('no load'),
			dl: _('download'),
			ul: _('upload'),
			both: _('download and upload')
		}[key];
	},

	renderRunning: function(what, left, ls) {
		const nodes = [
			E('p', { 'class': 'cake-autorate-phase' }, text(_('%s (%d s left).').format(what, left)))
		];
		if (ls)
			nodes.push(E('p', { 'class': 'cake-autorate-delay' }, text(_('Delay delta now: download %.1f ms, upload %.1f ms. State: %s.')
				.format(+ls.download.delta_ms || 0, +ls.upload.delta_ms || 0, ls.state))));
		nodes.push(E('p', { 'class': 'cake-autorate-used' }, text(_('Data: %s of %s.').format(volume(this.load.used()), volume(this.load.limit)))));
		// The Stop button stays in the page: then it keeps the keyboard
		// focus during the refreshes.
		if (!this.runningView || !this.node.contains(this.runningView.info)) {
			this.runningView = {
				info: E('div', { 'class': 'cake-autorate-progress' }),
				actions: E('div', { 'class': 'right' }, E('button', {
					'class': 'cbi-button cbi-button-negative',
					'click': ui.createHandlerFn(this, 'stop')
				}, _('Stop')))
			};
			this.node.replaceChildren(this.runningView.info, this.runningView.actions);
		}
		this.runningView.info.replaceChildren(...nodes);
	},

	// Returns the cells of a phase: the rates of the browser, the median
	// and the p95 of the delay delta of each direction, and the grade (the
	// larger median).
	// Returns true if the link did not carry the load of the phase: in a
	// direction of the load, the rate of the link (from the program) was
	// below LINK_SHARE of the rate of the browser. On a router with more
	// than one WAN link, the browser traffic can use another link.
	offLink: function(ph) {
		if (!ph.load || !ph.bytes || !(ph.seconds > 0))
			return false;
		return [ 'dl', 'ul' ].some((dir) => (ph.load == dir || ph.load == 'both') &&
			ph.linkKbps[dir] < LINK_SHARE * (ph.bytes[dir] * 8 / ph.seconds / 1000));
	},

	phaseCells: function(ph) {
		const rate = (dir) => (ph.bytes && ph.seconds > 0) ? mbit(ph.bytes[dir] * 8 / ph.seconds / 1000) : '-';
		const delay = (v) => v.length ? _('%.1f ms (p95 %.1f)').format(quantile(v, 0.5), quantile(v, 0.95)) : '-';
		const m = Math.max(quantile(ph.dl, 0.5) ?? -1, quantile(ph.ul, 0.5) ?? -1);
		const off = this.offLink(ph);
		const g = (ph.key != 'idle' && m >= 0 && !off) ? grade(m) : '';
		return [
			text(this.phaseTitle(ph.key)),
			text(ph.key == 'idle' ? '-' : rate('dl')),
			text(ph.key == 'idle' ? '-' : rate('ul')),
			text(delay(ph.dl)),
			text(delay(ph.ul)),
			g ? E('span', { 'class': 'label ' + ({ 'A+': 'success', 'A': 'success', 'B': 'notice' }[g] || 'warning'), 'data-grade': g }, g) : text(off ? _('load not on this link') : (!ph.dl.length ? _('no ping replies') : ''))
		];
	},

	renderResult: function(runs, error, shapeError) {
		const nodes = [];
		if (shapeError)
			nodes.push(E('p', { 'class': 'alert-message danger', 'data-shape-error': '' }, text(_('The shaping did not come back: %s. Click "Turn shaping on" on the status page. Else the service turns it on after the time limit of the test.').format(shapeError))));
		if (error)
			nodes.push(E('p', { 'class': 'alert-message warning' }, text(_('The test stopped with an error: %s').format(error))));
		else if (this.load.overLimit)
			nodes.push(E('p', { 'class': 'alert-message notice' }, text(_('The test stopped: the load reached the data limit of %s.').format(volume(this.load.limit)))));
		else if (this.cancelled)
			nodes.push(E('p', { 'class': 'alert-message notice' }, text(_('The test stopped.'))));
		nodes.push(E('p', {}, text(_('The delay is the delay delta of the reflectors of the service (the increase above the baseline) in each direction: the median and the p95. The grade is for the larger median: A+ below 5 ms, A below 30 ms, B below 60 ms, C below 200 ms, D below 400 ms, else F. The rates are the rates of this browser. A load phase with a rate of the link below half the rate of the browser gets no grade: the load did not go through this link.'))));
		for (const r of runs) {
			const rows = [
				E('tr', { 'class': 'tr table-titles' }, [
					E('th', { 'class': 'th' }, _('Load')),
					E('th', { 'class': 'th' }, _('Download rate')),
					E('th', { 'class': 'th' }, _('Upload rate')),
					E('th', { 'class': 'th' }, _('Download delay')),
					E('th', { 'class': 'th' }, _('Upload delay')),
					E('th', { 'class': 'th' }, _('Grade'))
				])
			];
			for (const ph of r.phases)
				rows.push(E('tr', { 'class': 'tr', 'data-phase': ph.key }, this.phaseCells(ph).map((c) => E('td', { 'class': 'td' }, c))));
			nodes.push(E('h4', {}, text(r.title)), E('table', { 'class': 'table cake-autorate-bufferbloat-result' }, rows));
		}
		nodes.push(E('div', { 'class': 'right' }, E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.hideModal }, _('Close'))));
		this.node.replaceChildren(...nodes);
	}
});
