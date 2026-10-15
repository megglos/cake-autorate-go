// SPDX-License-Identifier: GPL-2.0
//
// rpcd plugin of luci-app-cake-autorate-go. It sends the commands "pause"
// and "resume" to the control socket of cake-autorate-go. The ACL of the
// app gives these methods only to the users that can write the
// configuration.

'use strict';

import * as socket from 'socket';

const CONTROL_SOCKET = '/var/run/cake-autorate/control.sock';

// The rule of the program for link names (config.go).
const LINK_NAME_RE = /^[A-Za-z0-9_.-]{1,64}$/;

// The time in seconds that the program has to answer.
const TIMEOUT = 5;

// Sends one command line and returns the answer line, or an error text.
function command(line) {
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

	let answer = null;
	if (s.send(line + '\n') != null)
		answer = s.recv(256);
	// Get the error before close: a close without error clears it.
	const err = socket.error();
	s.close();

	// An empty answer: the program closed the connection without an answer.
	if (answer == null || answer == '')
		return { error: 'No answer from the service' + (err ? ': ' + err : '') };
	answer = trim(answer);
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

return {
	'cake-autorate': {
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
