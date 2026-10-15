// SPDX-License-Identifier: GPL-2.0
//
// Writes the configuration of cake-autorate-go from /etc/config/cake-autorate.
//
// Usage: ucode uci2yaml.uc OUTFILE
//
// Exit codes: 0 = file written, 1 = error, 3 = the service or all links are
// disabled (no file written).

'use strict';

import { cursor } from 'uci';
import { readfile, writefile } from 'fs';
import { convert } from './convert.uc';

const dir = '/usr/share/cake-autorate';

// The package sets these keys. LuCI reads the status file and uses the
// control socket at these paths.
const fixed = {
	status_file: '/var/run/cake-autorate/status.json',
	control_socket: '/var/run/cake-autorate/control.sock',
};

let outfile = ARGV[0];
if (!outfile) {
	warn('usage: ucode uci2yaml.uc OUTFILE\n');
	exit(1);
}

let schema = json(readfile(dir + '/schema.json'));
let uci = cursor();
let sections = uci.get_all('cake-autorate');
if (sections == null) {
	warn(sprintf('cake-autorate: cannot read /etc/config/cake-autorate: %s\n', uci.error() ?? 'unknown error'));
	exit(1);
}
let r = convert(sections, schema, fixed);

for (let e in r.errors)
	warn('cake-autorate: ' + e + '\n');
if (length(r.errors) > 0)
	exit(1);
if (r.disabled)
	exit(3);

if (writefile(outfile, sprintf('%.J\n', r.config)) == null) {
	warn(sprintf('cake-autorate: cannot write %s\n', outfile));
	exit(1);
}
exit(0);
