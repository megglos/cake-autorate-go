// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the calibration dialog on the status page, in the
// OpenWrt VM. Most tests replace the rpcd methods calibrate_* with a fake
// session, and the speed test endpoints with fast local answers: then the
// tests do not depend on the internet and do not change the shaper of the
// VM. One test runs a real session of the program and stops it.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/network/cake-autorate/status';

const RESULT = {
	link: 'wan',
	finished: '2026-10-16T10:00:00Z',
	download: { min_rate_kbps: 10000, base_rate_kbps: 27000, max_rate_kbps: 33000, owd_delta_delay_thr_ms: 30 },
	upload: { min_rate_kbps: 1500, base_rate_kbps: 13000, max_rate_kbps: 32500, owd_delta_delay_thr_ms: 31 },
	learn_cross_delay: true,
	ping_method: 'icmp-timestamp',
	reflectors: [ '9.9.9.9', '1.0.0.1', '8.8.4.4', '149.112.112.112' ],
	connection_active_thr_kbps: 750,
	why: { 'download rates': 'unshaped capacity (1 s means, 17 s): p95 32.9 Mbit/s' },
	notes: [ 'no autorate ul block: the upload grant delay is not measured' ],
	measured: {}
};

// A fake session: before the start, the state is "none". Then each status
// request gives the next status of steps. The last step stays.
// calibrate_stop gives the state "stopped".
function fakeSession(steps, started) {
	const s = { steps: steps, i: 0, starts: [], stops: 0, started: !!started };
	s.status = () => {
		if (!s.started)
			return { state: 'none', load: '' };
		const st = s.stopped ? { state: 'stopped', load: '' } : s.steps[Math.min(s.i, s.steps.length - 1)];
		s.i++;
		return st;
	};
	return s;
}

// A running status in a block and phase.
function running(block, mode, policy, phase, load, session) {
	return { state: 'running', session: session || 's1', link: 'wan', block: block, blocks: 5, mode: mode, policy: policy, phase: phase, load: load,
		phase_left_s: 10, left_s: 100, load_s: 40 };
}

// Routes the rpcd methods calibrate_* to the fake session. The other calls
// of the same batch go to the VM.
async function routeSession(page, session) {
	await page.route('**/ubus/**', async (route) => {
		const body = route.request().postDataJSON();
		const requests = Array.isArray(body) ? body : [ body ];
		const isCal = (q) => q && q.params && q.params[1] == 'cake-autorate' && /^calibrate_/.test(q.params[2]);
		if (!requests.some(isCal))
			return route.continue();
		const others = requests.filter((q) => !isCal(q));
		let answers = [];
		if (others.length) {
			try {
				const res = await route.fetch({ postData: JSON.stringify(others) });
				answers = await res.json();
				if (!Array.isArray(answers))
					answers = [ answers ];
			}
			catch (e) {
				return;
			}
		}
		for (const q of requests.filter(isCal)) {
			let result;
			switch (q.params[2]) {
			case 'calibrate_start':
				session.starts.push(q.params[3]);
				session.started = true;
				result = { result: 'ok' };
				break;
			case 'calibrate_stop':
				session.stops++;
				if (session.stopError) {
					result = { error: session.stopError };
					break;
				}
				session.stopped = true;
				result = { result: 'ok' };
				break;
			case 'calibrate_status':
				session.polls = (session.polls || 0) + 1;
				result = { data: JSON.stringify(session.status()) };
				break;
			default:
				result = { data: JSON.stringify(session.status()) };
			}
			answers.push({ jsonrpc: '2.0', id: q.id, result: [ 0, result ] });
		}
		await route.fulfill({ json: Array.isArray(body) ? answers : answers[0] });
	});
}

// Answers the speed test endpoints locally and counts the requests.
async function routeLoad(page) {
	const load = { down: 0, up: 0 };
	await page.route('https://speed.cloudflare.com/**', async (route) => {
		const req = route.request();
		if (req.method() == 'OPTIONS')
			return route.fulfill({ status: 204, headers: { 'access-control-allow-origin': '*', 'access-control-allow-headers': 'content-type' } });
		if (req.url().includes('__down'))
			load.down++;
		else
			load.up++;
		// A short delay, so that the loops of the page do not spin.
		await new Promise((resolve) => setTimeout(resolve, 100));
		try {
			await route.fulfill({ status: 200, headers: { 'access-control-allow-origin': '*' }, body: 'x'.repeat(1000) });
		}
		catch (e) {
			// The page aborted the request.
		}
	});
	return load;
}

// Returns the number of requests of each kind during ms milliseconds.
async function countDuring(page, load, ms) {
	const before = { down: load.down, up: load.up };
	await page.waitForTimeout(ms);
	return { down: load.down - before.down, up: load.up - before.up };
}

let errors;

test.beforeAll(() => {
	resetConfig();
});

test.beforeEach(async ({ page }) => {
	await login(page);
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
});

test.afterEach(() => {
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('the dialog shows the time and the data volume, and starts the session', async ({ page }) => {
	const session = fakeSession([ running(1, 'dl', 'unshaped', 'idle', '') ]);
	await routeSession(page, session);
	await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	const modal = page.locator('.modal');
	await expect(modal).toContainText('Calibrate the link wan');
	// The test link: 80 and 35 Mbit/s maximum. 40 s: 3 blocks of each
	// direction at the maximum: 40 * 3 * (10 + 4.375) MB = 1.7 GB.
	await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 1.7 GB');
	await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 5 minutes');
	// The default data limit is 1.5 times the volume, up to the next 100 MB.
	await expect(modal.locator('#cake-autorate-calibrate-limit')).toHaveValue('2600');
	await modal.locator('#cake-autorate-calibrate-load').selectOption('20');
	await expect(modal.locator('.cake-autorate-estimate')).toContainText(/approximately 86[23] MB/);
	await expect(modal.locator('#cake-autorate-calibrate-limit')).toHaveValue('1300');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('.cake-autorate-block')).toContainText('Block 1 of 5');
	await expect(modal.locator('.cake-autorate-used')).toContainText('Data: 0 MB of 1.3 GB.');
	expect(session.starts).toEqual([ { link: 'wan', load_s: 20 } ]);
});

test('the load stops the session at the data limit', async ({ page }) => {
	const session = fakeSession([ running(3, 'ul', 'unshaped', 'load', 'ul') ]);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	const modal = page.locator('.modal');
	// One upload is 4 MiB: more than the limit of 1 MB.
	await modal.locator('#cake-autorate-calibrate-limit').fill('1');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The calibration stopped: the load reached the data limit of 1 MB.', { timeout: 10000 });
	expect(session.stops).toBe(1);
	expect(load.up).toBe(0);
});

test('the browser makes the load that the session asks for', async ({ page }) => {
	// Each status is shown approximately one second.
	const steps = [];
	const add = (n, st) => { for (let i = 0; i < n; i++) steps.push(st); };
	add(3, running(1, 'dl', 'unshaped', 'idle', ''));
	add(4, running(1, 'dl', 'unshaped', 'load', 'dl'));
	add(3, running(2, 'dl', 'autorate', 'tail', ''));
	add(4, running(3, 'ul', 'unshaped', 'load', 'ul'));
	add(4, running(5, 'both', 'unshaped', 'load', 'both'));
	add(30, running(5, 'both', 'unshaped', 'tail', ''));
	const session = fakeSession(steps);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	const block = page.locator('.modal .cake-autorate-block');
	const phase = async (text) => {
		await expect(block).toContainText(text, { timeout: 15000 });
		// The load of the phase starts with the status.
		await page.waitForTimeout(500);
		return countDuring(page, load, 1500);
	};

	let n = await phase('Block 1 of 5: download, without a shaper limit. Phase: no load');
	expect(n).toEqual({ down: 0, up: 0 });
	n = await phase('Block 1 of 5: download, without a shaper limit. Phase: load');
	expect(n.down).toBeGreaterThan(0);
	expect(n.up).toBe(0);
	await expect(page.locator('.modal .cake-autorate-load')).toContainText('Load from this browser: download');
	n = await phase('Block 2 of 5: download, with the rate control. Phase: end of the block');
	expect(n).toEqual({ down: 0, up: 0 });
	n = await phase('Block 3 of 5: upload');
	expect(n.down).toBe(0);
	expect(n.up).toBeGreaterThan(0);
	n = await phase('Block 5 of 5: download and upload');
	expect(n.down).toBeGreaterThan(0);
	expect(n.up).toBeGreaterThan(0);
	n = await phase('Block 5 of 5: download and upload, without a shaper limit. Phase: end of the block');
	expect(n).toEqual({ down: 0, up: 0 });
});

test('Stop ends the session and the load', async ({ page }) => {
	const session = fakeSession([ running(1, 'dl', 'unshaped', 'load', 'dl') ]);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	await expect.poll(() => load.down, { timeout: 10000 }).toBeGreaterThan(0);
	await page.locator('.modal').getByRole('button', { name: 'Stop' }).click();
	await expect(page.locator('.modal')).toContainText('The calibration stopped.');
	expect(session.stops).toBe(1);
	await page.waitForTimeout(500);
	expect(await countDuring(page, load, 1500)).toEqual({ down: 0, up: 0 });
});

test('the result shows the proposal and applies the selected values', async ({ page }) => {
	const session = fakeSession([ running(5, 'both', 'unshaped', 'tail', ''), { state: 'done', link: 'wan', load: '', result: RESULT } ]);
	await routeSession(page, session);
	await routeLoad(page);
	// The global section can have another name.
	vm('uci rename cake-autorate.global=main && uci commit cake-autorate');
	try {
		await page.goto(PAGE);
		await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
		await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
		const table = page.locator('.modal table.cake-autorate-result');
		await expect(table).toBeVisible({ timeout: 10000 });
		const row = (key) => table.locator(`tr[data-key="${key}"]`);
		await expect(row('dl_max_rate_kbps')).toContainText('80000');
		await expect(row('dl_max_rate_kbps')).toContainText('33000');
		await expect(row('dl_max_rate_kbps')).toContainText('p95 32.9 Mbit/s');
		await expect(row('ul_owd_delta_delay_thr_ms')).toContainText('31');
		await expect(row('ul_owd_delta_delay_thr_ms')).toContainText('30 (default)');
		await expect(row('ul_owd_delta_delay_thr_ms').locator('input[type=checkbox]')).toBeChecked();
		// The proposal is the same as the default value: no change, not selected.
		await expect(row('dl_owd_delta_delay_thr_ms')).toContainText('30 (default)');
		await expect(row('dl_owd_delta_delay_thr_ms')).toContainText('(no change)');
		await expect(row('dl_owd_delta_delay_thr_ms').locator('input[type=checkbox]')).not.toBeChecked();
		await expect(row('ping_method')).toContainText('icmp (global)');
		await expect(row('reflectors')).toContainText('9.9.9.9 1.0.0.1 8.8.4.4 149.112.112.112');
		await expect(page.locator('.modal')).toContainText('the upload grant delay is not measured');
		await expect(row('connection_active_thr_kbps')).toContainText('2000 (default)');
		await expect(row('connection_active_thr_kbps')).toContainText('750');
		// The reflectors of the link do not reply to ICMP timestamps: the
		// ping method needs the proposed reflectors.
		await row('reflectors').locator('input[type=checkbox]').uncheck();
		await page.locator('.modal').getByRole('button', { name: 'Apply the selected values' }).click();
		await expect(page.locator('.modal .alert-message.danger')).toContainText('no reflector of the list replies to ICMP timestamp requests');
		expect(vm('uci -q get cake-autorate.wan.dl_max_rate_kbps').trim()).toBe('80000');
		// Do not apply the ping method and the reflectors.
		await row('ping_method').locator('input[type=checkbox]').uncheck();
		await page.locator('.modal').getByRole('button', { name: 'Apply the selected values' }).click();
		await expect.poll(() => vm('uci -q get cake-autorate.wan.dl_max_rate_kbps || true').trim(), { timeout: 30000 }).toBe('33000');
		expect(vm('uci -q get cake-autorate.wan.ul_base_rate_kbps').trim()).toBe('13000');
		expect(vm('uci -q get cake-autorate.wan.ping_method || true').trim()).toBe('');
		expect(vm('uci -q get cake-autorate.main.connection_active_thr_kbps').trim()).toBe('750');
		expect(vm('uci -q get cake-autorate.wan.learn_cross_delay').trim()).toBe('1');
		expect(vm('uci -q get cake-autorate.wan.ul_owd_delta_delay_thr_ms').trim()).toBe('31');
		expect(vm('uci -q get cake-autorate.wan.dl_owd_delta_delay_thr_ms || true').trim()).toBe('');
		expect(vm('uci -q get cake-autorate.wan.reflectors').trim()).toBe('1.1.1.1 8.8.8.8');
	}
	finally {
		resetConfig();
	}
});

test('Apply refuses rates that are not in the order minimum, base, maximum', async ({ page }) => {
	const result = JSON.parse(JSON.stringify(RESULT));
	result.download.max_rate_kbps = 15000; // below the base rate of the link (20000)
	const session = fakeSession([ { state: 'done', link: 'wan', load: '', result: result } ]);
	await routeSession(page, session);
	await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	const table = page.locator('.modal table.cake-autorate-result');
	await expect(table).toBeVisible({ timeout: 10000 });
	for (const key of [ 'dl_min_rate_kbps', 'dl_base_rate_kbps' ])
		await table.locator(`tr[data-key="${key}"] input[type=checkbox]`).uncheck();
	await page.locator('.modal').getByRole('button', { name: 'Apply the selected values' }).click();
	await expect(page.locator('.modal .alert-message.danger')).toContainText('Download: the selected rates give minimum 5000, base 20000, maximum 15000 kbit/s');
	await expect(table).toBeVisible();
	expect(vm('uci -q get cake-autorate.wan.dl_max_rate_kbps').trim()).toBe('80000');
});

test('a reload of the page shows the running session again and keeps the data limit', async ({ page }) => {
	const session = fakeSession([ running(3, 'ul', 'unshaped', 'load', 'ul') ]);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal #cake-autorate-calibrate-limit').fill('100000');
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	await expect(page.locator('.modal .cake-autorate-block')).toContainText('Block 3 of 5', { timeout: 10000 });
	await page.reload();
	await expect(page.locator('.modal .cake-autorate-block')).toContainText('Block 3 of 5', { timeout: 10000 });
	await expect(page.locator('.modal .cake-autorate-used')).toContainText('of 100.0 GB');
	await expect.poll(() => load.up, { timeout: 10000 }).toBeGreaterThan(0);
});

test('a session that this page did not start makes no load before the user sets a data limit', async ({ page }) => {
	// A session that runs before the page opens (for example, from another
	// browser): no data limit in the session storage.
	const session = fakeSession([ running(3, 'ul', 'unshaped', 'load', 'ul') ], true);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	const modal = page.locator('.modal');
	await expect(modal).toContainText('this page did not start it', { timeout: 10000 });
	const polls = session.polls || 0;
	await page.waitForTimeout(2000);
	expect(load.up).toBe(0);
	// No status requests: the heartbeat of the session is not from this page.
	expect(session.polls || 0).toBe(polls);
	await modal.locator('#cake-autorate-calibrate-takeover-limit').fill('5000');
	await modal.getByRole('button', { name: 'Continue the load' }).click();
	await expect(modal.locator('.cake-autorate-used')).toContainText('of 5.0 GB', { timeout: 10000 });
	await expect.poll(() => load.up, { timeout: 10000 }).toBeGreaterThan(0);
});

test('the saved data limit of an older session does not start the load of a new session', async ({ page }) => {
	const session = fakeSession([ running(3, 'ul', 'unshaped', 'load', 'ul', 'A') ]);
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal #cake-autorate-calibrate-limit').fill('100000');
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	await expect(page.locator('.modal .cake-autorate-block')).toContainText('Block 3 of 5', { timeout: 10000 });
	// Session A ends, and another client starts session B of the link.
	session.steps = [ running(1, 'dl', 'unshaped', 'load', 'dl', 'B') ];
	await page.reload();
	const modal = page.locator('.modal');
	await expect(modal).toContainText('this page did not start it', { timeout: 10000 });
	const down = load.down;
	await page.waitForTimeout(2000);
	expect(load.down).toBe(down);
});

test('the Stop button keeps the keyboard focus during the refreshes', async ({ page }) => {
	const session = fakeSession([ running(1, 'dl', 'unshaped', 'idle', '') ]);
	await routeSession(page, session);
	await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	const stop = page.locator('.modal').getByRole('button', { name: 'Stop' });
	await stop.focus();
	const polls = session.polls;
	await expect.poll(() => session.polls, { timeout: 10000 }).toBeGreaterThan(polls + 2);
	await expect(stop).toBeFocused();
});

test('a failed stop ends the load and the status requests', async ({ page }) => {
	const session = fakeSession([ running(1, 'dl', 'unshaped', 'load', 'dl') ]);
	session.stopError = 'Command failed';
	await routeSession(page, session);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
	await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
	await expect.poll(() => load.down, { timeout: 10000 }).toBeGreaterThan(0);
	await page.locator('.modal').getByRole('button', { name: 'Stop' }).click();
	await expect(page.locator('.modal')).toContainText('The stop failed: Command failed.');
	const polls = session.polls;
	await page.waitForTimeout(500);
	expect(await countDuring(page, load, 2000)).toEqual({ down: 0, up: 0 });
	expect(session.polls).toBe(polls);
});

test('a session of the program starts and stops from the dialog', async ({ page }) => {
	await routeLoad(page);
	try {
		await page.goto(PAGE);
		await page.locator('.cbi-section[data-link="wan"] .cake-autorate-calibrate').click();
		await page.locator('.modal').getByRole('button', { name: 'Start' }).click();
		await expect(page.locator('.modal .cake-autorate-block')).toContainText('Block 1 of 5', { timeout: 10000 });
		await expect.poll(() => vm('jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].calibrating"').trim(),
			{ timeout: 10000 }).toBe('true');
		await page.locator('.modal').getByRole('button', { name: 'Stop' }).click();
		await expect(page.locator('.modal')).toContainText('The calibration stopped.', { timeout: 10000 });
		await expect.poll(() => vm('jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].calibrating"').trim(),
			{ timeout: 10000 }).toBe('false');
	}
	finally {
		vm('echo "calibrate stop" | ucode -e \'import * as s from "socket"; let c = s.connect("/var/run/cake-autorate/control.sock"); c.send("calibrate stop\\n"); c.recv(256);\' 2>/dev/null; true');
	}
});
