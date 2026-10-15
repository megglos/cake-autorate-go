// SPDX-License-Identifier: GPL-2.0
//
// Tests of convert.uc. Usage: ucode -L 'FILES_DIR/*.uc' tests/convert_test.uc FILES_DIR
// The exit code is 1 if a test fails.

'use strict';

import { readfile } from 'fs';
import { convert } from 'convert';

let files = ARGV[0];
let schema = json(readfile(files + '/schema.json'));
let failed = 0;

function check(name, got, want) {
	let g = sprintf('%J', got), w = sprintf('%J', want);
	if (g != w) {
		warn(sprintf('FAIL %s\n  got:  %s\n  want: %s\n', name, g, w));
		failed++;
	}
	else
		print(sprintf('ok   %s\n', name));
}

// sec returns a UCI section as cursor.get_all gives it.
function sec(t, name, index, opts) {
	let s = { '.type': t, '.name': name, '.index': index, '.anonymous': false };
	for (let k, v in opts)
		s[k] = v;
	return s;
}

let r = convert({
	global: sec('global', 'global', 0, {
		enabled: '1', pinger_count: '6', debug: '0', alpha_delta_ewma: '0.095',
		dl_owd_delta_delay_thr_ms: '30', reflectors: [ '1.1.1.1', '9.9.9.9' ],
		ping_method: '',
	}),
	wan: sec('link', 'wan', 1, {
		enabled: '1', name: '5g', dl_interface: 'ifb4wan', dl_adjust: '1',
		dl_min_rate_kbps: '1000', ul_interface: 'wan', ul_max_rate_kbps: '50000',
		ping_method: 'icmp-timestamp', learn_cross_delay: 'off', reflectors: '8.8.8.8',
	}),
	off: sec('link', 'off', 2, { enabled: '0', dl_interface: 'x' }),
}, schema, { status_file: '/var/run/s.json' });
check('full config', r, {
	config: {
		alpha_delta_ewma: 0.095, debug: false,
		download: { owd_delta_delay_thr_ms: 30 },
		pinger_count: 6, reflectors: [ '1.1.1.1', '9.9.9.9' ],
		status_file: '/var/run/s.json',
		links: [ {
			download: { adjust: true, interface: 'ifb4wan', min_rate_kbps: 1000 },
			learn_cross_delay: false, name: '5g', ping_method: 'icmp-timestamp',
			reflectors: [ '8.8.8.8' ],
			upload: { interface: 'wan', max_rate_kbps: 50000, adjust: true },
		} ],
	},
	disabled: false, errors: [],
});

r = convert({ b: sec('link', 'b', 1, { dl_interface: 'b' }), a: sec('link', 'a', 0, { dl_interface: 'a' }) }, schema, null);
check('link order and name from section', map(r.config.links, (l) => l.name), [ 'a', 'b' ]);

r = convert({ global: sec('global', 'global', 0, { enabled: '0' }), a: sec('link', 'a', 1, {}) }, schema, null);
check('service disabled', r, { config: null, disabled: true, errors: [] });

r = convert({ global: sec('global', 'global', 0, {}) }, schema, null);
check('no enabled link', r, { config: null, disabled: true, errors: [] });

r = convert({
	global: sec('global', 'global', 0, { foo: '1', dl_interface: 'x', pinger_count: 'six', reflectors: '1.1.1.1' }),
	a: sec('link', 'a', 1, { dl_adjust: 'maybe', dl_max_rate_kbps: '1.5', alpha_delta_ewma: '0.1', name: [ 'x', 'y' ] }),
	b: sec('link', 'b', 2, { name: 'b' }),
	c: sec('link', 'c', 3, { name: 'b' }),
	x: sec('other', 'x', 4, {}),
}, schema, null);
check('errors', r, {
	config: null, disabled: false, errors: [
		'global global: unknown option "dl_interface"',
		'global global: unknown option "foo"',
		'global global.pinger_count: "six" is not an integer',
		'link a: unknown option "alpha_delta_ewma"',
		'link a.dl_adjust: "maybe" is not a boolean (use 0 or 1)',
		'link a.dl_max_rate_kbps: "1.5" is not an integer',
		'link a.name: must be an option, not a list',
		'link c: the link name "b" is used more than one time',
		'other x: unknown section type',
	],
});

let anon = sec('link', 'cfg023456', 0, { dl_interface: 'a' });
anon['.anonymous'] = true;
r = convert({ cfg023456: anon }, schema, null);
check('anonymous link needs a name', r.errors, [ 'link cfg023456: set the option "name"' ]);

r = convert({ g1: sec('global', 'g1', 0, {}), g2: sec('global', 'g2', 1, {}), a: sec('link', 'a', 2, {}) }, schema, null);
check('one global section', r.errors, [ 'global g2: only one global section is permitted' ]);

r = convert({ a: sec('link', 'a', 0, { min_rate: '1', dl_owd_delta_delay_thr_ms: '-2.5' }) }, schema, null);
check('float and unknown link key', r.errors, [ 'link a: unknown option "min_rate"' ]);

r = convert({
	global: sec('global', 'global', 0, { ping_interface: 'wwan0', ping_source_addr: '10.0.0.2' }),
	a: sec('link', 'a', 1, { dl_adjust: '0', ul_adjust: '' }),
	b: sec('link', 'b', 2, { ping_interface: 'eth1' }),
}, schema, null);
check('adjust default and explicit 0', [ r.config.links[0].download.adjust, r.config.links[0].upload.adjust ], [ false, true ]);
check('global ping keys apply to the links',
	map(r.config.links, (l) => [ l.ping_interface, l.ping_source_addr ]),
	[ [ 'wwan0', '10.0.0.2' ], [ 'eth1', '10.0.0.2' ] ]);

r = convert({ a: sec('link', 'a', 0, { pinger_cout: '', dl_foo: '' }), g: sec('global', 'g', 1, { dl_owd_delta_delay_thr_ms: '' }) }, schema, null);
check('unknown option with an empty value', r.errors, [ 'link a: unknown option "dl_foo"', 'link a: unknown option "pinger_cout"' ]);
r = convert({ g: sec('global', 'g', 0, { dl_owd_delta_delay_thr_ms: '' }), a: sec('link', 'a', 1, {}) }, schema, null);
check('empty direction option writes no direction', r.config.download, null);

exit(failed > 0 ? 1 : 0);
