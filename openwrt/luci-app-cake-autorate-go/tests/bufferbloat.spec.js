// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the bufferbloat test of the status page in the OpenWrt
// VM. The page answers the speed test requests locally. The delay comes
// from the status file of the program; some tests change it.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/network/cake-autorate/status';

// Short phases: 2 s each, without warm-up.
const PHASES = [
	{ key: 'idle', load: '', seconds: 2 },
	{ key: 'dl', load: 'dl', seconds: 2 },
	{ key: 'ul', load: 'ul', seconds: 2 },
	{ key: 'both', load: 'both', seconds: 2 }
];

// Answers the speed test endpoints locally and counts the requests. With
// status, each answer has this HTTP status.
async function routeLoad(page, status) {
	const load = { down: 0, up: 0 };
	await page.route('https://speed.cloudflare.com/**', async (route) => {
		const req = route.request();
		if (req.method() == 'OPTIONS')
			return route.fulfill({ status: 204, headers: { 'access-control-allow-origin': '*', 'access-control-allow-headers': 'content-type' } });
		if (req.url().includes('__down'))
			load.down++;
		else
			load.up++;
		await new Promise((resolve) => setTimeout(resolve, 100));
		try {
			await route.fulfill({ status: status || 200, headers: { 'access-control-allow-origin': '*' }, body: 'x'.repeat(100000) });
		}
		catch (e) {
			// The page aborted the request.
		}
	});
	return load;
}

// The calls of pin and unpin of the last routeUbus.
let pinCalls = [];

// Changes the status file that the page reads (the file on the router
// does not change), and records the calls of unshape and shape. It
// answers pin and unpin itself (the routing of the VM does not change)
// and records them in pinCalls. With pinError, pin answers this error.
// Each status read gets one more ping reply ("replies"), as a link with
// pings; mutate(status, asked) can change it. asked is the time of the
// request (Date.now()).
async function routeUbus(page, mutate, failShape, statusDelay, pinError) {
	const calls = [];
	pinCalls = [];
	let replies = 1000;
	await page.route('**/ubus/**', async (route) => {
		const request = route.request().postDataJSON();
		const requests = Array.isArray(request) ? request : [ request ];
		for (const q of requests)
			if (q && q.params && q.params[1] == 'cake-autorate' && /^(un)?shape$/.test(q.params[2]))
				calls.push([ q.params[2], q.params[3] ]);
		const isPin = (q) => q && q.params && q.params[1] == 'cake-autorate' && /^(un)?pin$/.test(q.params[2]);
		if (requests.some(isPin)) {
			// The page sends pin and unpin alone (not in a batch).
			const answers = requests.map((q) => {
				pinCalls.push([ q.params[2], q.params[3] ]);
				const result = (pinError && q.params[2] == 'pin') ? { error: pinError } : { result: 'ok' };
				return { jsonrpc: '2.0', id: q.id, result: [ 0, result ] };
			});
			return route.fulfill({ json: Array.isArray(request) ? answers : answers[0] });
		}
		// "shape" fails: an answer with an error, the router gets nothing.
		const shape = requests.find((q) => q && q.params && q.params[2] == 'shape');
		if (failShape && shape && requests.length == 1) {
			const answer = { jsonrpc: '2.0', id: shape.id, result: [ 0, { error: 'tc failed' } ] };
			return route.fulfill({ json: Array.isArray(request) ? [ answer ] : answer });
		}
		const asked = Date.now();
		const readsStatus = requests.some((q) => q && q.params && q.params[1] == 'file' && q.params[3] && q.params[3].path == '/var/run/cake-autorate/status.json');
		// statusDelay is a time in ms, or a function of the request time.
		const delay = typeof statusDelay == 'function' ? statusDelay(asked) : statusDelay;
		if (delay && readsStatus)
			await new Promise((resolve) => setTimeout(resolve, delay));
		// After a delay, the page can be closed: then the request, its
		// answer and the route are gone. Ignore the errors that this causes.
		try {
			const response = await route.fetch();
			const body = await response.json();
			for (const res of Array.isArray(body) ? body : [ body ]) {
				const p = (requests.find((q) => q && q.id === res.id) || {}).params;
				const data = res.result && res.result[1] && res.result[1].data;
				if (p && p[1] == 'file' && p[2] == 'read' && p[3] && p[3].path == '/var/run/cake-autorate/status.json' && data) {
					const status = JSON.parse(data);
					for (const ls of status.links || [])
						ls.replies = replies;
					replies++;
					if (mutate)
						mutate(status, asked);
					res.result[1].data = JSON.stringify(status);
				}
			}
			await route.fulfill({ response, json: body });
		}
		catch (e) {
			if (!page.isClosed())
				throw e;
		}
	});
	return calls;
}

// Opens the status page with short phases and opens the dialog.
async function openDialog(page) {
	await page.goto(PAGE);
	await page.evaluate((phases) => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = phases;
		b.warmup = 0;
		b.statusLag = 0;
	}), PHASES);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	return page.locator('.modal');
}

// Returns the "unshaped" value of the link in the status file.
function unshapedInFile() {
	return vm('jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].unshaped" 2>/dev/null || true').trim();
}

let errors;

test.beforeAll(() => {
	resetConfig();
});

test.afterAll(() => {
	resetConfig();
});

test.beforeEach(async ({ page }) => {
	await login(page);
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
});

test.afterEach(async ({ page }) => {
	// A route handler can still wait (a delayed status answer) when the
	// test ends. Remove the routes and ignore the errors of these handlers:
	// else the end of the page makes the test fail.
	await page.unrouteAll({ behavior: 'ignoreErrors' });
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('the test measures the delay in each phase and gives a grade', async ({ page }) => {
	await routeUbus(page, (s) => {
		Object.assign(s.links[0], { state: 'RUNNING' });
		// The load goes through the link.
		Object.assign(s.links[0].download, { delta_ms: 12, achieved_kbps: 1e7 });
		Object.assign(s.links[0].upload, { delta_ms: 40, achieved_kbps: 1e7 });
	});
	const load = await routeLoad(page);
	const modal = await openDialog(page);
	await expect(modal).toContainText('Bufferbloat test of the link wan');
	await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 8 s');
	await expect(modal.locator('.cake-autorate-estimate')).toContainText('uses more data');
	// No data limit: the test is short, and the user can stop it.
	await expect(modal.locator('input[type=number]')).toHaveCount(0);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('.cake-autorate-phase')).toContainText('Run 1 of 1 (With shaping)');
	const table = modal.locator('table.cake-autorate-bufferbloat-result');
	await expect(table).toHaveCount(1, { timeout: 20000 });
	await expect(table.locator('tr[data-phase]')).toHaveCount(4);
	await expect(table.locator('tr[data-phase="idle"]')).toContainText('12.0 ms (p95 12.0)');
	await expect(table.locator('tr[data-phase="idle"] [data-grade]')).toHaveCount(0);
	for (const key of [ 'dl', 'ul', 'both' ]) {
		// The larger median is 40 ms: grade B.
		await expect(table.locator(`tr[data-phase="${key}"] [data-grade]`)).toHaveText('B');
		await expect(table.locator(`tr[data-phase="${key}"]`)).toContainText('40.0 ms (p95 40.0)');
	}
	await expect(table.locator('tr[data-phase="dl"]')).toContainText('Mbit/s');
	expect(load.down).toBeGreaterThan(0);
	expect(load.up).toBeGreaterThan(0);
});

test('a link without new ping replies shows no delay', async ({ page }) => {
	// RUNNING, but the reflectors do not reply: the delay deltas are old.
	await routeUbus(page, (s) => {
		Object.assign(s.links[0], { state: 'RUNNING', replies: 7 });
		Object.assign(s.links[0].download, { delta_ms: 0.1, achieved_kbps: 1e7 });
		s.links[0].upload.achieved_kbps = 1e7;
	});
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	const table = modal.locator('table.cake-autorate-bufferbloat-result');
	await expect(table).toHaveCount(1, { timeout: 20000 });
	await expect(table.locator('tr[data-phase="dl"]')).toContainText('no ping replies');
	await expect(table.locator('[data-grade]')).toHaveCount(0);
});

test('the comparison turns the shaping off for the second run, with a time limit', async ({ page }) => {
	const calls = await routeUbus(page);
	await routeLoad(page);
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		// The run with shaping has no phase without load: 6 s, 3 s pause and
		// 8 s.
		await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 17 s');
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('.cake-autorate-phase')).toContainText('Run 2 of 2 (Without shaping)', { timeout: 20000 });
		await expect.poll(() => unshapedInFile(), { timeout: 10000 }).toBe('true');
		const tables = modal.locator('table.cake-autorate-bufferbloat-result');
		await expect(tables).toHaveCount(2, { timeout: 20000 });
		await expect(modal.locator('#cake-autorate-bufferbloat h4')).toHaveText([ 'With shaping', 'Without shaping' ]);
		// Only the run without shaping has the phase without load.
		await expect(tables.nth(0).locator('tr[data-phase]')).toHaveCount(3);
		await expect(tables.nth(0).locator('tr[data-phase="idle"]')).toHaveCount(0);
		await expect(tables.nth(1).locator('tr[data-phase="idle"]')).toHaveCount(1);
		// The time limit: the run (8 s) and 60 s.
		expect(calls).toEqual([ [ 'unshape', { link: 'wan', seconds: 68 } ], [ 'shape', { link: 'wan' } ] ]);
		await expect.poll(() => unshapedInFile(), { timeout: 10000 }).toBe('false');
	}
	finally {
		resetConfig();
	}
});

test('Stop in the second run turns the shaping on again', async ({ page }) => {
	const calls = await routeUbus(page);
	await routeLoad(page);
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('.cake-autorate-phase')).toContainText('Run 2 of 2', { timeout: 20000 });
		await expect.poll(() => unshapedInFile(), { timeout: 10000 }).toBe('true');
		await modal.getByRole('button', { name: 'Stop' }).click();
		await expect(modal).toContainText('The test stopped.', { timeout: 10000 });
		await expect.poll(() => unshapedInFile(), { timeout: 10000 }).toBe('false');
		expect(calls.map((c) => c[0])).toEqual([ 'unshape', 'shape' ]);
	}
	finally {
		resetConfig();
	}
});

test('the button is disabled while the shaping is off', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].unshaped = true; });
	await page.goto(PAGE);
	await expect(page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat')).toBeDisabled();
});

test('Stop in the pause between the runs does not change the shaping', async ({ page }) => {
	const calls = await routeUbus(page);
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.locator('#cake-autorate-bufferbloat-compare').check();
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('.cake-autorate-phase')).toContainText('Pause between the runs', { timeout: 20000 });
	await modal.getByRole('button', { name: 'Stop' }).click();
	await expect(modal).toContainText('The test stopped.', { timeout: 10000 });
	expect(calls).toEqual([]);
});

test('a failed "shape" after the test shows an error', async ({ page }) => {
	await routeUbus(page, null, true);
	await routeLoad(page);
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('[data-shape-error]')).toContainText('The shaping did not come back: tc failed', { timeout: 30000 });
	}
	finally {
		resetConfig();
	}
});

test('a failed load stops the test without grades', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; });
	await routeLoad(page, 403);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The test stopped with an error: The load failed: HTTP 403', { timeout: 20000 });
	await expect(modal.locator('[data-grade]')).toHaveCount(0);
});

test('the upload rate counts the uploads while they run', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; });
	await routeLoad(page);
	// A request to the upload endpoint gets its answer after 20 s (this
	// route comes after the other one: Playwright uses it first).
	await page.route('https://speed.cloudflare.com/__up', async (route) => {
		if (route.request().method() == 'OPTIONS')
			return route.fulfill({ status: 204, headers: { 'access-control-allow-origin': '*', 'access-control-allow-headers': 'content-type' } });
		await new Promise((resolve) => setTimeout(resolve, 20000));
		await route.fulfill({ status: 200, headers: { 'access-control-allow-origin': '*' }, body: '' }).catch(() => {});
	});
	// A slow uplink: an upload takes 20 s, longer than the phase. Chromium
	// sends no upload progress for a request that Playwright answers, thus
	// a fake XMLHttpRequest for the upload endpoint reports the progress
	// (100 kB each 100 ms). The other requests (LuCI) use the real one.
	await page.addInitScript(() => {
		const proto = XMLHttpRequest.prototype;
		const open = proto.open, send = proto.send, abort = proto.abort;
		proto.open = function(method, url, ...rest) {
			this.fakeUpload = String(url).includes('speed.cloudflare.com/__up');
			if (!this.fakeUpload)
				return open.call(this, method, url, ...rest);
		};
		proto.send = function(body) {
			if (!this.fakeUpload)
				return send.call(this, body);
			let loaded = 0;
			this.fakeTimer = setInterval(() => {
				loaded = Math.min(body.size, loaded + 100000);
				if (this.upload.onprogress)
					this.upload.onprogress({ loaded: loaded });
			}, 100);
		};
		proto.abort = function() {
			if (!this.fakeUpload)
				return abort.call(this);
			clearInterval(this.fakeTimer);
			if (this.onabort)
				this.onabort();
		};
	});
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	const ul = modal.locator('table.cake-autorate-bufferbloat-result tr[data-phase="ul"] td').nth(2);
	await expect(ul).toContainText('Mbit/s', { timeout: 30000 });
	// 4 uploads with up to 1 MB/s each (the timers of a busy page are
	// slower). The old count gave 0.
	expect(parseFloat(await ul.textContent())).toBeGreaterThan(1);
});

test('slow status requests do not make the phases longer', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; }, false, 3000);
	await routeLoad(page);
	const modal = await openDialog(page);
	const start = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('table.cake-autorate-bufferbloat-result')).toHaveCount(1, { timeout: 60000 });
	// 4 phases of 2 s, and some time for the page.
	expect(Date.now() - start).toBeLessThan(16000);
});

test('the run without shaping waits until the status shows the shaping off', async ({ page }) => {
	// The status always shows a shaped link.
	await routeUbus(page, (s) => { s.links[0].unshaped = false; });
	await routeLoad(page);
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal).toContainText('The status does not show the shaping off.', { timeout: 30000 });
		await expect(modal.locator('#cake-autorate-bufferbloat h4')).toHaveText([ 'With shaping' ]);
	}
	finally {
		resetConfig();
	}
});

test('the run without shaping stops when the shaping comes back', async ({ page }) => {
	// After 3 status reads with the shaping off, the status shows it on.
	let off = 0;
	await routeUbus(page, (s) => {
		if (s.links[0].unshaped && ++off > 3)
			s.links[0].unshaped = false;
	});
	await routeLoad(page);
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal).toContainText('The shaping came back before the end of the run without shaping.', { timeout: 30000 });
	}
	finally {
		resetConfig();
	}
});

test('after a failed "shape", the next test does not send "shape"', async ({ page }) => {
	const calls = await routeUbus(page, null, true);
	await routeLoad(page);
	try {
		let modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('[data-shape-error]')).toBeVisible({ timeout: 30000 });
		await modal.getByRole('button', { name: 'Close' }).click();
		calls.length = 0;
		// The link is still unshaped: the button is disabled. Open the
		// dialog from the module.
		await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => b.open({ name: 'wan', download: {}, upload: {} })));
		modal = page.locator('.modal');
		await modal.getByRole('button', { name: 'Start' }).click();
		// The shaping is still off: the run with shaping does not start.
		await expect(modal).toContainText('The shaping is off: the run with shaping needs it on.', { timeout: 30000 });
		expect(calls).toEqual([]);
	}
	finally {
		resetConfig();
	}
});

test('a failed "unshape" turns the shaping on again', async ({ page }) => {
	const calls = await routeUbus(page);
	await routeLoad(page);
	// "unshape" answers with an error (as after a failed rate change).
	await page.route('**/ubus/**', async (route) => {
		const request = route.request().postDataJSON();
		const q = (Array.isArray(request) ? request : [ request ]).find((r) => r && r.params && r.params[2] == 'unshape');
		if (!q)
			return route.fallback();
		calls.push([ 'unshape', q.params[3] ]);
		const answer = { jsonrpc: '2.0', id: q.id, result: [ 0, { error: 'tc failed' } ] };
		return route.fulfill({ json: Array.isArray(request) ? [ answer ] : answer });
	});
	try {
		const modal = await openDialog(page);
		await modal.locator('#cake-autorate-bufferbloat-compare').check();
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal).toContainText('The test stopped with an error: tc failed', { timeout: 30000 });
		expect(calls.map((c) => c[0])).toEqual([ 'unshape', 'shape' ]);
	}
	finally {
		resetConfig();
	}
});

test('the Stop button shows before the first status answer', async ({ page }) => {
	await routeUbus(page, null, false, 5000);
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.getByRole('button', { name: 'Stop' })).toBeVisible({ timeout: 1500 });
});

test('the run with shaping stops when the shaping is off', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].unshaped = true; });
	await routeLoad(page);
	// The button is disabled for an unshaped link (another client can turn
	// the shaping off after the dialog opens): open the dialog from the
	// module.
	await page.goto(PAGE);
	await page.evaluate((phases) => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = phases;
		b.warmup = 0;
		b.statusLag = 0;
		b.open({ name: 'wan', download: {}, upload: {} });
	}), PHASES);
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The shaping is off: the run with shaping needs it on.', { timeout: 20000 });
});

test('a status that was asked in the warm-up is not a sample', async ({ page }) => {
	// Status answers come 2.5 s late. The delay is 100 ms for a status
	// asked in the first 3 s of the phase (the warm-up), then 10 ms.
	let t0 = null;
	await routeUbus(page, (s, asked) => {
		Object.assign(s.links[0], { state: 'RUNNING' });
		const warm = t0 == null || asked - t0 < 3000;
		s.links[0].download.delta_ms = s.links[0].upload.delta_ms = warm ? 100 : 10;
	}, false, 2500);
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'dl', load: 'dl', seconds: 9 } ];
		b.warmup = 3;
		b.statusLag = 0;
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	t0 = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	const dl = modal.locator('table.cake-autorate-bufferbloat-result tr[data-phase="dl"]');
	await expect(dl).toContainText('ms (p95', { timeout: 30000 });
	await expect(dl).toContainText('10.0 ms (p95 10.0)');
});

test('the test stops when a calibration runs', async ({ page }) => {
	await routeUbus(page, (s) => { Object.assign(s.links[0], { state: 'RUNNING', calibrating: true }); });
	await routeLoad(page);
	// The button is disabled during a calibration (it can start after the
	// dialog opens): open the dialog from the module.
	await page.goto(PAGE);
	await page.evaluate((phases) => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = phases;
		b.warmup = 0;
		b.statusLag = 0;
		b.open({ name: 'wan', download: {}, upload: {} });
	}), PHASES);
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('A calibration of the link runs.', { timeout: 20000 });
	await expect(modal.locator('[data-grade]')).toHaveCount(0);
});

test('a load that transfers no data stops the test', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; });
	await routeLoad(page);
	// The downloads hang: no answer, no error.
	await page.route('https://speed.cloudflare.com/__down*', () => new Promise(() => {}));
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The load did not transfer data (download).', { timeout: 20000 });
	await expect(modal.locator('[data-grade]')).toHaveCount(0);
});

test('the Stop button keeps the keyboard focus during the refreshes', async ({ page }) => {
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; });
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'idle', load: '', seconds: 8 } ];
		b.warmup = 0;
		b.statusLag = 0;
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	const stop = modal.getByRole('button', { name: 'Stop' });
	await stop.focus();
	await page.waitForTimeout(3500);
	await expect(stop).toBeFocused();
});

test('the rates count from the end of the warm-up, also with slow status requests', async ({ page }) => {
	// Status answers come 5 s late: no status answer in the phase after
	// the warm-up.
	await routeUbus(page, (s) => { s.links[0].state = 'RUNNING'; }, false, 5000);
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'dl', load: 'dl', seconds: 6 } ];
		b.warmup = 1;
		b.statusLag = 0;
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	const rate = modal.locator('table.cake-autorate-bufferbloat-result tr[data-phase="dl"] td').nth(1);
	await expect(rate).toContainText('Mbit/s', { timeout: 30000 });
	expect(parseFloat(await rate.textContent())).toBeGreaterThan(0);
});

test('a load phase that does not use the link gets no grade', async ({ page }) => {
	// Another WAN link carries the browser traffic: the link has no rate.
	await routeUbus(page, (s) => {
		Object.assign(s.links[0], { state: 'RUNNING' });
		Object.assign(s.links[0].download, { delta_ms: 2, achieved_kbps: 0 });
		Object.assign(s.links[0].upload, { delta_ms: 2, achieved_kbps: 0 });
	});
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	const table = modal.locator('table.cake-autorate-bufferbloat-result');
	await expect(table).toHaveCount(1, { timeout: 20000 });
	await expect(table.locator('tr[data-phase="dl"]')).toContainText('load not on this link');
	await expect(table.locator('[data-grade]')).toHaveCount(0);
});

test('a load error during the last status request of a phase stops the test', async ({ page }) => {
	let t0 = Infinity;
	// After the first status (before the phase), no status answer comes in
	// the phase: the only status request of the phase waits until its end.
	await routeUbus(page, null, false, (asked) => asked - t0 > 500 ? 10000 : 0);
	await routeLoad(page);
	// The downloads fail while a status request of the test waits (more
	// than 300 ms): the error comes during the last status request.
	await page.route('https://speed.cloudflare.com/__down*', async (route) => {
		const waiting = await page.evaluate(() => window.readStart != null && performance.now() - window.readStart > 300).catch(() => false);
		if (!waiting)
			return route.fallback();
		await route.fulfill({ status: 403, headers: { 'access-control-allow-origin': '*' }, body: '' }).catch(() => {});
	});
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		// 1.8 s: the status request at 1 s waits until the end of the phase.
		b.phases = [ { key: 'dl', load: 'dl', seconds: 1.8 } ];
		b.warmup = 0;
		b.statusLag = 0;
		const read = b.readLink;
		b.readLink = function(...args) {
			window.readStart = performance.now();
			// A busy page: the answer comes 100 ms after the end of the
			// request, thus after the end of the phase.
			return read.apply(this, args)
				.then((v) => new Promise((resolve) => setTimeout(() => resolve(v), 100)))
				.finally(() => { window.readStart = null; });
		};
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	t0 = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The load failed: HTTP 403', { timeout: 20000 });
});

test('a status in Idle is not a sample, also with new replies', async ({ page }) => {
	// The reply counter increases (default of the mock), but the link is in
	// Idle: the delay delta is from before.
	await routeUbus(page, (s) => {
		s.links[0].state = 'IDLE';
		Object.assign(s.links[0].download, { delta_ms: 0.1, achieved_kbps: 1e7 });
		s.links[0].upload.achieved_kbps = 1e7;
	});
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	const table = modal.locator('table.cake-autorate-bufferbloat-result');
	await expect(table).toHaveCount(1, { timeout: 20000 });
	await expect(table.locator('tr[data-phase="dl"]')).toContainText('no ping replies');
	await expect(table.locator('[data-grade]')).toHaveCount(0);
});

test('a stopped phase is not in the result', async ({ page }) => {
	await routeUbus(page, (s) => {
		s.links[0].state = 'RUNNING';
		Object.assign(s.links[0].download, { delta_ms: 2, achieved_kbps: 1e7 });
	});
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'idle', load: '', seconds: 2 }, { key: 'dl', load: 'dl', seconds: 20 } ];
		b.warmup = 0;
		b.statusLag = 0;
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('.cake-autorate-phase')).toContainText('download', { timeout: 10000 });
	await page.waitForTimeout(3000);
	await modal.getByRole('button', { name: 'Stop' }).click();
	await expect(modal).toContainText('The test stopped.', { timeout: 10000 });
	const table = modal.locator('table.cake-autorate-bufferbloat-result');
	await expect(table.locator('tr[data-phase="idle"]')).toHaveCount(1);
	await expect(table.locator('tr[data-phase="dl"]')).toHaveCount(0);
});

test('the first status of a run stops the test before the load', async ({ page }) => {
	// Only the first status of the run (in the first 500 ms after Start)
	// shows a calibration; the later status requests get no answer in
	// time.
	let t0 = Infinity;
	await routeUbus(page, (s, asked) => {
		if (asked - t0 < 500)
			s.links[0].calibrating = true;
	}, false, (asked) => asked - t0 >= 500 ? 30000 : 0);
	const load = await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate((phases) => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = phases;
		b.warmup = 0;
		b.statusLag = 0;
		b.open({ name: 'wan', download: {}, upload: {} });
	}), PHASES);
	const modal = page.locator('.modal');
	t0 = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('A calibration of the link runs.', { timeout: 20000 });
	expect(load.down + load.up).toBe(0);
});

test('a status that comes after the end of the phase is also checked', async ({ page }) => {
	// From 1 s after Start, the status shows a calibration, and its answer
	// comes after the end of the 1.8 s phase.
	let t0 = Infinity;
	await routeUbus(page, (s, asked) => {
		s.links[0].state = 'RUNNING';
		Object.assign(s.links[0].download, { delta_ms: 2, achieved_kbps: 1e7 });
		if (asked - t0 > 900)
			s.links[0].calibrating = true;
	});
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'dl', load: 'dl', seconds: 1.8 } ];
		b.warmup = 0;
		b.statusLag = 0;
		const read = b.readLink;
		// A busy page: each answer comes 1 s after the request.
		b.readLink = function(...args) {
			return read.apply(this, args).then((v) => new Promise((resolve) => setTimeout(() => resolve(v), 1000)));
		};
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	t0 = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('A calibration of the link runs.', { timeout: 20000 });
	await expect(modal.locator('[data-grade]')).toHaveCount(0);
});

test('replies from the warm-up are not samples, also in a late status', async ({ page }) => {
	// The program publishes and writes the status each second: a status
	// shows the replies of up to 2 s before (the mock: 1.9 s). The link
	// has a reply each 300 ms until the end of the warm-up (3 s after the
	// start of the phase), then no reply.
	let t0 = Infinity;
	await routeUbus(page, (s, asked) => {
		s.links[0].state = 'RUNNING';
		Object.assign(s.links[0].download, { delta_ms: 100, achieved_kbps: 1e7 });
		const shown = Math.min(asked - 1900, t0 + 3000) - t0;
		s.links[0].replies = Math.max(0, Math.floor(shown / 300));
	});
	await routeLoad(page);
	await page.goto(PAGE);
	await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = [ { key: 'dl', load: 'dl', seconds: 9 } ];
		b.warmup = 3;
		b.statusLag = 2;
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	await modal.getByRole('button', { name: 'Start' }).click();
	t0 = Date.now();
	const dl = modal.locator('table.cake-autorate-bufferbloat-result tr[data-phase="dl"]');
	await expect(dl).toContainText('no ping replies', { timeout: 30000 });
	await expect(dl.locator('[data-grade]')).toHaveCount(0);
});

test('the test pins the link for its time and unpins it at the end', async ({ page }) => {
	await routeUbus(page);
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('table.cake-autorate-bufferbloat-result')).toHaveCount(1, { timeout: 20000 });
	// The time: the run (8 s) and 60 s.
	expect(pinCalls).toEqual([ [ 'pin', { link: 'wan', seconds: 68 } ], [ 'unpin', { link: 'wan' } ] ]);
	await expect(modal.locator('[data-pin-error]')).toHaveCount(0);
});

test('a failed pin shows a notice, and the test runs', async ({ page }) => {
	await routeUbus(page, (s) => {
		Object.assign(s.links[0], { state: 'RUNNING' });
		// The load goes through the link.
		Object.assign(s.links[0].download, { delta_ms: 2, achieved_kbps: 1e7 });
		Object.assign(s.links[0].upload, { delta_ms: 2, achieved_kbps: 1e7 });
	}, false, 0, 'device eth1 of link "wan" has no IPv4 default route');
	await routeLoad(page);
	const modal = await openDialog(page);
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal.locator('table.cake-autorate-bufferbloat-result')).toHaveCount(1, { timeout: 20000 });
	await expect(modal.locator('[data-pin-error]')).toContainText('The load was not pinned to this link: device eth1 of link "wan" has no IPv4 default route.');
	await expect(modal.locator('tr[data-phase="dl"] [data-grade]')).toHaveText('A+');
	// Without a pin, the page does not unpin.
	expect(pinCalls.map((c) => c[0])).toEqual([ 'pin' ]);
});
