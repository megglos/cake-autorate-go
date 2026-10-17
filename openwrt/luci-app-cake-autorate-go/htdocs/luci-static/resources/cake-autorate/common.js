// SPDX-License-Identifier: GPL-2.0
//
// Functions that the status page and the Overview section of
// cake-autorate-go both use.

'use strict';
'require baseclass';

// The states of a link: the name, the description and the class of the
// label. The colors come from the LuCI theme: "success" is green, "warning"
// is orange, a label without a class is gray.
const STATES = {
	RUNNING: [ _('Running'), _('The program changes the shaper rates.'), 'success' ],
	IDLE: [ _('Idle'), _('The load is low. The pings stopped until the load increases.'), '' ],
	STALL: [ _('Stall'), _('No ping replies. The pings stopped until there is traffic again.'), 'warning' ]
};

// The label of a link without shaping (command "unshape").
const UNSHAPED = [ _('Shaping off'), _('The shapers have no limit and the rate control stops. The delay measurement continues.'), 'warning' ];

// Returns a text node. The status file contains UCI values, and E() reads a
// string as HTML.
function text(v) {
	return document.createTextNode(v == null ? '' : String(v));
}

return baseclass.extend({
	text: text,

	// Returns the label of the state of a link. An unknown state shows as
	// it is, without a color. A link without shaping (unshaped) shows
	// "Shaping off" (orange) in all states.
	stateLabel: function(state, unshaped) {
		const s = unshaped ? UNSHAPED : (STATES[state] || [ state, '', '' ]);
		const attrs = { 'class': ('label ' + s[2]).trim(), 'title': s[1], 'data-state': state };
		if (unshaped)
			attrs['data-unshaped'] = '';
		return E('span', attrs, text(s[0]));
	},

	// Returns the label of the rate control of a direction: "Active"
	// (green) or "Paused" (blue).
	controlLabel: function(paused) {
		return E('span', { 'class': paused ? 'label notice' : 'label success', 'data-control': paused ? 'paused' : 'active' },
			paused ? _('Paused') : _('Active'));
	}
});
