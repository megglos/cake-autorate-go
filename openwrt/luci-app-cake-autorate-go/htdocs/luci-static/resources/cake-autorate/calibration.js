// SPDX-License-Identifier: GPL-2.0
//
// Calibration of a link from the status page. The program runs the session
// (rpcd methods calibrate_start, calibrate_status, calibrate_stop). This
// browser makes the load: the field "load" of the status tells which load
// (dl, ul or both). The status requests are also the sign for the program
// that the load operates: without them, the session stops.

'use strict';
'require baseclass';
'require rpc';
'require uci';
'require ui';

// The load endpoints. The answers have the header
// "Access-Control-Allow-Origin: *".
const DOWN_URL = 'https://speed.cloudflare.com/__down?bytes=';
const UP_URL = 'https://speed.cloudflare.com/__up';

// The parallel connections for each direction, the size of one download
// and of one upload.
const STREAMS = 4;
const DOWN_BYTES = 25 * 1000 * 1000;
const UP_BYTES = 4 * 1024 * 1024;

// The time between two status requests (ms). The program stops the
// session after 15 s without a request.
const POLL_MS = 1000;

// The default values of the program for the options of the proposal that
// can be empty (config.go, DefaultConfig). The dialog compares the proposal
// with these values when the configuration does not set the option.
const DEFAULTS = {
	dl_owd_delta_delay_thr_ms: '30',
	ul_owd_delta_delay_thr_ms: '30',
	learn_cross_delay: '0',
	ping_method: 'icmp',
	connection_active_thr_kbps: '2000'
};

// The default reflectors for ICMP echo. They do not reply to ICMP
// timestamp requests (config.go, noTimestampReply).
const ECHO_REFLECTORS = [
	'1.1.1.1', '1.0.0.1', '8.8.8.8', '8.8.4.4',
	'9.9.9.9', '149.112.112.112', '208.67.222.222', '208.67.220.220'
];

// The key in the session storage for the data limit of the running
// session and the data that the load used. Thus, a reload of the page
// keeps the limit.
const LIMIT_KEY = 'cake-autorate-calibration-limit';

// The load times that the user can select (s).
const LOAD_TIMES = [ 20, 40, 80 ];

// The blocks of a session with load in each direction (the program has
// these blocks: unshaped dl, autorate dl, unshaped ul, autorate ul,
// unshaped both).
const DL_BLOCKS = 3, UL_BLOCKS = 3, BLOCKS = 5;

// The time of a block without the load (s): idle time and tail.
const BLOCK_EXTRA_S = 18;

const callStart = rpc.declare({
	object: 'cake-autorate',
	method: 'calibrate_start',
	params: [ 'link', 'load_s' ]
});

const callStatus = rpc.declare({
	object: 'cake-autorate',
	method: 'calibrate_status'
});

const callStop = rpc.declare({
	object: 'cake-autorate',
	method: 'calibrate_stop'
});

// Returns a text node. The values come from the program and from UCI, and
// E() reads a string as HTML.
function text(v) {
	return document.createTextNode(v == null ? '' : String(v));
}

// Returns the status of the session (the JSON text of calibrate_status) as
// an object, or an object with "error".
function parseStatus(res) {
	if (!res || typeof res.data != 'string')
		return { state: 'error', error: (res && res.error) || _('No answer from the service') };
	try {
		return JSON.parse(res.data);
	}
	catch (e) {
		return { state: 'error', error: String(e) };
	}
}

function mbit(kbps) {
	return '%.1f Mbit/s'.format((+kbps || 0) / 1000);
}

// Returns a data volume in bytes as text.
function volume(bytes) {
	return bytes >= 1e9 ? '%.1f GB'.format(bytes / 1e9) : '%.0f MB'.format(bytes / 1e6);
}

// The load of this browser: parallel downloads or uploads, in a loop,
// until the load changes.
const Load = baseclass.extend({
	__init__: function() {
		this.mode = '';
		this.abort = null;
		this.bytes = { dl: 0, ul: 0 };
		this.errors = 0;
		this.lastError = null;
		this.upload = null;
		// limit is the maximum data volume of the load (bytes). At the
		// limit, the load stops and calls onLimit.
		this.limit = Infinity;
		this.onLimit = null;
		this.overLimit = false;
	},

	// Returns the data that the load used (bytes). An upload counts from
	// its start.
	used: function() {
		return this.bytes.dl + this.bytes.ul;
	},

	// Stops the load at the data limit.
	limitReached: function() {
		if (this.overLimit)
			return;
		this.overLimit = true;
		this.stop();
		if (this.onLimit)
			this.onLimit();
	},

	// Sets the load: dl, ul, both or empty (no load).
	set: function(mode) {
		mode = this.overLimit ? '' : (mode || '');
		if (mode == this.mode)
			return;
		this.stop();
		this.mode = mode;
		if (!mode)
			return;
		const abort = new AbortController();
		this.abort = abort;
		for (let i = 0; i < STREAMS; i++) {
			if (mode == 'dl' || mode == 'both')
				this.loop(abort.signal, 'dl');
			if (mode == 'ul' || mode == 'both')
				this.loop(abort.signal, 'ul');
		}
	},

	stop: function() {
		if (this.abort)
			this.abort.abort();
		this.abort = null;
		this.mode = '';
	},

	// The data of an upload: random bytes, so that no compression on the
	// path makes the load smaller.
	uploadData: function() {
		if (!this.upload) {
			const chunk = new Uint8Array(65536);
			crypto.getRandomValues(chunk);
			const parts = [];
			for (let n = 0; n < UP_BYTES; n += chunk.length)
				parts.push(chunk);
			this.upload = new Blob(parts);
		}
		return this.upload;
	},

	// Runs one connection in a loop until signal aborts.
	loop: async function(signal, dir) {
		while (!signal.aborted) {
			try {
				if (dir == 'dl') {
					const res = await fetch(DOWN_URL + DOWN_BYTES, { signal: signal, cache: 'no-store' });
					if (!res.ok || !res.body)
						throw new Error('HTTP ' + res.status);
					const reader = res.body.getReader();
					for (;;) {
						const r = await reader.read();
						if (r.done)
							break;
						this.bytes.dl += r.value.length;
						if (this.used() >= this.limit) {
							this.limitReached();
							return;
						}
					}
				}
				else {
					const data = this.uploadData();
					// Count the upload before it starts: then the parallel
					// uploads cannot go above the limit.
					if (this.used() + data.size > this.limit) {
						this.limitReached();
						return;
					}
					this.bytes.ul += data.size;
					const res = await fetch(UP_URL, { method: 'POST', body: data, signal: signal, cache: 'no-store' });
					if (!res.ok)
						throw new Error('HTTP ' + res.status);
				}
			}
			catch (e) {
				if (signal.aborted)
					return;
				this.errors++;
				this.lastError = String(e.message || e);
				// Do not try again at once: the endpoint or the link can be
				// down.
				await new Promise((resolve) => setTimeout(resolve, 1000));
			}
		}
	}
});

return baseclass.extend({
	__init__: function() {
		this.load = new Load();
		this.timer = null;
		this.link = null;
		this.node = null;
		this.lastBytes = null;
		this.rates = { dl: 0, ul: 0 };
		this.limitReached = false;
		this.onUnload = (ev) => {
			ev.preventDefault();
			ev.returnValue = '';
		};
	},

	// The data volume of a session with the load time loadS for a link
	// with the status ls: all load blocks at the maximum rates.
	estimate: function(ls, loadS) {
		const dl = (+(ls.download || {}).max_kbps || 0) * 1000 / 8;
		const ul = (+(ls.upload || {}).max_kbps || 0) * 1000 / 8;
		return loadS * (DL_BLOCKS * dl + UL_BLOCKS * ul);
	},

	// Returns the default data limit for the estimate in bytes: 1.5 times
	// the estimate (the capacity can be above the maximum rate), up to the
	// next 100 MB.
	defaultLimit: function(estimate) {
		return Math.max(1, Math.ceil(1.5 * estimate / 1e8)) * 1e8;
	},

	// Opens the dialog to start a calibration of the link with the status
	// ls.
	open: function(ls) {
		this.link = ls.name;
		const select = E('select', { 'class': 'cbi-input-select', 'id': 'cake-autorate-calibrate-load' },
			LOAD_TIMES.map((s) => E('option', { 'value': s, 'selected': s == 40 ? '' : null }, _('%d s').format(s))));
		const info = E('p', { 'class': 'cake-autorate-estimate' });
		const limit = E('input', { 'type': 'number', 'class': 'cbi-input-text', 'id': 'cake-autorate-calibrate-limit', 'min': 1, 'step': 1 });
		const limitProblem = E('div', { 'class': 'cake-autorate-problem' });
		let edited = false;
		limit.addEventListener('input', () => { edited = true; });
		const update = () => {
			const s = +select.value;
			const minutes = Math.ceil(BLOCKS * (s + BLOCK_EXTRA_S) / 60);
			const estimate = this.estimate(ls, s);
			info.replaceChildren(text(_('Time: approximately %d minutes. Data volume: approximately %s (with the maximum rates of the link).')
				.format(minutes, volume(estimate))));
			if (!edited)
				limit.value = Math.round(this.defaultLimit(estimate) / 1e6);
		};
		select.addEventListener('change', update);
		update();
		ui.showModal(_('Calibrate the link %s').format(ls.name), [
			E('p', {}, text(_('The calibration measures the capacity and the delay of the link and proposes values for its configuration. The service runs 5 blocks. In some blocks, the shaper of the link has no limit, and the delay can be high. This browser makes the load: it downloads from and uploads to the speed test of Cloudflare (speed.cloudflare.com).'))),
			E('ul', {}, [
				E('li', {}, text(_('Keep this page open until the end.'))),
				E('li', {}, text(_('Do not use the network during the calibration: other traffic makes the result less accurate.'))),
				E('li', {}, text(_('On a mobile data plan, the data volume can be expensive.')))
			]),
			E('div', { 'class': 'cbi-value' }, [
				E('label', { 'class': 'cbi-value-title', 'for': 'cake-autorate-calibrate-load' }, _('Load time of each block')),
				E('div', { 'class': 'cbi-value-field' }, select)
			]),
			info,
			E('div', { 'class': 'cbi-value' }, [
				E('label', { 'class': 'cbi-value-title', 'for': 'cake-autorate-calibrate-limit' }, _('Data limit (MB)')),
				E('div', { 'class': 'cbi-value-field' }, [
					limit,
					E('div', { 'class': 'cbi-value-description' },
						_('The calibration stops when the load of this browser reaches this limit. A link that is faster than its maximum rate needs more data.'))
				])
			]),
			limitProblem,
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.hideModal }, _('Cancel')),
				' ',
				E('button', {
					'class': 'cbi-button cbi-button-positive',
					'click': ui.createHandlerFn(this, function() {
						const mb = Math.floor(+limit.value);
						limitProblem.replaceChildren(...(mb >= 1 ? [] : [ E('p', { 'class': 'alert-message danger' }, text(_('The data limit must be at least 1 MB.'))) ]));
						if (!(mb >= 1))
							return;
						return this.start(ls.name, +select.value, mb * 1e6);
					})
				}, _('Start'))
			])
		]);
	},

	// Starts a session and shows its progress.
	start: function(link, loadS, limit) {
		return callStart(link, loadS).then((res) => {
			if (!res || res.error)
				throw new Error((res && res.error) || _('No answer from the service'));
			this.saveLimit({ link: link, limit: limit, dl: 0, ul: 0 });
			this.attach(link);
		}).catch((e) => {
			ui.addNotification(null, E('p', {}, text(_('The calibration did not start: %s').format(e.message || e))), 'danger');
		});
	},

	// Shows the progress dialog of the running session of the link and
	// starts the status requests.
	attach: function(link) {
		this.link = link;
		this.lastBytes = null;
		this.limitReached = false;
		// A new load for each session. After a reload, the load continues
		// with the limit and the used data of the session.
		const saved = this.readLimit();
		this.load.stop();
		this.load = new Load();
		if (saved && saved.link == link) {
			this.load.limit = saved.limit;
			this.load.bytes = { dl: saved.dl, ul: saved.ul };
		}
		this.load.onLimit = () => {
			this.limitReached = true;
			this.stop();
		};
		this.node = E('div', { 'id': 'cake-autorate-calibration' }, E('p', {}, E('em', { 'class': 'spinning' }, _('Starting…'))));
		ui.showModal(_('Calibrate the link %s').format(link), [ this.node ]);
		window.addEventListener('beforeunload', this.onUnload);
		this.poll();
	},

	// Checks if a session runs (for example after a reload of the page).
	// Then it shows the progress and makes the load again.
	resume: function() {
		return L.resolveDefault(callStatus(), null).then((res) => {
			const st = parseStatus(res);
			if (st.state == 'running')
				this.attach(st.link);
		});
	},

	poll: function() {
		this.timer = null;
		return L.resolveDefault(callStatus(), null).then((res) => {
			const st = parseStatus(res);
			if (st.state == 'running') {
				this.load.set(st.load);
				this.measure();
				if (this.load.limit < Infinity)
					this.saveLimit({ link: this.link, limit: this.load.limit, dl: this.load.bytes.dl, ul: this.load.bytes.ul });
				this.renderRunning(st);
				this.timer = setTimeout(() => this.poll(), POLL_MS);
				return;
			}
			this.finish();
			this.renderEnd(st);
		});
	},

	// Updates the load rates of this browser (kbit/s since the last
	// status).
	measure: function() {
		const now = performance.now();
		const b = this.load.bytes;
		if (this.lastBytes) {
			const s = (now - this.lastBytes.t) / 1000;
			if (s > 0) {
				this.rates.dl = (b.dl - this.lastBytes.dl) * 8 / s / 1000;
				this.rates.ul = (b.ul - this.lastBytes.ul) * 8 / s / 1000;
			}
		}
		this.lastBytes = { t: now, dl: b.dl, ul: b.ul };
	},

	// Ends the load and the status requests.
	finish: function() {
		if (this.timer)
			clearTimeout(this.timer);
		this.timer = null;
		this.load.stop();
		window.removeEventListener('beforeunload', this.onUnload);
		this.saveLimit(null);
	},

	// Reads the data limit of the running session from the session
	// storage, or null.
	readLimit: function() {
		try {
			const v = JSON.parse(sessionStorage.getItem(LIMIT_KEY));
			return (v && typeof v.link == 'string' && v.limit > 0 && v.dl >= 0 && v.ul >= 0) ? v : null;
		}
		catch (e) {
			return null;
		}
	},

	// Saves the data limit of the running session (null removes it).
	saveLimit: function(v) {
		try {
			if (v)
				sessionStorage.setItem(LIMIT_KEY, JSON.stringify(v));
			else
				sessionStorage.removeItem(LIMIT_KEY);
		}
		catch (e) {
			// No storage (private mode): a reload loses the limit.
		}
	},

	stop: function() {
		return L.resolveDefault(callStop(), null).then(() => {
			if (this.timer) {
				clearTimeout(this.timer);
				return this.poll();
			}
		});
	},

	renderRunning: function(st) {
		const modes = { dl: _('download'), ul: _('upload'), both: _('download and upload') };
		const policies = { unshaped: _('without a shaper limit'), autorate: _('with the rate control') };
		const phases = { idle: _('no load'), load: _('load'), tail: _('end of the block') };
		const total = st.blocks * ((+st.load_s || 0) + BLOCK_EXTRA_S);
		const done = Math.max(0, Math.min(100, total > 0 ? 100 * (1 - (+st.left_s || 0) / total) : 0));
		const nodes = [
			E('div', { 'class': 'cbi-progressbar', 'title': '%d %%'.format(done) },
				E('div', { 'style': 'width:%.0f%%'.format(done) })),
			E('p', { 'class': 'cake-autorate-block' }, text(_('Block %d of %d: %s, %s. Phase: %s (%d s left). Session: %d s left.').format(
				st.block, st.blocks, modes[st.mode] || st.mode, policies[st.policy] || st.policy,
				phases[st.phase] || st.phase, Math.round(+st.phase_left_s || 0), Math.round(+st.left_s || 0)))),
			E('p', { 'class': 'cake-autorate-load' }, text(st.load
				? _('Load from this browser: download %s, upload %s.').format(mbit(this.rates.dl), mbit(this.rates.ul))
				: _('No load in this phase.'))),
			E('p', { 'class': 'cake-autorate-used' }, text(this.load.limit < Infinity
				? _('Data: %s of %s.').format(volume(this.load.used()), volume(this.load.limit))
				: _('Data: %s (no limit after the reload of the page).').format(volume(this.load.used()))))
		];
		if (this.load.errors > 0)
			nodes.push(E('p', { 'class': 'alert-message warning' },
				text(_('The load had %d errors (last: %s). The result can be too low.').format(this.load.errors, this.load.lastError))));
		nodes.push(E('div', { 'class': 'right' }, E('button', {
			'class': 'cbi-button cbi-button-negative',
			'click': ui.createHandlerFn(this, 'stop')
		}, _('Stop'))));
		this.node.replaceChildren(...nodes);
	},

	renderEnd: function(st) {
		const close = E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.hideModal }, _('Close'));
		if (st.state == 'done' && st.result)
			return this.renderResult(st.result, close);
		const message = {
			stopped: this.limitReached
				? _('The calibration stopped: the load reached the data limit of %s.').format(volume(this.load.limit))
				: _('The calibration stopped.'),
			failed: _('The calibration failed: %s').format(st.error || ''),
			error: _('Cannot read the status of the calibration: %s').format(st.error || '')
		}[st.state] || _('No calibration runs.');
		this.node.replaceChildren(
			E('p', { 'class': st.state == 'stopped' ? 'alert-message notice' : 'alert-message warning' }, text(message)),
			E('div', { 'class': 'right' }, close));
	},

	// Shows the proposal: one row for each value, with the current value
	// and a checkbox. "Apply" writes the selected values to the link.
	renderResult: function(r, close) {
		return uci.load('cake-autorate').then(() => {
			const sid = this.sectionOf(r.link);
			const get = (key) => sid ? uci.get('cake-autorate', sid, key) : null;
			// The global section can have any name, or no name: use the
			// first section of the type, as the configuration page does.
			const gsid = (uci.sections('cake-autorate', 'global')[0] || {})['.name'];
			const global = (key) => gsid ? uci.get('cake-autorate', gsid, key) : null;
			const join = (v) => Array.isArray(v) ? v.join(' ') : String(v);
			// Returns the value that the program uses now: the option of the
			// link, else the option of the global section, else the default.
			const current = (key, globalKey) => {
				const v = key ? get(key) : null;
				if (v != null && v !== '')
					return { value: join(v), text: join(v) };
				const g = globalKey ? global(globalKey) : null;
				if (g != null && g !== '')
					return { value: join(g), text: _('%s (global)').format(join(g)) };
				const d = globalKey ? DEFAULTS[globalKey] : null;
				if (d != null)
					return { value: d, text: _('%s (default)').format(d) };
				return { value: null, text: _('default') };
			};
			const same = (a, b) => a != null && (a === b || (a !== '' && b !== '' && !isNaN(a) && !isNaN(b) && +a == +b));
			const rows = [];
			const add = (key, title, value, shown, why, globalKey, scope) => {
				if (value == null || value === '' || (Array.isArray(value) && !value.length))
					return;
				// An option of the global section has no link value.
				const cur = scope == 'global' ? current('', key) : current(key, globalKey);
				rows.push({ key: key, title: title, value: value, shown: shown, why: why, current: cur.text,
					unchanged: same(cur.value, join(value)), scope: scope || 'link' });
			};
			for (const [ dir, name, prefix ] of [ [ r.download, _('Download'), 'dl' ], [ r.upload, _('Upload'), 'ul' ] ]) {
				const d = dir || {};
				const why = (r.why || {})[prefix == 'dl' ? 'download rates' : 'upload rates'];
				add(prefix + '_min_rate_kbps', _('%s minimum rate (kbit/s)').format(name), d.min_rate_kbps, d.min_rate_kbps, why);
				add(prefix + '_base_rate_kbps', _('%s base rate (kbit/s)').format(name), d.base_rate_kbps, d.base_rate_kbps, why);
				add(prefix + '_max_rate_kbps', _('%s maximum rate (kbit/s)').format(name), d.max_rate_kbps, d.max_rate_kbps, why);
				add(prefix + '_owd_delta_delay_thr_ms', _('%s delay threshold (ms)').format(name), d.owd_delta_delay_thr_ms,
					d.owd_delta_delay_thr_ms, (r.why || {})[prefix == 'dl' ? 'download threshold' : 'upload threshold'],
					prefix + '_owd_delta_delay_thr_ms');
			}
			add('learn_cross_delay', _('Learn the cross delay'), r.learn_cross_delay ? '1' : '0',
				r.learn_cross_delay ? _('on') : _('off'), (r.why || {}).learn_cross_delay, 'learn_cross_delay');
			add('ping_method', _('Ping method'), r.ping_method, r.ping_method, (r.why || {}).ping_method || (r.why || {}).reflectors, 'ping_method');
			add('reflectors', _('Reflectors'), r.reflectors, (r.reflectors || []).join(' '), (r.why || {}).reflectors, 'reflectors');
			add('connection_active_thr_kbps', _('Activity threshold (kbit/s), all links'), r.connection_active_thr_kbps,
				r.connection_active_thr_kbps,
				_('Above this rate, the program leaves Idle. It must be below the minimum rates. This option of the global section applies to all links.'),
				null, 'global');

			const table = E('table', { 'class': 'table cake-autorate-result' }, [
				E('tr', { 'class': 'tr table-titles' }, [
					E('th', { 'class': 'th' }, _('Apply')),
					E('th', { 'class': 'th' }, _('Setting')),
					E('th', { 'class': 'th' }, _('Current value')),
					E('th', { 'class': 'th' }, _('Proposed value'))
				]),
				...rows.map((row) => E('tr', { 'class': 'tr', 'data-key': row.key }, [
					E('td', { 'class': 'td' }, E('input', { 'type': 'checkbox', 'checked': row.unchanged ? null : '', 'data-key': row.key, 'aria-label': row.title })),
					E('td', { 'class': 'td' }, [ text(row.title), row.why ? E('br') : '', row.why ? E('small', {}, text(row.why)) : '' ]),
					E('td', { 'class': 'td' }, text(row.current)),
					E('td', { 'class': 'td' }, row.unchanged ? [ text(row.shown), ' ', E('small', {}, text(_('(no change)'))) ] : E('strong', {}, text(row.shown)))
				]))
			]);
			const notes = (r.notes || []).map((n) => E('li', {}, text(n)));
			const problemNode = E('div', { 'class': 'cake-autorate-problem' });
			const apply = E('button', {
				'class': 'cbi-button cbi-button-apply',
				'disabled': sid ? null : '',
				'click': ui.createHandlerFn(this, function() {
					const keys = Array.prototype.filter.call(table.querySelectorAll('input[type=checkbox]'), (c) => c.checked)
						.map((c) => c.dataset.key);
					const selected = rows.filter((row) => keys.indexOf(row.key) >= 0);
					const problem = this.check(sid, gsid, selected);
					problemNode.replaceChildren(...(problem ? [ E('p', { 'class': 'alert-message danger' }, text(problem)) ] : []));
					if (problem)
						return;
					return this.apply(sid, gsid, selected);
				})
			}, _('Apply the selected values'));
			this.node.replaceChildren(
				E('p', {}, text(_('The calibration is done. The proposal comes from the measured data. Check the values before you apply them.'))),
				table,
				notes.length ? E('div', { 'class': 'alert-message notice' }, [ E('strong', {}, _('Notes')), E('ul', {}, notes) ]) : '',
				sid ? '' : E('p', { 'class': 'alert-message warning' }, text(_('The configuration has no link "%s".').format(r.link))),
				problemNode,
				E('div', { 'class': 'right' }, [ close, ' ', apply ]));
		});
	},

	// Returns the UCI section of the link with the name name: the option
	// "name", else the section name.
	sectionOf: function(name) {
		const s = uci.sections('cake-autorate', 'link').find((s) => (s.name || s['.name']) == name);
		return s ? s['.name'] : null;
	},

	// Returns the problem of the configuration with the selected rows, or
	// null. The service would refuse the configuration (config.go,
	// Validate): the rates of a direction must be in the order min, base,
	// max, and icmp-timestamp needs a reflector that can reply to it.
	check: function(sid, gsid, rows) {
		const value = (key, global) => {
			const row = rows.find((r) => r.key == key);
			if (row)
				return row.value;
			const v = sid ? uci.get('cake-autorate', sid, key) : null;
			if (v != null && v !== '' && !(Array.isArray(v) && !v.length))
				return v;
			return (global && gsid) ? uci.get('cake-autorate', gsid, key) : null;
		};
		for (const [ dir, name ] of [ [ 'dl', _('Download') ], [ 'ul', _('Upload') ] ]) {
			const min = +value(dir + '_min_rate_kbps'), base = +value(dir + '_base_rate_kbps'), max = +value(dir + '_max_rate_kbps');
			if (min > 0 && base > 0 && max > 0 && !(min <= base && base <= max))
				return _('%s: the selected rates give minimum %d, base %d, maximum %d kbit/s. Select the rates together.').format(name, min, base, max);
		}
		const method = value('ping_method', true) || DEFAULTS.ping_method;
		let reflectors = value('reflectors', true);
		reflectors = Array.isArray(reflectors) ? reflectors : (reflectors ? String(reflectors).split(/\s+/) : []);
		if (method == 'icmp-timestamp' && reflectors.length && reflectors.every((r) => ECHO_REFLECTORS.indexOf(r) >= 0))
			return _('With the ping method icmp-timestamp, no reflector of the list replies to ICMP timestamp requests. Select the proposed reflectors too.');
		return null;
	},

	// Writes the rows to the link section (and the global section) and
	// applies the changes.
	apply: function(sid, gsid, rows) {
		for (const row of rows) {
			let target = sid;
			if (row.scope == 'global')
				target = gsid || uci.add('cake-autorate', 'global');
			uci.set('cake-autorate', target, row.key, Array.isArray(row.value) ? row.value : String(row.value));
		}
		return uci.save().then(() => {
			ui.hideModal();
			return ui.changes.apply(true);
		});
	}
});
