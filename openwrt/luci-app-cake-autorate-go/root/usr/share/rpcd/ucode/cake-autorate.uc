// SPDX-License-Identifier: GPL-2.0
//
// rpcd plugin of luci-app-cake-autorate-go. It sends the commands "pause"
// and "resume" to the control socket of cake-autorate-go. The ACL of the
// app gives these methods only to the users that can write the
// configuration. The method "history" gives the samples of the last 15
// minutes, for the graphs. The methods "calibrate_start",
// "calibrate_status" and "calibrate_stop" control a calibration session of
// the program; the ACL gives them only to the users that can write the
// configuration. The method "qdisc" gives the root qdisc of an
// interface, to find interfaces without CAKE.

'use strict';

import * as socket from 'socket';
import { popen } from 'fs';

const CONTROL_SOCKET = '/var/run/cake-autorate/control.sock';

// The rule of the program for link names (config.go).
const LINK_NAME_RE = /^[A-Za-z0-9_.-]{1,64}$/;

// The rule of Linux for interface names, without the characters that the
// shell or tc read in a different way.
const IFACE_RE = /^[A-Za-z0-9_.@:-]{1,15}$/;

// The time in seconds that the program has to answer.
const TIMEOUT = 5;

// The maximum length of an answer in bytes. A ubus message must be smaller
// than 1 MiB.
const MAX_ANSWER = 960 * 1024;

// Sends one command line and returns { answer } (the answer line without
// the newline) or { error }.
function exchange(line) {
	// An explicit Unix socket: socket.connect() with a path that does not
	// exist tries a DNS lookup of the path.
	let s = socket.create(socket.AF_UNIX, socket.SOCK_STREAM);
	if (!s)
		return { error: 'Cannot create a socket: ' + socket.error() };

	// Set the timeouts before connect: a connect to a full queue also
	// waits. The socket module reads the fields "sec" and "usec".
	const tv = { sec: TIMEOUT, usec: 0 };
	s.setopt(socket.SOL_SOCKET, socket.SO_RCVTIMEO, tv);
	s.setopt(socket.SOL_SOCKET, socket.SO_SNDTIMEO, tv);

	if (!s.connect({ family: socket.AF_UNIX, path: CONTROL_SOCKET })) {
		const err = socket.error();
		s.close();
		return { error: 'The service does not run: ' + err };
	}

	// Read until the newline: a long answer comes in more parts.
	let answer = '', err = null, complete = false;
	if (s.send(line + '\n') != null) {
		while (length(answer) <= MAX_ANSWER) {
			const part = s.recv(65536);
			if (part == null || part == '')
				break;
			answer += part;
			if (index(part, '\n') >= 0) {
				complete = true;
				break;
			}
		}
	}
	// Get the error before close: a close without error clears it.
	err = socket.error();
	s.close();

	if (length(answer) > MAX_ANSWER)
		return { error: 'The answer of the service is too long' };
	// An empty answer: the program closed the connection without an answer.
	if (answer == '')
		return { error: 'No answer from the service' + (err ? ': ' + err : '') };
	if (!complete)
		return { error: 'Incomplete answer from the service' + (err ? ': ' + err : '') };
	return { answer: trim(answer) };
}

// Sends one command line and returns { result: 'ok' } or { error }.
function command(line) {
	const res = exchange(line);
	if (res.error)
		return res;
	const answer = res.answer;
	if (answer == 'ok')
		return { result: 'ok' };
	return { error: replace(answer, /^error: /, '') };
}

// Checks the arguments and sends "verb LINK [dl|ul]".
function pause_resume(verb, args) {
	const link = args?.link, dir = args?.dir ?? '';
	if (type(link) != 'string' || !match(link, LINK_NAME_RE))
		return { error: 'Invalid link name' };
	if (!(dir in [ '', 'dl', 'ul' ]))
		return { error: 'Invalid direction, use dl or ul' };
	return command(verb + ' ' + link + (dir ? ' ' + dir : ''));
}

// Returns the history of the program as a JSON text in "data". The page
// parses it: a JSON text is smaller in a ubus message than a table.
function history() {
	const res = exchange('history');
	if (res.error)
		return res;
	if (substr(res.answer, 0, 7) == 'error: ')
		return { error: substr(res.answer, 7) };
	return { data: res.answer };
}

// Starts a calibration session for the link. load_s is the load time of
// each block in seconds (the program checks the range).
function calibrate_start(args) {
	const link = args?.link, load = args?.load_s;
	if (type(link) != 'string' || !match(link, LINK_NAME_RE))
		return { error: 'Invalid link name' };
	if (load != null && (type(load) != 'int' || load < 1 || load > 3600))
		return { error: 'Invalid load time' };
	return command('calibrate start ' + link + (load != null ? ' ' + load : ''));
}

// Returns the status of the calibration as a JSON text in "data".
function calibrate_status() {
	const res = exchange('calibrate status');
	if (res.error)
		return res;
	if (substr(res.answer, 0, 7) == 'error: ')
		return { error: substr(res.answer, 7) };
	return { data: res.answer };
}

// Returns the kind of the root qdisc of the interface, for example "cake"
// or "fq_codel".
function qdisc(args) {
	const iface = args?.interface;
	if (type(iface) != 'string' || !match(iface, IFACE_RE))
		return { error: 'Invalid interface name' };
	const p = popen('tc qdisc show dev ' + iface + ' root 2>&1');
	if (!p)
		return { error: 'Cannot run tc' };
	const out = p.read('all');
	const status = p.close();
	const m = match(out, /^qdisc ([^ \n]+) /);
	if (status != 0 || !m)
		return { error: trim(out) || 'tc failed' };
	return { kind: m[1] };
}

return {
	'cake-autorate': {
		history: {
			call: function() {
				return history();
			}
		},
		calibrate_start: {
			args: { link: 'link', load_s: 0 },
			call: function(req) {
				return calibrate_start(req.args);
			}
		},
		calibrate_status: {
			call: function() {
				return calibrate_status();
			}
		},
		calibrate_stop: {
			call: function() {
				return command('calibrate stop');
			}
		},
		qdisc: {
			args: { interface: 'interface' },
			call: function(req) {
				return qdisc(req.args);
			}
		},
		pause: {
			args: { link: 'link', dir: 'dir' },
			call: function(req) {
				return pause_resume('pause', req.args);
			}
		},
		resume: {
			args: { link: 'link', dir: 'dir' },
			call: function(req) {
				return pause_resume('resume', req.args);
			}
		}
	}
};
