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

// The graphs keep the samples only in the browser, for this time. A reload
// of the page starts with empty graphs.
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

const SVG_NS = 'http://www.w3.org/2000/svg';

const COLORS = { dl: '#2a7ab8', ul: '#3a9a3a' };

const STATES = {
	RUNNING: [ _('Running'), _('The program changes the shaper rates.') ],
	IDLE: [ _('Idle'), _('The load is low. The pings stopped until the load increases.') ],
	STALL: [ _('Stall'), _('No ping replies. The pings stopped until there is traffic again.') ]
};

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

const callResume = rpc.declare({
	object: 'cake-autorate',
	method: 'resume',
	params: [ 'link', 'dir' ]
});

// Returns a text node. The status file contains UCI values, and E() reads a
// string as HTML.
function text(v) {
	return document.createTextNode(v == null ? '' : String(v));
}

function rate(kbps) {
	return text('%.1f Mbit/s'.format((+kbps || 0) / 1000));
}

function ms(v) {
	return '%.1f ms'.format(+v || 0);
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
function sample(ls, t) {
	const s = { t: t };
	for (const [ key, dir ] of DIRECTIONS) {
		const st = ls[key] || {};
		s[dir] = {
			shaper: (+st.shaper_kbps || 0) / 1000,
			achieved: (+st.achieved_kbps || 0) / 1000,
			delta: +st.delta_ms || 0,
			threshold: +st.threshold_ms || 0
		};
	}
	return s;
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
	period: 300,
	lastSampled: null,
	lastData: null,

	// The cursor on a graph: the link, the graph, and the x position of the
	// pointer as a fraction of the width ("fx") or the time of the sample
	// that the keyboard selected ("t"). A refresh shows the cursor again.
	hover: null,

	// Adds one sample for each link if the program wrote a new status, and
	// removes the old samples and the links that are not in the status.
	addSamples: function(status) {
		if (!status || !Array.isArray(status.links) || status.updated === this.lastSampled)
			return;
		this.lastSampled = status.updated;
		const now = performance.now();
		const history = new Map();
		for (const ls of status.links) {
			const samples = (this.history.get(ls.name) || []).filter((s) => now - s.t <= HISTORY_SECONDS * 1000);
			samples.push(sample(ls, now));
			history.set(ls.name, samples);
		}
		this.history = history;
	},

	// Returns a graph of the series. Each series has a key, a color, a dash
	// pattern and a function that gets the value from a sample. "floor" is
	// the smallest maximum of the y axis.
	renderGraph: function(name, kind, title, unit, samples, series, floor) {
		const now = performance.now();
		const span = this.period * 1000;
		const visible = samples.filter((s) => now - s.t <= span);
		let max = floor;
		for (const s of visible)
			for (const se of series)
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
		// and a box with the values of this sample.
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

		// Shows the cursor at the sample nearest to the hover position.
		const showCursor = (hover) => {
			let best = null, dist = Infinity;
			for (const s of visible) {
				const d = (hover.t != null) ? Math.abs(s.t - hover.t) : Math.abs(xOf(s) - hover.fx * GRAPH_WIDTH);
				if (d < dist) {
					best = s;
					dist = d;
				}
			}
			if (best == null)
				return hideCursor();
			const bestX = xOf(best);
			cursor.setAttribute('x1', bestX.toFixed(1));
			cursor.setAttribute('x2', bestX.toFixed(1));
			cursor.setAttribute('visibility', 'visible');
			const lines = [ E('div', {}, text(_('%d s ago').format(Math.round((now - best.t) / 1000)))) ];
			for (const se of series)
				lines.push(E('div', {}, [
					E('span', { 'style': 'color:%s'.format(se.color) }, '■'), ' ',
					text('%s: %.1f %s'.format(se.title, se.value(best), unit))
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

		const setHover = (hover) => {
			this.hover = Object.assign({ name: name, kind: kind }, hover);
			showCursor(this.hover);
		};
		const clearHover = () => {
			this.hover = null;
			hideCursor();
		};
		// The index of the selected sample, or -1.
		const selected = () => {
			const h = this.hover;
			return (h && h.name == name && h.kind == kind && h.t != null) ? visible.findIndex((s) => s.t == h.t) : -1;
		};

		graph.addEventListener('pointermove', (ev) => {
			const r = graph.getBoundingClientRect();
			setHover({ fx: Math.min(Math.max((ev.clientX - r.left) / r.width, 0), 1) });
		});
		graph.addEventListener('pointerleave', () => {
			if (document.activeElement !== graph)
				clearHover();
		});
		// The keyboard selects a sample: the arrow keys the previous or the
		// next sample, Home the first and End the newest.
		graph.addEventListener('focus', () => {
			const h = this.hover;
			if (visible.length && !(h && h.name == name && h.kind == kind))
				setHover({ t: visible[visible.length - 1].t });
		});
		graph.addEventListener('blur', clearHover);
		graph.addEventListener('keydown', (ev) => {
			if (!visible.length)
				return;
			const i = selected(), last = visible.length - 1;
			let j;
			switch (ev.key) {
			case 'ArrowLeft': j = Math.max((i < 0 ? last : i) - 1, 0); break;
			case 'ArrowRight': j = Math.min((i < 0 ? last : i) + 1, last); break;
			case 'Home': j = 0; break;
			case 'End': j = last; break;
			default: return;
			}
			ev.preventDefault();
			setHover({ t: visible[j].t });
		});
		if (this.hover && this.hover.name == name && this.hover.kind == kind)
			showCursor(this.hover);

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
		const v = (dir, field) => (s) => s[dir][field];
		return E('div', { 'class': 'cake-autorate-graphs', 'style': 'display:flex; flex-wrap:wrap; gap:1em; margin:1em 0' }, [
			this.renderGraph(name, 'rate', _('Rates'), 'Mbit/s', samples, [
				{ key: 'dl-shaper', title: _('Download shaper'), color: COLORS.dl, dash: '4 3', value: v('dl', 'shaper') },
				{ key: 'dl-achieved', title: _('Download achieved'), color: COLORS.dl, value: v('dl', 'achieved') },
				{ key: 'ul-shaper', title: _('Upload shaper'), color: COLORS.ul, dash: '4 3', value: v('ul', 'shaper') },
				{ key: 'ul-achieved', title: _('Upload achieved'), color: COLORS.ul, value: v('ul', 'achieved') }
			], 1),
			this.renderGraph(name, 'delay', _('Delay delta'), 'ms', samples, [
				{ key: 'dl-delta', title: _('Download delta'), color: COLORS.dl, value: v('dl', 'delta') },
				{ key: 'dl-threshold', title: _('Download threshold'), color: COLORS.dl, dash: '1 3', value: v('dl', 'threshold') },
				{ key: 'ul-delta', title: _('Upload delta'), color: COLORS.ul, value: v('ul', 'delta') },
				{ key: 'ul-threshold', title: _('Upload threshold'), color: COLORS.ul, dash: '1 3', value: v('ul', 'threshold') }
			], 1)
		]);
	},

	load: function() {
		return Promise.all([
			readStatus(),
			L.resolveDefault(callAccess('ubus', 'cake-autorate', 'pause'), false)
		]);
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

	renderControl: function(link, dir, st) {
		if (!st.adjust)
			return E('em', {}, _('Off (adjust)'));
		const verb = st.paused ? 'resume' : 'pause';
		const nodes = [
			E('span', { 'class': st.paused ? 'label notice' : 'label' },
				st.paused ? _('Paused') : _('Active'))
		];
		if (this.canControl)
			nodes.push(' ', E('button', {
				'class': 'cbi-button ' + (st.paused ? 'cbi-button-apply' : 'cbi-button-neutral'),
				'data-link': link,
				'data-dir': dir,
				'click': ui.createHandlerFn(this, 'handleControl', verb, link, dir)
			}, st.paused ? _('Resume') : _('Pause')));
		return E('span', {}, nodes);
	},

	renderLink: function(ls) {
		const state = STATES[ls.state] || [ ls.state, '' ];
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
			let delta = ms(st.delta_ms) + ' / ' + ms(st.threshold_ms);
			if (+st.cross_delay_ms > 0)
				delta += ' ' + _('(cross delay %s)').format(ms(st.cross_delay_ms));
			rows.push(E('tr', { 'class': 'tr', 'data-dir': dir }, [
				E('td', { 'class': 'td', 'data-title': _('Direction') }, title),
				E('td', { 'class': 'td', 'data-title': _('Interface') }, text(st.interface)),
				E('td', { 'class': 'td', 'data-title': _('Shaper rate') }, [
					rate(st.shaper_kbps),
					E('br'),
					E('small', {}, text(_('%s to %s, base %s').format(
						'%.1f'.format((+st.min_kbps || 0) / 1000),
						'%.1f'.format((+st.max_kbps || 0) / 1000),
						'%.1f'.format((+st.base_kbps || 0) / 1000))))
				]),
				E('td', { 'class': 'td', 'data-title': _('Achieved rate') }, rate(st.achieved_kbps)),
				E('td', { 'class': 'td', 'data-title': _('Load') }, text('%.0f %%'.format(+st.load_percent || 0))),
				E('td', { 'class': 'td', 'data-title': _('Delay delta / threshold') }, text(delta)),
				E('td', { 'class': 'td', 'data-title': _('High-delay pings') }, text(st.bufferbloat_count)),
				E('td', { 'class': 'td', 'data-title': _('Rate changes') }, text(st.rate_changes)),
				E('td', { 'class': 'td', 'data-title': _('Rate control') }, this.renderControl(ls.name, dir, st))
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
			E('h3', {}, [
				text(ls.name), ' ',
				E('span', { 'class': 'label', 'title': state[1], 'data-state': ls.state }, text(state[0]))
			]),
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
			// The refresh makes new buttons and graphs. Keep the keyboard
			// focus on the button of the same link and direction, or on the
			// same graph, and keep the cursor.
			const focused = document.activeElement;
			const inside = focused && this.container.contains(focused);
			const key = (inside && focused.dataset.dir) ? [ focused.dataset.link, 'dir', focused.dataset.dir ]
				: (inside && focused.dataset.plot) ? [ focused.dataset.link, 'plot', focused.dataset.plot ] : null;
			const hover = this.hover;
			this.addSamples(data.status);
			this.lastData = data;
			this.container.replaceChildren(this.renderStatus(data));
			// The remove of a focused graph can send "blur", which clears the
			// cursor.
			this.hover = hover;
			if (key) {
				const target = Array.prototype.find.call(this.container.querySelectorAll('[data-' + key[1] + ']'),
					(e) => e.dataset.link == key[0] && e.dataset[key[1]] == key[2]);
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
			E('div', { 'class': 'cbi-map-descr' }, _('The state of each link and the rates of its shapers. "Pause" sets the shaper of a direction to the base rate and stops the rate changes until "Resume" or a restart of the service. The graphs show only the time since you opened this page.')),
			E('p', {}, [ E('label', { 'for': 'cake-autorate-period' }, _('Graph period')), ' ', period ]),
			this.container
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
