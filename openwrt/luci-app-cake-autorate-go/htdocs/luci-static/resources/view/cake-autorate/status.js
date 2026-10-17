// SPDX-License-Identifier: GPL-2.0
//
// Status page of cake-autorate-go. It reads the status file of the program
// each 2 seconds and shows one section for each link. The buttons pause and
// resume the rate control of a direction through the rpcd plugin
// "cake-autorate".

'use strict';
'require view';
'require fs';
'require poll';
'require rpc';
'require ui';
'require cake-autorate.common as common';
'require cake-autorate.calibration as calibration';

const STATUS_FILE = '/var/run/cake-autorate/status.json';
const ERROR_FILE = '/var/run/cake-autorate/config-error';

// The program writes the status file each second. If the time "updated"
// in the file does not change for this time, the program does not write it.
// The page measures this time with the clock of the browser, because the
// clock of the router can be different.
const STALE_SECONDS = 10;

const DIRECTIONS = [
	[ 'download', 'dl', _('Download') ],
	[ 'upload', 'ul', _('Upload') ]
];

// The graphs keep the samples for this time. When the page opens, it gets
// the samples of this time from the program (method "history"), if the
// history is on (status_history). Then it adds a sample at each read of
// the status file.
const HISTORY_SECONDS = 900;

// The periods that the graphs can show, in seconds.
const PERIODS = [
	[ 60, _('1 minute') ],
	[ 300, _('5 minutes') ],
	[ 900, _('15 minutes') ]
];

// A time between two samples that is longer than this is a gap in the
// line (for example, a stopped program or a hidden browser tab).
const GAP_SECONDS = 6;

// The size of the graph in SVG units. The graph fills the width of the
// page.
const GRAPH_WIDTH = 600;
const GRAPH_HEIGHT = 150;

// The time between two checks of the root qdiscs.
const QDISC_SECONDS = 10;

const SVG_NS = 'http://www.w3.org/2000/svg';

const COLORS = { dl: '#2a7ab8', ul: '#3a9a3a' };

const callAccess = rpc.declare({
	object: 'session',
	method: 'access',
	params: [ 'scope', 'object', 'function' ],
	expect: { access: false }
});

const callPause = rpc.declare({
	object: 'cake-autorate',
	method: 'pause',
	params: [ 'link', 'dir' ]
});

const callHistory = rpc.declare({
	object: 'cake-autorate',
	method: 'history'
});

const callQdisc = rpc.declare({
	object: 'cake-autorate',
	method: 'qdisc',
	params: [ 'interface' ]
});

const callShaping = {
	unshape: rpc.declare({ object: 'cake-autorate', method: 'unshape', params: [ 'link' ] }),
	shape: rpc.declare({ object: 'cake-autorate', method: 'shape', params: [ 'link' ] })
};

const callResume = rpc.declare({
	object: 'cake-autorate',
	method: 'resume',
	params: [ 'link', 'dir' ]
});

// Returns a text node. The status file contains UCI values, and E() reads a
// string as HTML.
const text = common.text;

// The shaper rate of the program without a useful limit (unshaped blocks
// of a calibration, command "unshape"): 10 Gbit/s, in Mbit/s.
const NO_LIMIT_MBITS = 10000;

function rate(kbps) {
	return text('%.1f Mbit/s'.format((+kbps || 0) / 1000));
}

// Returns a shaper rate in Mbit/s for the graphs, or null for a shaper
// without a useful limit (exactly the rate of the program for it; a
// configured rate can be higher). Else the y axis goes to 10 Gbit/s.
function shaperMbits(kbps) {
	const v = (+kbps || 0) / 1000;
	return v == NO_LIMIT_MBITS ? null : v;
}

function ms(v) {
	return '%.1f ms'.format(+v || 0);
}

// Returns the key of a focusable element. A refresh gives the focus to
// the new element with the same key. JSON keeps link names with any
// character apart.
function focusKey(link, kind, id) {
	return JSON.stringify([ link, kind, id ]);
}

function svg(tag, attrs) {
	const e = document.createElementNS(SVG_NS, tag);
	for (const k in attrs)
		e.setAttribute(k, attrs[k]);
	return e;
}

// Returns the smallest value 1, 2 or 5 * 10^n that is not less than v.
function niceMax(v) {
	if (!(v > 0))
		return 1;
	const e = Math.pow(10, Math.floor(Math.log10(v)));
	for (const m of [ 1, 2, 5, 10 ])
		if (m * e >= v)
			return m * e;
	return 10 * e;
}

// Returns one sample of a link for the graphs: rates in Mbit/s, delays in
// ms.
// Returns the time of an RFC 3339 time stamp in ms, or null. The program
// writes nanoseconds: some browsers do not parse more than 3 digits.
function parseTime(ts) {
	const ms = Date.parse(String(ts).replace(/(\.\d{3})\d+/, '$1'));
	return isNaN(ms) ? null : ms;
}

// Returns a time in ms as "YYYY-MM-DD HH:mm:ss:SSS" in the time zone of the
// browser.
function formatTime(ms) {
	const d = new Date(ms);
	const p = (n, w) => String(n).padStart(w || 2, '0');
	return '%s-%s-%s %s:%s:%s:%s'.format(d.getFullYear(), p(d.getMonth() + 1), p(d.getDate()),
		p(d.getHours()), p(d.getMinutes()), p(d.getSeconds()), p(d.getMilliseconds(), 3));
}

// "t" is the time of the browser (performance.now()) for the graph. "at" is
// the time of the status on the router, for the box of the cursor.
function sample(ls, t, at) {
	const s = { t: t, at: at };
	for (const [ key, dir ] of DIRECTIONS) {
		const st = ls[key] || {};
		s[dir] = {
			shaper: shaperMbits(st.shaper_kbps),
			achieved: (+st.achieved_kbps || 0) / 1000,
			delta: +st.delta_ms || 0,
			threshold: +st.threshold_ms || 0
		};
	}
	return s;
}

// Returns the history of the program (the answer of the method "history")
// as an object, or null if the answer is not valid. A program without the
// command "history" (an old version) gives an error: then the graphs start
// empty.
function parseHistory(res) {
	if (!res || typeof res.data != 'string')
		return null;
	try {
		const h = JSON.parse(res.data);
		return (h && h.schema == 1 && Array.isArray(h.fields) && Array.isArray(h.links)) ? h : null;
	}
	catch (e) {
		return null;
	}
}

// Returns a Map with the samples of each link from the history of the
// program. "now" is the time of the browser (performance.now()) for the
// status at the time "at" of the router. Thus, the difference of the two
// clocks has no effect. Only the samples before "at" are used: the status
// at "at" gives the next sample.
function historySamples(h, now, at) {
	const out = new Map();
	if (at == null)
		return out;
	const col = {};
	h.fields.forEach((f, i) => { col[f] = i; });
	const v = (row, f) => +row[col[f]] || 0;
	for (const lh of h.links) {
		if (!lh || typeof lh.name != 'string' || !Array.isArray(lh.samples))
			continue;
		const samples = [];
		for (const row of lh.samples) {
			const t_ms = Array.isArray(row) ? +row[col.t_ms] : NaN;
			if (!(t_ms < at) || at - t_ms > HISTORY_SECONDS * 1000)
				continue;
			const s = { t: now - (at - t_ms), at: t_ms };
			for (const [ , dir ] of DIRECTIONS)
				s[dir] = {
					shaper: shaperMbits(v(row, dir + '_shaper_kbps')),
					achieved: v(row, dir + '_achieved_kbps') / 1000,
					delta: v(row, dir + '_delta_ms'),
					threshold: v(row, dir + '_threshold_ms')
				};
			samples.push(s);
		}
		out.set(lh.name, samples);
	}
	return out;
}

function readStatus() {
	return Promise.all([
		L.resolveDefault(fs.read(STATUS_FILE), null),
		L.resolveDefault(fs.read(ERROR_FILE), '')
	]).then(function(data) {
		let status = null;
		try {
			status = data[0] ? JSON.parse(data[0]) : null;
		}
		catch (e) {
			// The program renames a complete file into place, thus an
			// error is not expected. Show the page as "no status".
		}
		return { status: status, error: data[1] };
	});
}

return view.extend({
	// True if the user can call the rpcd methods (the write ACL).
	canControl: false,

	// The last value of "updated" and the time (performance.now()) when the
	// page read it first.
	lastUpdated: null,
	lastChange: 0,

	// Returns true if "updated" did not change for STALE_SECONDS.
	isStale: function(updated) {
		const now = performance.now();
		if (updated !== this.lastUpdated) {
			this.lastUpdated = updated;
			this.lastChange = now;
		}
		return now - this.lastChange > STALE_SECONDS * 1000;
	},

	// The samples of the graphs for each link name, the period of the
	// graphs in seconds, the last "updated" with samples, and the last data.
	// "history" is a Map: a link name can be the name of an Object
	// property, for example "constructor".
	history: new Map(),
	// The answer of the method "history" at the load of the page, or null.
	programHistory: null,
	period: 300,
	lastSampled: null,
	lastData: null,

	// The cursor of the pointer: the link, the graph and the x position as a
	// fraction of the width ("fx").
	pointer: null,

	// The sample that the keyboard selected: the link, the graph and the
	// time of the sample ("t"). Only the graph with the keyboard focus has
	// a selection. A refresh shows the two cursors again.
	selection: null,

	// Shows the cursors again on the graphs of a link, or on all graphs if
	// name is null.
	showCursors: function(name) {
		for (const g of this.container.querySelectorAll('svg[data-plot]'))
			if (g.showCursor && (name == null || g.dataset.link === name))
				g.showCursor();
	},

	// Removes the cursor of the pointer from all graphs.
	leavePointer: function() {
		this.pointer = null;
		this.showCursors(null);
	},

	// Adds one sample for each link if the program wrote a new status, and
	// removes the old samples and the links that are not in the status.
	addSamples: function(status) {
		if (!status || !Array.isArray(status.links) || status.updated === this.lastSampled)
			return;
		this.lastSampled = status.updated;
		const now = performance.now();
		const at = parseTime(status.updated);
		const history = new Map();
		// The first status: start with the history of the program. Use it
		// only once.
		if (this.programHistory) {
			this.history = historySamples(this.programHistory, now, at);
			this.programHistory = null;
		}
		for (const ls of status.links) {
			const samples = (this.history.get(ls.name) || []).filter((s) => now - s.t <= HISTORY_SECONDS * 1000);
			samples.push(sample(ls, now, at));
			history.set(ls.name, samples);
		}
		this.history = history;
	},

	// Returns a graph of the series. Each series has a key, a color, a dash
	// pattern and a function that gets the value from a sample. "floor" is
	// the smallest maximum of the y axis. "now" is the time at the right
	// side. All graphs of a link must use the same time, so that a cursor
	// finds the same sample in each graph.
	renderGraph: function(name, kind, title, unit, samples, series, floor, now) {
		const span = this.period * 1000;
		const visible = samples.filter((s) => now - s.t <= span);
		let max = floor;
		// A value of null (a shaper without a limit) has no point.
		for (const s of visible)
			for (const se of series)
				if (se.value(s) != null)
					max = Math.max(max, se.value(s));
		max = niceMax(max);

		const graph = svg('svg', {
			'viewBox': '0 0 %d %d'.format(GRAPH_WIDTH, GRAPH_HEIGHT),
			'preserveAspectRatio': 'none',
			'role': 'img',
			'aria-label': _('%s. Use the arrow keys to read the values of each sample.').format(title),
			'tabindex': 0,
			'data-link': name,
			'data-plot': kind,
			'data-focus': focusKey(name, 'plot', kind),
			'style': 'display:block; width:100%; height:%dpx; border:1px solid rgba(128,128,128,.4)'.format(GRAPH_HEIGHT)
		});
		for (const f of [ 0.25, 0.5, 0.75 ])
			graph.appendChild(svg('line', {
				'x1': 0, 'x2': GRAPH_WIDTH, 'y1': GRAPH_HEIGHT * f, 'y2': GRAPH_HEIGHT * f,
				'stroke': 'currentColor', 'stroke-opacity': 0.15, 'vector-effect': 'non-scaling-stroke'
			}));
		for (const se of series) {
			let d = '', prev = null;
			for (const s of visible) {
				if (se.value(s) == null) {
					prev = null;
					continue;
				}
				const x = GRAPH_WIDTH - (now - s.t) / span * GRAPH_WIDTH;
				const y = GRAPH_HEIGHT - se.value(s) / max * GRAPH_HEIGHT;
				d += ((prev == null || s.t - prev > GAP_SECONDS * 1000) ? 'M' : 'L') + x.toFixed(1) + ' ' + y.toFixed(1) + ' ';
				prev = s.t;
			}
			graph.appendChild(svg('path', {
				'd': d.trim(),
				'fill': 'none',
				'stroke': se.color,
				'stroke-width': 1.5,
				'stroke-dasharray': se.dash || 'none',
				'vector-effect': 'non-scaling-stroke',
				'data-series': se.key
			}));
		}

		// The cursor: a vertical line at the sample nearest to the pointer,
		// and a box with the values of this sample. The other graphs of the
		// link show a dashed line and a box at the same sample.
		const cursor = svg('line', {
			'y1': 0, 'y2': GRAPH_HEIGHT,
			'stroke': 'currentColor', 'stroke-opacity': 0.6,
			'vector-effect': 'non-scaling-stroke', 'visibility': 'hidden'
		});
		graph.appendChild(cursor);
		const tip = E('div', {
			'class': 'graph-tip',
			'role': 'status',
			'aria-live': 'polite',
			'style': 'position:absolute; top:4px; display:none; padding:2px 6px; font-size:smaller; white-space:nowrap; pointer-events:none; background:var(--background-color-high, #fff); border:1px solid rgba(128,128,128,.6); border-radius:3px'
		});

		const xOf = (s) => GRAPH_WIDTH - (now - s.t) / span * GRAPH_WIDTH;

		const mine = (c) => c && c.name == name && c.kind == kind;
		const sameLink = (c) => c && c.name == name;

		// Returns the cursor of this graph: the pointer, else the selection.
		const own = () => mine(this.pointer) ? this.pointer : mine(this.selection) ? this.selection : null;
		// Returns the cursor to show: the cursor of this graph, else the
		// cursor of another graph of the link.
		const active = () => own() || (sameLink(this.pointer) ? this.pointer : sameLink(this.selection) ? this.selection : null);

		// Returns the sample nearest to the cursor c, or null.
		const nearest = (c) => {
			let best = null, dist = Infinity;
			for (const s of visible) {
				const d = (c.t != null) ? Math.abs(s.t - c.t) : Math.abs(xOf(s) - c.fx * GRAPH_WIDTH);
				if (d < dist) {
					best = s;
					dist = d;
				}
			}
			return best;
		};

		// Shows the cursor at a sample: the sample under the pointer, else
		// the selected sample. A selected sample that is out of the period
		// now changes to the oldest sample in the period.
		const showCursor = () => {
			const c = active();
			const best = c && nearest(c);
			if (best == null)
				return hideCursor();
			if (c.t != null)
				c.t = best.t;
			const bestX = xOf(best);
			cursor.setAttribute('x1', bestX.toFixed(1));
			cursor.setAttribute('x2', bestX.toFixed(1));
			cursor.setAttribute('visibility', 'visible');
			const linked = c !== own();
			cursor.setAttribute('stroke-dasharray', linked ? '3 3' : 'none');
			// Only the box of the graph with the cursor tells its values to a
			// screen reader.
			tip.setAttribute('aria-live', linked ? 'off' : 'polite');
			const lines = [ E('div', {}, text(_('%d s ago').format(Math.round((now - best.t) / 1000)))) ];
			if (best.at != null)
				lines.push(E('div', { 'class': 'graph-time' }, text(formatTime(best.at))));
			for (const se of series)
				lines.push(E('div', {}, [
					E('span', { 'style': 'color:%s'.format(se.color) }, '■'), ' ',
					text(se.value(best) == null
						? _('%s: no limit').format(se.title)
						: '%s: %.1f %s'.format(se.title, se.value(best), unit))
				]));
			tip.replaceChildren(...lines);
			tip.style.display = '';
			// Show the box on the side of the cursor with more space.
			const left = bestX / GRAPH_WIDTH < 0.5;
			tip.style.left = left ? 'calc(%.2f%% + 8px)'.format(bestX / GRAPH_WIDTH * 100) : '';
			tip.style.right = left ? '' : 'calc(%.2f%% + 8px)'.format(100 - bestX / GRAPH_WIDTH * 100);
		};
		const hideCursor = () => {
			cursor.setAttribute('visibility', 'hidden');
			tip.style.display = 'none';
		};

		// Returns the index of the shown sample, or -1.
		const shown = () => {
			const c = own();
			const best = c && nearest(c);
			return best ? visible.indexOf(best) : -1;
		};
		const select = (i) => {
			this.selection = { name: name, kind: kind, t: visible[i].t };
			this.showCursors(name);
		};

		graph.addEventListener('pointermove', (ev) => {
			const r = graph.getBoundingClientRect();
			this.pointer = { name: name, kind: kind, fx: Math.min(Math.max((ev.clientX - r.left) / r.width, 0), 1) };
			this.showCursors(name);
		});
		graph.addEventListener('pointerleave', () => {
			if (mine(this.pointer))
				this.pointer = null;
			this.showCursors(name);
		});
		// For leavePointer(): a refresh replaces the graph under the
		// pointer, and the new graph does not always get "pointerleave".
		graph.showCursor = showCursor;
		// The keyboard selects a sample: the arrow keys the previous or the
		// next sample, Home the first and End the newest. The focus selects
		// the shown sample, or the newest.
		graph.addEventListener('focus', () => {
			if (visible.length && !mine(this.selection)) {
				const i = shown();
				select(i < 0 ? visible.length - 1 : i);
			}
		});
		graph.addEventListener('blur', () => {
			if (mine(this.selection))
				this.selection = null;
			this.showCursors(name);
		});
		graph.addEventListener('keydown', (ev) => {
			if (!visible.length)
				return;
			const i = shown(), last = visible.length - 1;
			let j;
			switch (ev.key) {
			case 'ArrowLeft': j = Math.max((i < 0 ? last : i) - 1, 0); break;
			case 'ArrowRight': j = Math.min((i < 0 ? last : i) + 1, last); break;
			case 'Home': j = 0; break;
			case 'End': j = last; break;
			default: return;
			}
			ev.preventDefault();
			// The keyboard moves the cursor away from the pointer.
			if (mine(this.pointer))
				this.pointer = null;
			select(j);
		});
		showCursor();

		const legend = series.map((se) => E('span', { 'style': 'margin-right:1em; white-space:nowrap' }, [
			E('span', { 'style': 'display:inline-block; width:1.5em; vertical-align:middle; border-top:2px %s %s'.format(se.dash ? 'dashed' : 'solid', se.color) }),
			' ', se.title
		]));
		const label = 'position:absolute; left:4px; font-size:smaller; opacity:.7';

		return E('div', { 'data-graph': kind, 'data-max': max, 'style': 'flex:1 1 320px; min-width:0' }, [
			E('strong', {}, title),
			E('div', { 'style': 'font-size:smaller' }, legend),
			E('div', { 'style': 'position:relative' }, [
				graph,
				tip,
				E('span', { 'class': 'graph-max', 'style': label + '; top:2px' }, text('%s %s'.format(max, unit))),
				E('span', { 'style': label + '; bottom:2px' }, text('0')),
				(visible.length < 2)
					? E('em', { 'style': 'position:absolute; top:40%; width:100%; text-align:center' }, _('Collecting data…'))
					: ''
			]),
			E('div', { 'style': 'display:flex; justify-content:space-between; font-size:smaller; opacity:.7' }, [
				E('span', { 'class': 'graph-start' }, text(_('-%d min').format(this.period / 60))),
				E('span', {}, _('now'))
			])
		]);
	},

	renderGraphs: function(name) {
		const samples = this.history.get(name) || [];
		const now = performance.now();
		const v = (dir, field) => (s) => s[dir][field];
		return E('div', { 'class': 'cake-autorate-graphs', 'style': 'display:flex; flex-wrap:wrap; gap:1em; margin:1em 0' }, [
			this.renderGraph(name, 'rate', _('Rates'), 'Mbit/s', samples, [
				{ key: 'dl-shaper', title: _('Download shaper'), color: COLORS.dl, dash: '4 3', value: v('dl', 'shaper') },
				{ key: 'dl-achieved', title: _('Download achieved'), color: COLORS.dl, value: v('dl', 'achieved') },
				{ key: 'ul-shaper', title: _('Upload shaper'), color: COLORS.ul, dash: '4 3', value: v('ul', 'shaper') },
				{ key: 'ul-achieved', title: _('Upload achieved'), color: COLORS.ul, value: v('ul', 'achieved') }
			], 1, now),
			this.renderGraph(name, 'delay', _('Delay delta'), 'ms', samples, [
				{ key: 'dl-delta', title: _('Download delta'), color: COLORS.dl, value: v('dl', 'delta') },
				{ key: 'dl-threshold', title: _('Download threshold'), color: COLORS.dl, dash: '1 3', value: v('dl', 'threshold') },
				{ key: 'ul-delta', title: _('Upload delta'), color: COLORS.ul, value: v('ul', 'delta') },
				{ key: 'ul-threshold', title: _('Upload threshold'), color: COLORS.ul, dash: '1 3', value: v('ul', 'threshold') }
			], 1, now)
		]);
	},

	// The root qdisc of each interface of the links: the answer of the
	// method "qdisc" ({kind} or {error}). The page checks the interfaces
	// on the load and each QDISC_SECONDS.
	qdiscs: new Map(),
	qdiscChecked: 0,

	// True if the menu has the page of SQM QoS (luci-app-sqm).
	hasSqm: false,

	// Gets the root qdisc of each interface in the status. An interface
	// without an answer (for example, no permission) has no entry.
	checkQdiscs: function(status) {
		const ifaces = new Set();
		for (const ls of (status && Array.isArray(status.links)) ? status.links : [])
			for (const [ key ] of DIRECTIONS)
				if (ls[key] && ls[key].interface)
					ifaces.add(String(ls[key].interface));
		this.qdiscChecked = performance.now();
		return Promise.all(Array.from(ifaces, (i) =>
			L.resolveDefault(callQdisc(i), null).then((res) => [ i, res ])
		)).then((list) => {
			this.qdiscs = new Map(list.filter((e) => e[1] && (e[1].kind || e[1].error)));
		});
	},

	load: function() {
		return Promise.all([
			readStatus(),
			L.resolveDefault(callAccess('ubus', 'cake-autorate', 'pause'), false),
			L.resolveDefault(ui.menu.load(), null),
			L.resolveDefault(callHistory(), null)
		]).then(L.bind(function(data) {
			this.programHistory = parseHistory(data[3]);
			const network = data[2] && data[2].children && data[2].children.admin &&
				data[2].children.admin.children && data[2].children.admin.children.network;
			this.hasSqm = !!(network && network.children && network.children.sqm);
			return this.checkQdiscs(data[0].status).then(() => data);
		}, this));
	},

	// Runs pause or resume and shows the answer of the program.
	handleControl: function(verb, link, dir) {
		const call = (verb == 'pause') ? callPause : callResume;
		return call(link, dir).then(function(res) {
			if (res && res.error)
				ui.addNotification(null, E('p', {}, text(_('Command failed: %s').format(res.error))), 'danger');
		}).catch(function(err) {
			ui.addNotification(null, E('p', {}, text(_('Command failed: %s').format(err))), 'danger');
		}).then(L.bind(this.refresh, this));
	},

	renderNotRunning: function(error) {
		const nodes = [
			E('div', { 'class': 'alert-message notice' }, [
				E('h4', {}, _('The service does not run')),
				E('p', {}, _('Enable the service and a link on the configuration page, then click "Save & Apply".'))
			])
		];
		if (error && error.trim())
			nodes.push(E('div', { 'class': 'alert-message warning' }, [
				E('h4', {}, _('The service did not start: the configuration is not valid')),
				E('pre', { 'style': 'white-space: pre-wrap' }, text(error.trim()))
			]));
		return E('div', {}, nodes);
	},

	// Turns the shaping of the link off (unshape) or on (shape) and shows
	// the answer of the program.
	handleShaping: function(verb, link) {
		return callShaping[verb](link).then(function(res) {
			if (res && res.error)
				ui.addNotification(null, E('p', {}, text(_('Command failed: %s').format(res.error))), 'danger');
		}).catch(function(err) {
			ui.addNotification(null, E('p', {}, text(_('Command failed: %s').format(err))), 'danger');
		}).then(L.bind(this.refresh, this));
	},

	renderControl: function(link, dir, st, unshaped) {
		if (!st.adjust)
			return E('em', {}, _('Off (adjust)'));
		if (unshaped)
			return E('span', { 'class': 'label', 'data-control': 'unshaped' }, _('Off (shaping off)'));
		const verb = st.paused ? 'resume' : 'pause';
		const nodes = [
			common.controlLabel(st.paused)
		];
		if (this.canControl)
			nodes.push(' ', E('button', {
				'class': 'cbi-button ' + (st.paused ? 'cbi-button-apply' : 'cbi-button-neutral'),
				'data-link': link,
				'data-dir': dir,
				'data-focus': focusKey(link, 'button', dir),
				'click': ui.createHandlerFn(this, 'handleControl', verb, link, dir)
			}, st.paused ? _('Resume') : _('Pause')));
		return E('span', {}, nodes);
	},

	// Returns a warning for each direction with rate control whose
	// interface has no CAKE as root qdisc.
	renderCakeWarnings: function(ls) {
		const nodes = [];
		for (const [ key, dir, title ] of DIRECTIONS) {
			const st = ls[key] || {};
			const q = this.qdiscs.get(String(st.interface));
			if (!st.adjust || !q || q.kind == 'cake')
				continue;
			const problem = q.kind
				? _('%s: the interface %s has no CAKE (root qdisc: %s). The program cannot change the rate.').format(title, st.interface, q.kind)
				: _('%s: cannot read the qdisc of the interface %s: %s').format(title, st.interface, q.error);
			nodes.push(E('div', { 'class': 'alert-message warning', 'data-cake-warning': dir }, [
				E('p', {}, text(problem)),
				E('p', {}, this.hasSqm
					? [ _('Set up CAKE on this interface, for example with'), ' ',
						E('a', { 'href': L.url('admin/network/sqm'), 'data-focus': focusKey(ls.name, 'sqm', dir) }, _('SQM QoS')), '.' ]
					: _('Set up CAKE on this interface, for example with SQM QoS (package luci-app-sqm).'))
			]));
		}
		return nodes;
	},

	// Returns the title of a link: the name, the state, a label during a
	// calibration, and the button "Calibrate" for a user with write
	// permission.
	renderTitle: function(ls) {
		const nodes = [ text(ls.name), ' ', common.stateLabel(ls.state, ls.unshaped) ];
		if (ls.calibrating)
			nodes.push(' ', E('span', { 'class': 'label notice', 'data-calibrating': '' }, _('Calibration')));
		if (this.canControl) {
			// A calibration changes the shaper: the shaping button waits
			// for its end. Without shaping, a calibration cannot start.
			const verb = ls.unshaped ? 'shape' : 'unshape';
			nodes.push(' ', E('button', {
				'class': 'cbi-button cake-autorate-shaping ' + (ls.unshaped ? 'cbi-button-apply' : 'cbi-button-negative'),
				'style': 'font-size:small; vertical-align:middle',
				'disabled': ls.calibrating ? '' : null,
				'title': ls.unshaped
					? _('Turn the shaping on again: the rate control starts from the base rate.')
					: _('Turn the shaping off, for example to compare the delay without the shaper. A restart of the service turns it on again.'),
				'data-focus': focusKey(ls.name, 'shaping', ''),
				'click': ui.createHandlerFn(this, 'handleShaping', verb, ls.name)
			}, ls.unshaped ? _('Turn shaping on') : _('Turn shaping off')));
			nodes.push(' ', E('button', {
				'class': 'cbi-button cbi-button-action cake-autorate-calibrate',
				'style': 'font-size:small; vertical-align:middle',
				'disabled': ls.unshaped ? '' : null,
				'data-focus': focusKey(ls.name, 'calibrate', ''),
				'click': ui.createHandlerFn(this, function() { calibration.open(ls); })
			}, _('Calibrate…')));
		}
		return nodes;
	},

	renderLink: function(ls) {
		const rows = [
			E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th' }, _('Direction')),
				E('th', { 'class': 'th' }, _('Interface')),
				E('th', { 'class': 'th' }, _('Shaper rate')),
				E('th', { 'class': 'th' }, _('Achieved rate')),
				E('th', { 'class': 'th' }, _('Load')),
				E('th', { 'class': 'th' }, _('Delay delta / threshold')),
				E('th', { 'class': 'th' }, _('High-delay pings')),
				E('th', { 'class': 'th' }, _('Rate changes')),
				E('th', { 'class': 'th' }, _('Rate control'))
			])
		];
		for (const [ key, dir, title ] of DIRECTIONS) {
			const st = ls[key] || {};
			// A high delay is bufferbloat now: orange. The program adds the
			// time to send one packet at the shaper rate to the threshold,
			// thus use its "high_delay". An older program does not have it.
			const over = (typeof st.high_delay == 'boolean')
				? st.high_delay
				: (+st.threshold_ms > 0 && +st.delta_ms > +st.threshold_ms);
			const delta = [
				over ? E('span', { 'class': 'label warning', 'data-over': '' }, text(ms(st.delta_ms))) : text(ms(st.delta_ms)),
				text(' / ' + ms(st.threshold_ms))
			];
			if (+st.cross_delay_ms > 0)
				delta.push(text(' ' + _('(cross delay %s)').format(ms(st.cross_delay_ms))));
			rows.push(E('tr', { 'class': 'tr', 'data-dir': dir }, [
				E('td', { 'class': 'td', 'data-title': _('Direction') }, title),
				E('td', { 'class': 'td', 'data-title': _('Interface') }, text(st.interface)),
				E('td', { 'class': 'td', 'data-title': _('Shaper rate') }, [
					shaperMbits(st.shaper_kbps) == null ? text(_('No limit')) : rate(st.shaper_kbps),
					E('br'),
					E('small', {}, text(_('%s to %s, base %s').format(
						'%.1f'.format((+st.min_kbps || 0) / 1000),
						'%.1f'.format((+st.max_kbps || 0) / 1000),
						'%.1f'.format((+st.base_kbps || 0) / 1000))))
				]),
				E('td', { 'class': 'td', 'data-title': _('Achieved rate') }, rate(st.achieved_kbps)),
				E('td', { 'class': 'td', 'data-title': _('Load') }, text('%.0f %%'.format(+st.load_percent || 0))),
				E('td', { 'class': 'td', 'data-title': _('Delay delta / threshold') }, delta),
				E('td', { 'class': 'td', 'data-title': _('High-delay pings') }, text(st.bufferbloat_count)),
				E('td', { 'class': 'td', 'data-title': _('Rate changes') }, text(st.rate_changes)),
				E('td', { 'class': 'td', 'data-title': _('Rate control') }, this.renderControl(ls.name, dir, st, ls.unshaped))
			]));
		}

		const refl = [
			E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th' }, _('Reflector')),
				E('th', { 'class': 'th' }, _('In use')),
				E('th', { 'class': 'th' }, _('Baseline RTT')),
				E('th', { 'class': 'th' }, _('Missed replies'))
			])
		];
		for (const r of ls.reflectors || [])
			refl.push(E('tr', { 'class': 'tr' }, [
				E('td', { 'class': 'td', 'data-title': _('Reflector') }, text(r.address)),
				E('td', { 'class': 'td', 'data-title': _('In use') }, r.active ? _('Yes') : _('No')),
				E('td', { 'class': 'td', 'data-title': _('Baseline RTT') }, text(r.baseline_ms > 0 ? ms(r.baseline_ms) : '-')),
				E('td', { 'class': 'td', 'data-title': _('Missed replies') }, text(r.missed))
			]));

		return E('div', { 'class': 'cbi-section', 'data-link': ls.name }, [
			E('h3', {}, this.renderTitle(ls)),
			...this.renderCakeWarnings(ls),
			E('table', { 'class': 'table' }, rows),
			this.renderGraphs(ls.name),
			E('table', { 'class': 'table' }, refl)
		]);
	},

	renderStatus: function(data) {
		const s = data.status;
		if (!s || !Array.isArray(s.links))
			return this.renderNotRunning(data.error);

		const nodes = [];
		const updated = Date.parse(s.updated);
		if (this.isStale(s.updated))
			nodes.push(E('div', { 'class': 'alert-message warning' },
				text(_('The status is from %s. The program does not write the status file.').format(new Date(updated).toLocaleString()))));
		nodes.push(E('p', {}, text(_('Version %s, started %s').format(s.version, new Date(s.started).toLocaleString()))));
		if (!s.links.length)
			nodes.push(E('p', {}, E('em', {}, _('No link has a status yet.'))));
		for (const ls of s.links)
			nodes.push(this.renderLink(ls));
		return E('div', {}, nodes);
	},

	refresh: function() {
		return readStatus().then(L.bind(function(data) {
			if (performance.now() - this.qdiscChecked >= QDISC_SECONDS * 1000)
				return this.checkQdiscs(data.status).then(() => data);
			return data;
		}, this)).then(L.bind(function(data) {
			// The refresh makes new buttons, links and graphs. Keep the
			// keyboard focus on the element with the same focus key, and
			// keep the cursor.
			const focused = document.activeElement;
			const key = (focused && this.container.contains(focused)) ? focused.dataset.focus : null;
			const selection = this.selection;
			this.addSamples(data.status);
			this.lastData = data;
			this.container.replaceChildren(this.renderStatus(data));
			// The remove of a focused graph can send "blur", which clears the
			// selection.
			this.selection = selection;
			if (key) {
				const target = Array.prototype.find.call(this.container.querySelectorAll('[data-focus]'),
					(e) => e.dataset.focus == key);
				if (target)
					target.focus();
			}
		}, this));
	},

	render: function(data) {
		this.canControl = data[1];
		this.addSamples(data[0].status);
		this.lastData = data[0];
		this.container = E('div', { 'id': 'cake-autorate-status' }, this.renderStatus(data[0]));
		poll.add(L.bind(this.refresh, this), 2);
		// A calibration that runs (for example after a reload of the page)
		// needs the load of this browser again.
		if (this.canControl)
			calibration.resume();

		// The pointer left the graphs: on a move outside a graph, or out of
		// the window.
		document.addEventListener('pointermove', (ev) => {
			if (this.pointer && !(ev.target.closest && ev.target.closest('svg[data-plot]')))
				this.leavePointer();
		});
		document.addEventListener('pointerout', (ev) => {
			if (this.pointer && !ev.relatedTarget)
				this.leavePointer();
		});

		const period = E('select', {
			'id': 'cake-autorate-period',
			'class': 'cbi-input-select',
			'change': L.bind(function(ev) {
				this.period = +ev.target.value;
				this.container.replaceChildren(this.renderStatus(this.lastData));
			}, this)
		}, PERIODS.map(([ seconds, title ]) =>
			E('option', { 'value': seconds, 'selected': (seconds == this.period) ? '' : null }, title)));

		return E('div', {}, [
			E('h2', {}, _('CAKE Autorate (Go)')),
			E('div', { 'class': 'cbi-map-descr' }, _('The state of each link and the rates of its shapers. "Pause" sets the shaper of a direction to the base rate and stops the rate changes until "Resume" or a restart of the service. "Turn shaping off" removes the limit of the shapers of a link until "Turn shaping on" or a restart of the service. With "Graph history" on the configuration page, the graphs show the last 15 minutes (the service keeps them in memory until a restart). Else they show the time since you opened this page.')),
			E('p', {}, [ E('label', { 'for': 'cake-autorate-period' }, _('Graph period')), ' ', period ]),
			this.container
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
