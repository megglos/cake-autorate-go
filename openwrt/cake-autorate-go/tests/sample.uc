// SPDX-License-Identifier: GPL-2.0
//
// Writes the converted configuration of a sample UCI configuration to
// stdout. The CI checks the result with "cake-autorate-go -check-config
// -strict". The sample has the values of the package default, empty
// options and the global keys that the converter copies to the links.
// Usage: ucode -L 'FILES_DIR/*.uc' tests/sample.uc FILES_DIR

'use strict';

import { readfile } from 'fs';
import { convert } from 'convert';

let schema = json(readfile(ARGV[0] + '/schema.json'));

function sec(t, name, index, opts) {
	let s = { '.type': t, '.name': name, '.index': index, '.anonymous': false };
	for (let k, v in opts)
		s[k] = v;
	return s;
}

let r = convert({
	global: sec('global', 'global', 0, {
		enabled: '1', ping_interface: 'wan', pinger_count: '', debug: '0',
		dl_owd_delta_delay_thr_ms: '30', status_history: '1',
	}),
	wan: sec('link', 'wan', 1, {
		name: 'wan', dl_interface: 'ifb4wan', dl_min_rate_kbps: '5000',
		dl_base_rate_kbps: '20000', dl_max_rate_kbps: '80000', dl_adjust: '',
		ul_interface: 'wan', ul_adjust: '1', ul_min_rate_kbps: '5000',
		ul_base_rate_kbps: '20000', ul_max_rate_kbps: '35000',
	}),
	lte: sec('link', 'lte', 2, {
		dl_interface: 'ifb4wwan', dl_min_rate_kbps: '1000', dl_base_rate_kbps: '10000',
		dl_max_rate_kbps: '50000', ul_interface: 'wwan', ul_min_rate_kbps: '1000',
		ul_base_rate_kbps: '5000', ul_max_rate_kbps: '20000', ping_method: 'icmp',
		learn_cross_delay: '1',
	}),
}, schema, {
	status_file: '/var/run/cake-autorate/status.json',
	control_socket: '/var/run/cake-autorate/control.sock',
});

if (length(r.errors) > 0 || r.config == null) {
	warn(join('\n', r.errors) + '\n');
	exit(1);
}
printf('%.J\n', r.config);
