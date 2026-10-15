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
			// The refresh makes new buttons. Keep the keyboard focus on the
			// button of the same link and direction.
			const focused = document.activeElement;
			const key = (focused && this.container.contains(focused) && focused.dataset.dir)
				? [ focused.dataset.link, focused.dataset.dir ] : null;
			this.container.replaceChildren(this.renderStatus(data));
			if (key) {
				const button = Array.prototype.find.call(this.container.querySelectorAll('button[data-dir]'),
					(b) => b.dataset.link == key[0] && b.dataset.dir == key[1]);
				if (button)
					button.focus();
			}
		}, this));
	},

	render: function(data) {
		this.canControl = data[1];
		this.container = E('div', { 'id': 'cake-autorate-status' }, this.renderStatus(data[0]));
		poll.add(L.bind(this.refresh, this), 2);
		return E('div', {}, [
			E('h2', {}, _('CAKE Autorate (Go)')),
			E('div', { 'class': 'cbi-map-descr' }, _('The state of each link and the rates of its shapers. "Pause" sets the shaper of a direction to the base rate and stops the rate changes until "Resume" or a restart of the service.')),
			this.container
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
