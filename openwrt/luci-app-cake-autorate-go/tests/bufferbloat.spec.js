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

// Changes the status file that the page reads (the file on the router
// does not change), and records the calls of unshape and shape.
// Each status read gets one more ping reply ("replies"), as a link with
// pings; mutate(status, asked) can change it. asked is the time of the
// request (Date.now()).
async function routeUbus(page, mutate, failShape, statusDelay) {
	const calls = [];
	let replies = 1000;
	await page.route('**/ubus/**', async (route) => {
		const request = route.request().postDataJSON();
		const requests = Array.isArray(request) ? request : [ request ];
		for (const q of requests)
			if (q && q.params && q.params[1] == 'cake-autorate' && /^(un)?shape$/.test(q.params[2]))
				calls.push([ q.params[2], q.params[3] ]);
		// "shape" fails: an answer with an error, the router gets nothing.
		const shape = requests.find((q) => q && q.params && q.params[2] == 'shape');
		if (failShape && shape && requests.length == 1) {
			const answer = { jsonrpc: '2.0', id: shape.id, result: [ 0, { error: 'tc failed' } ] };
			return route.fulfill({ json: Array.isArray(request) ? [ answer ] : answer });
		}
		const asked = Date.now();
		const readsStatus = requests.some((q) => q && q.params && q.params[1] == 'file' && q.params[3] && q.params[3].path == '/var/run/cake-autorate/status.json');
		if (statusDelay && readsStatus)
			await new Promise((resolve) => setTimeout(resolve, statusDelay));
		let response;
		try {
			response = await route.fetch();
		}
		catch (e) {
			return;
		}
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
	});
	return calls;
}

// Opens the status page with short phases and opens the dialog.
async function openDialog(page) {
	await page.goto(PAGE);
	await page.evaluate((phases) => L.require('cake-autorate.bufferbloat').then((b) => {
		b.phases = phases;
		b.warmup = 0;
	}), PHASES);
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	return page.locator('.modal');
}

// Sets a high data limit: the local answers are fast, the uploads use
// approximately 600 Mbit/s.
async function noLimit(modal) {
	await modal.locator('#cake-autorate-bufferbloat-limit').fill('100000');
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

test.afterEach(() => {
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('the test measures the delay in each phase and gives a grade', async ({ page }) => {
	await routeUbus(page, (s) => {
		Object.assign(s.links[0], { state: 'RUNNING' });
		Object.assign(s.links[0].download, { delta_ms: 12 });
		Object.assign(s.links[0].upload, { delta_ms: 40 });
	});
	const load = await routeLoad(page);
	const modal = await openDialog(page);
	await expect(modal).toContainText('Bufferbloat test of the link wan');
	await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 8 s');
	await noLimit(modal);
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
		s.links[0].download.delta_ms = 0.1;
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
		await expect(modal.locator('.cake-autorate-estimate')).toContainText('approximately 19 s');
		await noLimit(modal);
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('.cake-autorate-phase')).toContainText('Run 2 of 2 (Without shaping)', { timeout: 20000 });
		await expect.poll(() => unshapedInFile(), { timeout: 10000 }).toBe('true');
		const tables = modal.locator('table.cake-autorate-bufferbloat-result');
		await expect(tables).toHaveCount(2, { timeout: 20000 });
		await expect(modal.locator('#cake-autorate-bufferbloat h4')).toHaveText([ 'With shaping', 'Without shaping' ]);
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
		await noLimit(modal);
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

test('the test stops at the data limit', async ({ page }) => {
	const calls = await routeUbus(page);
	const load = await routeLoad(page);
	const modal = await openDialog(page);
	await modal.locator('#cake-autorate-bufferbloat-compare').check();
	// One upload is 4 MiB: more than the limit of 1 MB.
	await modal.locator('#cake-autorate-bufferbloat-limit').fill('1');
	await modal.getByRole('button', { name: 'Start' }).click();
	await expect(modal).toContainText('The test stopped: the load reached the data limit of 1 MB.', { timeout: 20000 });
	expect(load.up).toBe(0);
	// The second run does not start.
	expect(calls).toEqual([]);
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
	await noLimit(modal);
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
		await noLimit(modal);
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
	await noLimit(modal);
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
	await noLimit(modal);
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
	await noLimit(modal);
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
		await noLimit(modal);
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
		await noLimit(modal);
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
		await noLimit(modal);
		await modal.getByRole('button', { name: 'Start' }).click();
		await expect(modal.locator('[data-shape-error]')).toBeVisible({ timeout: 30000 });
		await modal.getByRole('button', { name: 'Close' }).click();
		calls.length = 0;
		// The link is still unshaped: the button is disabled. Open the
		// dialog from the module.
		await page.evaluate(() => L.require('cake-autorate.bufferbloat').then((b) => b.open({ name: 'wan', download: {}, upload: {} })));
		modal = page.locator('.modal');
		await noLimit(modal);
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
		await noLimit(modal);
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
	await noLimit(modal);
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
		b.open({ name: 'wan', download: {}, upload: {} });
	}), PHASES);
	const modal = page.locator('.modal');
	await noLimit(modal);
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
	}));
	await page.locator('.cbi-section[data-link="wan"] .cake-autorate-bufferbloat').click();
	const modal = page.locator('.modal');
	await noLimit(modal);
	t0 = Date.now();
	await modal.getByRole('button', { name: 'Start' }).click();
	const dl = modal.locator('table.cake-autorate-bufferbloat-result tr[data-phase="dl"]');
	await expect(dl).toContainText('ms (p95', { timeout: 30000 });
	await expect(dl).toContainText('10.0 ms (p95 10.0)');
});
