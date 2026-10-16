// SPDX-License-Identifier: GPL-2.0
//
// Section of cake-autorate-go on the Overview page: one row for each link,
// from the status file of the program.

'use strict';
'require baseclass';
'require fs';
'require cake-autorate.common as common';

const STATUS_FILE = '/var/run/cake-autorate/status.json';

const text = common.text;

function rate(kbps) {
	return '%.1f Mbit/s'.format((+kbps || 0) / 1000);
}

// Returns the nodes for a direction: the shaper rate, the delay delta and
// the label "Paused" if a user paused the rate control.
function direction(st) {
	st = st || {};
	const nodes = [ text(rate(st.shaper_kbps) + ' (' + '%.1f ms'.format(+st.delta_ms || 0) + ')') ];
	if (st.paused)
		nodes.push(' ', common.controlLabel(true));
	return nodes;
}

return baseclass.extend({
	title: _('CAKE Autorate'),

	load: function() {
		return L.resolveDefault(fs.read(STATUS_FILE), null).then(function(data) {
			try {
				return data ? JSON.parse(data) : null;
			}
			catch (e) {
				return null;
			}
		});
	},

	render: function(status) {
		if (!status || !Array.isArray(status.links))
			return E('p', {}, E('em', {}, _('The service does not run.')));

		const rows = [
			E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th' }, _('Link')),
				E('th', { 'class': 'th' }, _('State')),
				E('th', { 'class': 'th' }, _('Download (delay delta)')),
				E('th', { 'class': 'th' }, _('Upload (delay delta)'))
			])
		];
		for (const ls of status.links)
			rows.push(E('tr', { 'class': 'tr', 'data-link': ls.name }, [
				E('td', { 'class': 'td', 'data-title': _('Link') }, text(ls.name)),
				E('td', { 'class': 'td', 'data-title': _('State') }, common.stateLabel(ls.state, ls.unshaped)),
				E('td', { 'class': 'td', 'data-title': _('Download (delay delta)') }, direction(ls.download)),
				E('td', { 'class': 'td', 'data-title': _('Upload (delay delta)') }, direction(ls.upload))
			]));
		return E('table', { 'class': 'table' }, rows);
	}
});
