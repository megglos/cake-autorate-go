// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the status page and of the Overview section in the
// OpenWrt VM. Start the VM and install the two packages first (see
// scripts/openwrt-vm.sh). The tests set their own configuration in the VM.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/network/cake-autorate/status';
const OVERVIEW = '/cgi-bin/luci/admin/status/overview';

// Returns the "paused" value of the direction key in the status file.
function pausedInFile(key) {
	return vm(`jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].${key}.paused" 2>/dev/null || true`).trim();
}

// Changes the status file that the page reads: mutate(status) changes the
// parsed file in the answers of rpcd. The file on the router does not
// change.
async function mockStatus(page, mutate) {
	await page.route('**/ubus/**', async (route) => {
		const request = route.request().postDataJSON();
		const requests = Array.isArray(request) ? request : [ request ];
		let response;
		try {
			response = await route.fetch();
		}
		catch (e) {
			// The test closed the page during the request.
			return;
		}
		const body = await response.json();
		for (const res of Array.isArray(body) ? body : [ body ]) {
			const p = (requests.find((q) => q && q.id === res.id) || {}).params;
			const data = res.result && res.result[1] && res.result[1].data;
			if (p && p[1] == 'file' && p[2] == 'read' && p[3] && p[3].path == '/var/run/cake-autorate/status.json' && data) {
				const status = JSON.parse(data);
				mutate(status);
				res.result[1].data = JSON.stringify(status);
			}
		}
		await route.fulfill({ response, json: body });
	});
}

// Waits until the program writes the status file.
async function waitForStatus() {
	await expect.poll(() => vm('[ -e /var/run/cake-autorate/status.json ] && echo yes || true').trim(),
		{ timeout: 30000 }).toBe('yes');
}

// Adds the rpcd login user "cakeread" with only the read permissions of
// luci-base and of the app. The password is the one of root (empty). LuCI
// calls session.access, thus the user also needs "unauthenticated".
function addReadOnlyUser() {
	vm(`uci -q delete rpcd.cakeread
		uci set rpcd.cakeread=login
		uci set rpcd.cakeread.username=cakeread
		uci set rpcd.cakeread.password='$p$root'
		uci add_list rpcd.cakeread.read=unauthenticated
		uci add_list rpcd.cakeread.read=luci-base
		uci add_list rpcd.cakeread.read=luci-app-cake-autorate-go
		uci commit rpcd
		/etc/init.d/rpcd reload
		# The reload is asynchronous. Wait until the user can log in.
		for i in $(seq 20); do
			ubus call session login '{"username":"cakeread","password":""}' >/dev/null 2>&1 && break
			sleep 1
		done
		sleep 1`);
}

function removeReadOnlyUser() {
	vm('uci -q delete rpcd.cakeread; uci commit rpcd; /etc/init.d/rpcd reload');
}

let errors;

test.beforeAll(async () => {
	// A reload of rpcd ends the sessions. Thus, add the user before the
	// logins of the tests.
	addReadOnlyUser();
	resetConfig();
	await waitForStatus();
});

test.afterAll(() => {
	vm('kill -CONT $(pidof cake-autorate-go) 2>/dev/null; true');
	removeReadOnlyUser();
	resetConfig();
});

test.beforeEach(async ({ page }) => {
	await login(page);
	// Only the errors of the page under test, not of the login page.
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
});

test.afterEach(() => {
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('menu opens the status page first', async ({ page }) => {
	// The menu entry shows its first child without a redirect.
	await page.goto('/cgi-bin/luci/admin/network/cake-autorate');
	await expect(page.locator('h2')).toHaveText('CAKE Autorate (Go)');
	await expect(page.locator('#cake-autorate-status .cbi-section[data-link="wan"]')).toBeVisible();
});

test('page shows the link, its directions and reflectors', async ({ page }) => {
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	await expect(link.locator('h3')).toContainText('wan');
	await expect(link.locator('[data-state]')).toHaveText(/^(Running|Idle|Stall)$/);
	await expect(link.locator('tr[data-dir="dl"]')).toContainText('br-lan');
	await expect(link.locator('tr[data-dir="dl"]')).toContainText('5.0 to 80.0, base 20.0');
	await expect(link.locator('tr[data-dir="ul"]')).toContainText('eth1');
	await expect(link.locator('tr[data-dir="ul"]')).toContainText('5.0 to 35.0, base 12.0');
	await expect(link.getByRole('cell', { name: '1.1.1.1', exact: true })).toBeVisible();
	await expect(link.getByRole('cell', { name: '8.8.8.8', exact: true })).toBeVisible();
	await expect(link.locator('button[data-dir="dl"]')).toHaveText('Pause');
	await expect(link.locator('button[data-dir="ul"]')).toHaveText('Pause');
	await expect(page.getByText(/^Version .*, started /)).toBeVisible();
});

test('pause and resume one direction', async ({ page }) => {
	await page.goto(PAGE);
	const dl = page.locator('tr[data-dir="dl"]');
	const ul = page.locator('tr[data-dir="ul"]');
	await dl.locator('button').click();
	await expect(dl.locator('.label')).toHaveText('Paused', { timeout: 10000 });
	await expect(dl.locator('button')).toHaveText('Resume');
	await expect(ul.locator('.label')).toHaveText('Active');
	expect(pausedInFile('download')).toBe('true');
	expect(pausedInFile('upload')).toBe('false');
	// A pause sets the base rate.
	expect(vm('tc qdisc show dev br-lan')).toContain('bandwidth 20Mbit');

	await dl.locator('button').click();
	await expect(dl.locator('.label')).toHaveText('Active', { timeout: 10000 });
	await expect(dl.locator('button')).toHaveText('Pause');
	expect(pausedInFile('download')).toBe('false');
});

test('a refresh keeps the keyboard focus on the button', async ({ page }) => {
	await page.goto(PAGE);
	const ul = page.locator('tr[data-dir="ul"] button');
	await ul.focus();
	await expect(ul).toBeFocused();
	// Wait for more than one refresh (each 2 seconds).
	const old = await ul.elementHandle();
	await expect.poll(() => old.evaluate((b) => b.isConnected), { timeout: 10000 }).toBe(false);
	await expect(ul).toBeFocused();
});

test('a direction with adjust off has no button', async ({ page }) => {
	vm('uci set cake-autorate.wan.ul_adjust=0 && uci commit cake-autorate && /etc/init.d/cake-autorate restart');
	try {
		await waitForStatus();
		await page.goto(PAGE);
		const ul = page.locator('tr[data-dir="ul"]');
		await expect(ul).toContainText('Off (adjust)', { timeout: 10000 });
		await expect(ul.locator('button')).toHaveCount(0);
		await expect(page.locator('tr[data-dir="dl"] button')).toHaveText('Pause');
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('a failed command shows the error', async ({ page }) => {
	await page.goto(PAGE);
	const button = page.locator('tr[data-dir="dl"] button');
	await expect(button).toHaveText('Pause');
	// Stop the polls, so that the button stays while the service stops.
	await page.evaluate(() => L.require('poll').then((poll) => poll.stop()));
	vm('/etc/init.d/cake-autorate stop');
	try {
		await button.click();
		await expect(page.locator('.alert-message.danger')).toContainText('Command failed: The service does not run');
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('page shows a stopped service and the configuration error', async ({ page }) => {
	vm(`uci set cake-autorate.wan.dl_min_rate_kbps=90000 && uci commit cake-autorate &&
		/etc/init.d/cake-autorate restart; true`);
	try {
		await page.goto(PAGE);
		// Not the LuCI warning about the empty root password.
		const status = page.locator('#cake-autorate-status');
		await expect(page.getByText('The service does not run', { exact: true })).toBeVisible({ timeout: 15000 });
		await expect(status.locator('.alert-message.warning pre')).toContainText('base_rate_kbps must be >= min_rate_kbps');
		await expect(page.locator('.cbi-section[data-link]')).toHaveCount(0);

		// The page shows the link again after a valid start, without a
		// reload.
		resetConfig();
		await expect(page.locator('.cbi-section[data-link="wan"]')).toBeVisible({ timeout: 30000 });
		await expect(status.locator('.alert-message.warning')).toHaveCount(0);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

// Moves the clock (Date) of the browser by ms. Thus the clock of the
// browser and of the router are different.
async function shiftClock(page, ms) {
	await page.addInitScript((off) => {
		const RealDate = Date;
		globalThis.Date = class extends RealDate {
			constructor(...a) { super(...(a.length ? a : [ RealDate.now() + off ])); }
			static now() { return RealDate.now() + off; }
		};
	}, ms);
}

for (const [ name, offset ] of [ [ 'the same', 0 ], [ 'a later', 3600000 ], [ 'an earlier', -3600000 ] ]) {
	test(`page warns if the status file is old, with ${name} browser clock`, async ({ page }) => {
		await shiftClock(page, offset);
		await page.goto(PAGE);
		const warning = page.getByText('The program does not write the status file.');
		await expect(page.locator('.cbi-section[data-link="wan"]')).toBeVisible();
		// No warning while the program writes the file.
		await page.waitForTimeout(12000);
		await expect(warning).toHaveCount(0);
		// A stopped process does not write the status file and does not
		// remove it.
		vm('kill -STOP $(pidof cake-autorate-go)');
		try {
			await expect(warning).toBeVisible({ timeout: 30000 });
		}
		finally {
			vm('kill -CONT $(pidof cake-autorate-go)');
		}
		await expect(warning).toHaveCount(0, { timeout: 15000 });
	});
}

test('page warns about an interface without CAKE', async ({ page }) => {
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	await expect(link.locator('tr[data-dir="ul"]')).toBeVisible();
	await expect(link.locator('[data-cake-warning]')).toHaveCount(0);

	vm('tc qdisc replace dev eth1 root fq_codel');
	try {
		// The page checks the qdiscs each 10 seconds.
		const warning = link.locator('[data-cake-warning="ul"]');
		await expect(warning).toContainText('Upload: the interface eth1 has no CAKE (root qdisc: fq_codel).', { timeout: 20000 });
		await expect(warning).toContainText('package luci-app-sqm');
		await expect(link.locator('[data-cake-warning="dl"]')).toHaveCount(0);

		vm('tc qdisc replace dev eth1 root cake bandwidth 12mbit');
		await expect(warning).toHaveCount(0, { timeout: 20000 });
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('the warning links to SQM QoS if the page is in the menu', async ({ browser }) => {
	// A menu entry like the one of luci-app-sqm.
	vm(`echo '{"admin/network/sqm":{"title":"SQM QoS","action":{"type":"view","path":"network/sqm"}}}' > /usr/share/luci/menu.d/test-sqm.json
		rm -rf /tmp/luci-*
		tc qdisc replace dev br-lan root fq_codel`);
	// LuCI keeps the menu of a session. A new login loads the new menu.
	const context = await browser.newContext();
	const page = await context.newPage();
	try {
		await login(page);
		page.on('pageerror', (e) => errors.push(e.message));
		await page.goto(PAGE);
		const warning = page.locator('.cbi-section[data-link="wan"] [data-cake-warning="dl"]');
		await expect(warning).toContainText('the interface br-lan has no CAKE', { timeout: 20000 });
		const sqm = warning.getByRole('link', { name: 'SQM QoS' });
		await expect(sqm).toHaveAttribute('href', /\/admin\/network\/sqm$/);

		// A refresh keeps the keyboard focus on the link.
		await sqm.focus();
		const old = await sqm.elementHandle();
		await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
		await expect(sqm).toBeFocused();
	}
	finally {
		await context.close();
		vm('rm -f /usr/share/luci/menu.d/test-sqm.json; rm -rf /tmp/luci-*');
		resetConfig();
		await waitForStatus();
	}
});

test('a direction with adjust off has no CAKE warning', async ({ page }) => {
	vm(`uci set cake-autorate.wan.ul_adjust=0 && uci commit cake-autorate && /etc/init.d/cake-autorate restart
		tc qdisc replace dev eth1 root fq_codel`);
	try {
		await waitForStatus();
		await page.goto(PAGE);
		const link = page.locator('.cbi-section[data-link="wan"]');
		await expect(link.locator('tr[data-dir="ul"]')).toContainText('Off (adjust)', { timeout: 10000 });
		await expect(link.locator('[data-cake-warning]')).toHaveCount(0);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('a user with only read permission sees no buttons, but the CAKE warning', async ({ browser }) => {
	const page = await browser.newPage();
	vm('tc qdisc replace dev eth1 root fq_codel');
	try {
		await login(page, 'cakeread');
		page.on('pageerror', (e) => errors.push(e.message));
		await page.goto(PAGE);
		const dl = page.locator('tr[data-dir="dl"]');
		await expect(dl.locator('.label')).toHaveText('Active');
		await expect(page.locator('.cbi-section[data-link="wan"] button')).toHaveCount(0);
		await expect(page.locator('[data-cake-warning="ul"]')).toContainText('has no CAKE');
	}
	finally {
		await page.close();
		resetConfig();
		await waitForStatus();
	}
});

// Sends one command to the control socket of the program.
function control(line) {
	vm(`ucode -e 'import * as socket from "socket"; let s = socket.connect("/var/run/cake-autorate/control.sock"); s.send("${line}\\n"); s.recv(256);'`);
}

// Returns the points of a series path as [ [ command, x, y ], ... ].
async function points(page, kind, series) {
	const d = await page.locator(`.cbi-section[data-link="wan"] [data-graph="${kind}"] path[data-series="${series}"]`).getAttribute('d');
	return [ ...(d || '').matchAll(/([ML])(-?[\d.]+) (-?[\d.]+)/g) ].map((m) => [ m[1], +m[2], +m[3] ]);
}

test('the labels show the state, the rate control and a high delay in color', async ({ page }) => {
	let state = 'RUNNING';
	await mockStatus(page, (s) => {
		const ls = s.links[0];
		ls.state = state;
		Object.assign(ls.download, { adjust: true, paused: false, delta_ms: 40, threshold_ms: 15, high_delay: true });
		// Above the configured threshold, but below the threshold of the
		// program: at a low rate, the program adds the time to send one
		// packet.
		Object.assign(ls.upload, { adjust: true, paused: true, delta_ms: 20, threshold_ms: 15, high_delay: false });
	});
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	const label = link.locator('h3 [data-state]');
	await expect(label).toHaveText('Running');
	await expect(label).toHaveClass('label success');

	// Active is green, Paused is blue.
	const dl = link.locator('tr[data-dir="dl"]'), ul = link.locator('tr[data-dir="ul"]');
	await expect(dl.locator('[data-control]')).toHaveText('Active');
	await expect(dl.locator('[data-control]')).toHaveClass('label success');
	await expect(ul.locator('[data-control]')).toHaveText('Paused');
	await expect(ul.locator('[data-control]')).toHaveClass('label notice');

	// Only a high delay is orange.
	await expect(dl.locator('[data-over]')).toHaveText('40.0 ms');
	await expect(dl.locator('[data-over]')).toHaveClass('label warning');
	await expect(ul.locator('[data-over]')).toHaveCount(0);
	await expect(ul).toContainText('20.0 ms / 15.0 ms');

	// Stall is orange, Idle has no color.
	state = 'STALL';
	await expect(label).toHaveText('Stall', { timeout: 10000 });
	await expect(label).toHaveClass('label warning');
	state = 'IDLE';
	await expect(label).toHaveText('Idle', { timeout: 10000 });
	await expect(label).toHaveClass('label');
});

// Returns the time (ms) of the sample in the box of a graph cursor, from
// its text "YYYY-MM-DD HH:mm:ss:SSS". Unlike the age ("N s ago"), it does
// not change with a refresh.
async function sampleTime(tip) {
	const m = (await tip.locator('.graph-time').textContent()).match(/^(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d):(\d{3})$/);
	return new Date(+m[1], m[2] - 1, +m[3], +m[4], +m[5], +m[6], +m[7]).getTime();
}

test('graphs show the rates and the delay delta of a link', async ({ page }) => {
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	await expect(link.locator('[data-graph="rate"] svg')).toBeVisible();
	await expect(link.locator('[data-graph="delay"] svg')).toBeVisible();
	for (const s of [ 'dl-shaper', 'dl-achieved', 'ul-shaper', 'ul-achieved' ])
		await expect(link.locator(`[data-graph="rate"] path[data-series="${s}"]`)).toHaveCount(1);
	for (const s of [ 'dl-delta', 'dl-threshold', 'ul-delta', 'ul-threshold' ])
		await expect(link.locator(`[data-graph="delay"] path[data-series="${s}"]`)).toHaveCount(1);
	// The page adds one sample for each refresh.
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(3);
	await expect(link.getByText('Collecting data…')).toHaveCount(0);
	// The newest sample is at the right side of the graph.
	const p = await points(page, 'rate', 'dl-shaper');
	expect(p[p.length - 1][1]).toBeGreaterThan(590);
	expect(p[0][1]).toBeLessThan(p[p.length - 1][1]);
});

test('the y axis of the rate graph fits the rates', async ({ page }) => {
	// A pause sets the base rates: 20 Mbit/s download, 12 Mbit/s upload.
	control('pause wan');
	try {
		await expect.poll(() => pausedInFile('upload'), { timeout: 10000 }).toBe('true');
		await page.goto(PAGE);
		const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
		await expect.poll(async () => (await points(page, 'rate', 'ul-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
		await expect(rate).toHaveAttribute('data-max', '20');
		await expect(rate.locator('.graph-max')).toHaveText('20 Mbit/s');
		// The graph is 150 units high: 20 Mbit/s is at the top (y 0),
		// 12 Mbit/s at y 60.
		for (const [ , , y ] of await points(page, 'rate', 'dl-shaper'))
			expect(y).toBeCloseTo(0, 1);
		for (const [ , , y ] of await points(page, 'rate', 'ul-shaper'))
			expect(y).toBeCloseTo(60, 1);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('the cursor shows the values of a sample and stays after a refresh', async ({ page }) => {
	// A pause sets the base rates: 20 Mbit/s download, 12 Mbit/s upload.
	control('pause wan');
	try {
		await expect.poll(() => pausedInFile('upload'), { timeout: 10000 }).toBe('true');
		await page.goto(PAGE);
		const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
		const tip = rate.locator('.graph-tip');
		await expect.poll(async () => (await points(page, 'rate', 'ul-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
		await expect(tip).toBeHidden();

		const box = await rate.locator('svg').boundingBox();
		await page.mouse.move(box.x + box.width * 0.99, box.y + box.height / 2);
		await expect(tip).toBeVisible();
		await expect(tip).toContainText('Download shaper: 20.0 Mbit/s');
		await expect(tip).toContainText('Upload shaper: 12.0 Mbit/s');
		await expect(tip).toContainText(/\d+ s ago/);
		// The time of the status on the router.
		const updated = JSON.parse(vm('cat /var/run/cake-autorate/status.json')).updated;
		await expect(tip.locator('.graph-time')).toHaveText(/^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d:\d{3}$/);
		const shown = await tip.locator('.graph-time').textContent();
		const [ date, time ] = shown.split(' ');
		const [ hh, mm, ss, ms ] = time.split(':');
		const at = new Date(date + 'T' + [ hh, mm, ss ].join(':') + '.' + ms).getTime();
		expect(Math.abs(at - Date.parse(updated.replace(/(\.\d{3})\d+/, '$1')))).toBeLessThan(10000);
		await expect(rate.locator('svg line[visibility="visible"]')).toHaveCount(1);

		// A refresh makes a new graph with the cursor at the same position.
		const old = await tip.elementHandle();
		await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
		await expect(tip).toBeVisible();
		await expect(tip).toContainText('Upload shaper: 12.0 Mbit/s');

		// The other graph of the link shows a dashed cursor at the same
		// sample. Only the box of the graph under the pointer is live for a
		// screen reader.
		const delay = page.locator('.cbi-section[data-link="wan"] [data-graph="delay"]');
		const delayTip = delay.locator('.graph-tip');
		await expect(delayTip).toBeVisible();
		await expect(delayTip).toContainText('Upload threshold');
		// Read both graphs in one evaluation, thus from the same render: the
		// pointer is on the newest sample, and a refresh moves both cursors.
		const both = await page.evaluate(() => [ 'rate', 'delay' ].map((g) => {
			const graph = document.querySelector(`.cbi-section[data-link="wan"] [data-graph="${g}"]`);
			return {
				time: graph.querySelector('.graph-time').textContent,
				x: graph.querySelector('svg line[visibility="visible"]').getAttribute('x1')
			};
		}));
		expect(both[1]).toEqual(both[0]);
		await expect(rate.locator('svg line[visibility="visible"]')).toHaveAttribute('stroke-dasharray', 'none');
		await expect(delay.locator('svg line[visibility="visible"]')).toHaveAttribute('stroke-dasharray', '3 3');
		await expect(tip).toHaveAttribute('aria-live', 'polite');
		await expect(delayTip).toHaveAttribute('aria-live', 'off');

		await page.mouse.move(box.x + box.width / 2, box.y + box.height + 200);
		await expect(tip).toBeHidden();
		await expect(delayTip).toBeHidden();
		await expect(rate.locator('svg line[visibility="visible"]')).toHaveCount(0);
		await expect(delay.locator('svg line[visibility="visible"]')).toHaveCount(0);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('the cursor goes away if the graph under the pointer gets no pointerleave', async ({ page }) => {
	await page.goto(PAGE);
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	const tip = rate.locator('.graph-tip');
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
	const box = await rate.locator('svg').boundingBox();
	await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
	await expect(tip).toBeVisible();

	// A refresh replaces the graph under the pointer. The browser does not
	// always send "pointerleave" to the new graph. A copy without event
	// handlers has the same effect.
	await page.evaluate(() => {
		const g = document.querySelector('.cbi-section[data-link="wan"] svg[data-plot="rate"]');
		g.replaceWith(g.cloneNode(true));
	});
	// A point in the window, above the graph. A point out of the window
	// gives no events.
	await page.mouse.move(box.x + box.width / 2, box.y - 40);
	// Also after the next refresh.
	const old = await tip.elementHandle();
	await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
	await expect(tip).toBeHidden();
});

test('the keyboard selects a sample and the selection stays after a refresh', async ({ page }) => {
	control('pause wan');
	try {
		await expect.poll(() => pausedInFile('upload'), { timeout: 10000 }).toBe('true');
		await page.goto(PAGE);
		const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
		const plot = rate.locator('svg');
		const tip = rate.locator('.graph-tip');
		await expect.poll(async () => (await points(page, 'rate', 'ul-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(4);

		// The focus selects the newest sample.
		await plot.focus();
		await expect(tip).toBeVisible();
		await expect(tip).toContainText('Upload shaper: 12.0 Mbit/s');
		// The time of the shown sample. Not the age: a refresh between two
		// reads makes all samples 2 s older. The times sort as text.
		const at = (t) => sampleTime(t || tip);
		const newest = await at();

		// Home selects the oldest sample, ArrowRight the next one.
		await page.keyboard.press('Home');
		const oldest = await at();
		expect(oldest < newest).toBe(true);
		await page.keyboard.press('ArrowRight');
		const next = await at();
		expect(next > oldest).toBe(true);
		// The other graph of the link shows the same sample.
		const delayTip = page.locator('.cbi-section[data-link="wan"] [data-graph="delay"] .graph-tip');
		await expect.poll(() => at(delayTip)).toBe(next);

		// A refresh keeps the focus and the selected sample.
		const old = await tip.elementHandle();
		await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
		await expect(plot).toBeFocused();
		await expect(tip).toBeVisible();
		expect(await at()).toBe(next);

		// Without the focus, there is no cursor.
		await page.locator('#cake-autorate-period').focus();
		await expect(tip).toBeHidden();
		await expect(delayTip).toBeHidden();
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('the arrow keys start from the shown sample after the selected one is too old', async ({ page }) => {
	// The graph must be full for one minute.
	test.setTimeout(150000);
	await page.goto(PAGE);
	await page.selectOption('#cake-autorate-period', '60');
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	const tip = rate.locator('.graph-tip');
	const ago = async () => +(await tip.textContent()).match(/(\d+) s ago/)[1];
	// Wait until the oldest sample is at the left side of the graph.
	await expect.poll(async () => {
		const p = await points(page, 'rate', 'dl-shaper');
		return p.length ? p[0][1] : 600;
	}, { timeout: 90000, intervals: [ 2000 ] }).toBeLessThan(25);

	await rate.locator('svg').focus();
	await page.keyboard.press('Home');
	// Two refreshes: the selected sample is out of the period now. The
	// cursor shows the oldest sample in the period.
	for (let i = 0; i < 2; i++) {
		const old = await tip.elementHandle();
		await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
	}
	expect(await ago()).toBeGreaterThanOrEqual(55);
	const shown = await sampleTime(tip);
	// ArrowRight selects the next sample, not one near the newest.
	await page.keyboard.press('ArrowRight');
	const next = await sampleTime(tip);
	expect(next).toBeGreaterThan(shown);
	expect(next - shown).toBeLessThanOrEqual(5000);
});

test('the arrow keys start from the sample under the pointer', async ({ page }) => {
	await page.goto(PAGE);
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	const plot = rate.locator('svg');
	const tip = rate.locator('.graph-tip');
	const ago = async () => +(await tip.textContent()).match(/(\d+) s ago/)[1];
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 30000 }).toBeGreaterThanOrEqual(8);

	// Point to the second oldest sample, then use the keyboard.
	const box = await plot.boundingBox();
	const x = (await points(page, 'rate', 'dl-shaper'))[1][1];
	await page.mouse.move(box.x + x / 600 * box.width, box.y + box.height / 2);
	await expect(tip).toBeVisible();
	expect(await ago()).toBeGreaterThanOrEqual(10);
	const shown = await sampleTime(tip);
	await plot.focus();
	await page.keyboard.press('ArrowRight');
	const next = await sampleTime(tip);
	expect(next).toBeGreaterThan(shown);
	expect(next - shown).toBeLessThanOrEqual(5000);
});

test('the pointer on one graph does not change the selection of the other graph', async ({ page }) => {
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	const rate = link.locator('[data-graph="rate"]');
	const delay = link.locator('[data-graph="delay"]');
	const time = (g) => sampleTime(g.locator('.graph-tip'));
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 30000 }).toBeGreaterThanOrEqual(8);

	// Select the oldest sample of Rates, then move the pointer over Delay
	// delta and away. Rates keeps its selection while the pointer is on
	// Delay delta. After the pointer leaves, Delay delta shows the
	// selection of Rates again.
	await rate.locator('svg').focus();
	await page.keyboard.press('Home');
	const oldest = await time(rate);
	// The newest sample is at the right side.
	const box = await delay.locator('svg').boundingBox();
	await page.mouse.move(box.x + box.width * 0.99, box.y + box.height / 2);
	await expect(delay.locator('.graph-tip')).toBeVisible();
	expect(await time(rate)).toBe(oldest);
	await expect.poll(() => time(delay)).toBeGreaterThan(oldest);
	await page.mouse.move(box.x + box.width / 2, box.y + box.height + 200);
	await expect(rate.locator('.graph-tip')).toBeVisible();
	await expect.poll(() => time(delay)).toBe(oldest);
	// The same sample.
	const still = await time(rate);
	expect(still).toBe(oldest);
	await page.keyboard.press('ArrowRight');
	const next = await time(rate);
	expect(next).toBeGreaterThan(still);
	expect(next - still).toBeLessThanOrEqual(5000);

	// With the pointer over Rates, Tab to Delay delta: Delay delta has a
	// selection, also after the pointer leaves Rates. Then Rates shows the
	// selection of Delay delta.
	const rbox = await rate.locator('svg').boundingBox();
	await page.mouse.move(rbox.x + rbox.width / 2, rbox.y + rbox.height / 2);
	await page.keyboard.press('Tab');
	await expect(delay.locator('svg')).toBeFocused();
	await page.keyboard.press('Home');
	const delayOldest = await time(delay);
	await page.mouse.move(rbox.x + rbox.width / 2, rbox.y + rbox.height + 200);
	await expect(delay.locator('.graph-tip')).toBeVisible();
	await expect.poll(() => time(rate)).toBe(delayOldest);
	expect(await time(delay)).toBe(delayOldest);
});

test('the cursor of the other graph shows the same sample near the middle of two samples', async ({ page }) => {
	// A slow browser: each call of performance.now() adds 500 ms. If the
	// two graphs read the time separately, the samples of the second graph
	// are more to the left.
	await page.addInitScript(() => {
		const now = performance.now.bind(performance);
		let extra = 0;
		performance.now = () => now() + (extra += 500);
	});
	await page.goto(PAGE);
	await page.selectOption('#cake-autorate-period', '60');
	const link = page.locator('.cbi-section[data-link="wan"]');
	const rate = link.locator('[data-graph="rate"]');
	const delay = link.locator('[data-graph="delay"]');
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 30000 }).toBeGreaterThanOrEqual(4);

	// Just after a refresh: point a little to the left of the middle of the
	// two newest samples of Rates.
	const old = await rate.locator('svg').elementHandle();
	await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
	const p = await points(page, 'rate', 'dl-shaper');
	const x = (p[p.length - 2][1] + p[p.length - 1][1]) / 2 - 2;
	const box = await rate.locator('svg').boundingBox();
	await page.mouse.move(box.x + x / 600 * box.width, box.y + box.height / 2);
	await expect(delay.locator('.graph-tip')).toBeVisible();
	expect(await delay.locator('.graph-time').textContent()).toBe(await rate.locator('.graph-time').textContent());
});

// Returns the number of samples of the first link in the history of the
// program.
function historyLength() {
	const n = vm(`ubus call cake-autorate history 2>/dev/null | jsonfilter -e @.data | jsonfilter -e '@.links[0].samples[*][0]' | wc -l`);
	return +n.trim();
}

// Turns on the history of the program (option status_history).
async function enableHistory() {
	vm('uci set cake-autorate.global.status_history=1 && uci commit cake-autorate && /etc/init.d/cake-autorate restart');
	await waitForStatus();
}

test('the graphs show the history of the program at the load of the page', async ({ page }) => {
	await enableHistory();
	try {
		await expect.poll(historyLength, { timeout: 40000, intervals: [ 2000 ] }).toBeGreaterThanOrEqual(15);
		await historyAtLoad(page);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

async function historyAtLoad(page) {
	await page.goto(PAGE);
	// Before the first refresh, the graph has the samples of the history:
	// one each second.
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	await expect(rate.locator('svg')).toBeVisible();
	const p = await points(page, 'rate', 'dl-shaper');
	expect(p.length).toBeGreaterThanOrEqual(14);
	// One line: no gap between the history and the first sample of the page.
	expect(p.filter((e) => e[0] == 'M').length).toBe(1);
	await expect(rate.getByText('Collecting data…')).toHaveCount(0);

	// The cursor on the oldest sample shows its age and its time.
	const box = await rate.locator('svg').boundingBox();
	await page.mouse.move(box.x + p[0][1] / 600 * box.width, box.y + box.height / 2);
	const tip = rate.locator('.graph-tip');
	await expect(tip).toBeVisible();
	const ago = +(await tip.textContent()).match(/(\d+) s ago/)[1];
	expect(ago).toBeGreaterThanOrEqual(13);
	await expect(tip.locator('.graph-time')).toHaveText(/^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d:\d{3}$/);
}

test('the graphs keep the time when the user was on another page', async ({ page }) => {
	await enableHistory();
	try {
		await keepsTimeAway(page);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

async function keepsTimeAway(page) {
	await page.goto(PAGE);
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
	await page.goto('/cgi-bin/luci/admin/network/cake-autorate/config');
	await page.waitForTimeout(12000);
	await page.goto(PAGE);
	await expect(page.locator('.cbi-section[data-link="wan"] [data-graph="rate"] svg')).toBeVisible();
	// The samples of the 12 seconds on the configuration page, without a
	// gap.
	const p = await points(page, 'rate', 'dl-shaper');
	expect(p.length).toBeGreaterThanOrEqual(12);
	expect(p.filter((e) => e[0] == 'M').length).toBe(1);
}

test('without Graph history (the default), the graphs start empty', async ({ page }) => {
	// The default configuration has no history.
	expect(vm('ubus call cake-autorate history')).toContain('status_history is off');
	await page.goto(PAGE);
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	await expect(rate.locator('svg')).toBeVisible();
	// Only the samples since the load: not more than some seconds. The
	// text "Collecting data…" can be gone at once: the first refresh runs
	// right after the load and can get a new status.
	const p = await points(page, 'rate', 'dl-shaper');
	expect(p.length).toBeLessThanOrEqual(3);
	// 10 s of the period of 5 minutes at the right side of 600 units.
	for (const e of p)
		expect(e[1]).toBeGreaterThan(580);
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
});

test('without the history of the program, the graphs start empty', async ({ page }) => {
	// An old program has no command "history".
	await page.route('**/ubus/**', async (route) => {
		const request = route.request().postDataJSON();
		const requests = Array.isArray(request) ? request : [ request ];
		if (!requests.some((q) => q && q.params && q.params[1] == 'cake-autorate' && q.params[2] == 'history'))
			return route.continue();
		let response;
		try {
			response = await route.fetch();
		}
		catch (e) {
			return;
		}
		const body = await response.json();
		for (const res of Array.isArray(body) ? body : [ body ]) {
			const q = requests.find((r) => r && r.id === res.id);
			if (q && q.params[2] == 'history')
				res.result = [ 0, { error: 'unknown command "history"' } ];
		}
		await route.fulfill({ response, json: body });
	});
	await page.goto(PAGE);
	const rate = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"]');
	await expect(rate.locator('svg')).toBeVisible();
	// Only the samples since the load (see the test before).
	const p = await points(page, 'rate', 'dl-shaper');
	expect(p.length).toBeLessThanOrEqual(3);
	for (const e of p)
		expect(e[1]).toBeGreaterThan(580);
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
});

test('a link name that is an Object property has graphs', async ({ page }) => {
	vm('uci rename cake-autorate.wan=constructor && uci commit cake-autorate && /etc/init.d/cake-autorate restart');
	try {
		await expect.poll(() => vm('jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].name" 2>/dev/null || true').trim(),
			{ timeout: 30000 }).toBe('constructor');
		await page.goto(PAGE);
		const link = page.locator('.cbi-section[data-link="constructor"]');
		await expect(link.locator('[data-graph="rate"] svg')).toBeVisible();
		await expect.poll(async () => {
			const d = await link.locator('path[data-series="dl-shaper"]').getAttribute('d');
			return (d || '').split('L').length;
		}, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('the graph period changes the time axis and stays after a refresh', async ({ page }) => {
	await page.goto(PAGE);
	const start = page.locator('.cbi-section[data-link="wan"] [data-graph="rate"] .graph-start');
	await expect(start).toHaveText('-5 min');
	await page.selectOption('#cake-autorate-period', '60');
	await expect(start).toHaveText('-1 min');
	// Wait for a refresh: it makes a new graph.
	const old = await start.elementHandle();
	await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
	await expect(start).toHaveText('-1 min');
	await expect(page.locator('#cake-autorate-period')).toHaveValue('60');
});

test('a graph has a gap while the program does not write the status', async ({ page }) => {
	await page.goto(PAGE);
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).length, { timeout: 15000 }).toBeGreaterThanOrEqual(2);
	vm('kill -STOP $(pidof cake-autorate-go)');
	try {
		await page.waitForTimeout(10000);
	}
	finally {
		vm('kill -CONT $(pidof cake-autorate-go)');
	}
	// A new line starts after the gap.
	await expect.poll(async () => (await points(page, 'rate', 'dl-shaper')).filter((p) => p[0] == 'M').length,
		{ timeout: 15000 }).toBe(2);
});

test('Overview shows the link and a pause', async ({ page }) => {
	await page.goto(OVERVIEW);
	const row = page.locator('tr[data-link="wan"]');
	await expect(row).toBeVisible({ timeout: 15000 });
	await expect(row.locator('.label[data-state]')).toHaveText(/^(Running|Idle|Stall)$/);
	await expect(row).toContainText('Mbit/s');
	await expect(row).not.toContainText('Paused');

	vm('ucode -e \'import * as socket from "socket"; let s = socket.connect("/var/run/cake-autorate/control.sock"); s.send("pause wan ul\\n"); s.recv(256);\'');
	try {
		await expect(row.locator('td').nth(3).locator('.label.notice')).toHaveText('Paused', { timeout: 15000 });
		await expect(row.locator('td').nth(2)).not.toContainText('Paused');
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('without "high_delay" (an older program), a delta above the threshold is orange', async ({ page }) => {
	await mockStatus(page, (s) => {
		const ls = s.links[0];
		Object.assign(ls.download, { delta_ms: 40, threshold_ms: 15 });
		Object.assign(ls.upload, { delta_ms: 2, threshold_ms: 15 });
		delete ls.download.high_delay;
		delete ls.upload.high_delay;
	});
	await page.goto(PAGE);
	const link = page.locator('.cbi-section[data-link="wan"]');
	await expect(link.locator('tr[data-dir="dl"] [data-over]')).toHaveText('40.0 ms');
	await expect(link.locator('tr[data-dir="ul"] [data-over]')).toHaveCount(0);
});

test('Overview shows the state with the same label as the status page', async ({ page }) => {
	await mockStatus(page, (s) => { s.links[0].state = 'RUNNING'; });
	await page.goto(OVERVIEW);
	const label = page.locator('tr[data-link="wan"] [data-state]');
	await expect(label).toHaveText('Running', { timeout: 15000 });
	await expect(label).toHaveClass('label success');
});

// Returns the "unshaped" value of the link in the status file.
function unshapedInFile() {
	return vm('jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].unshaped" 2>/dev/null || true').trim();
}

test('turn the shaping of a link off and on', async ({ page }) => {
	try {
		await page.goto(PAGE);
		const link = page.locator('.cbi-section[data-link="wan"]');
		const button = link.locator('.cake-autorate-shaping');
		await expect(button).toHaveText('Turn shaping off');
		await button.click();
		await expect(link.locator('h3 [data-state]')).toHaveText('Shaping off', { timeout: 10000 });
		await expect(link.locator('h3 [data-state]')).toHaveClass('label warning');
		await expect(button).toHaveText('Turn shaping on');
		expect(unshapedInFile()).toBe('true');
		expect(vm('tc qdisc show dev br-lan')).toContain('bandwidth 10Gbit');
		// No rate control: no pause buttons, and no calibration.
		for (const dir of [ 'dl', 'ul' ]) {
			await expect(link.locator(`tr[data-dir="${dir}"] [data-control="unshaped"]`)).toHaveText('Off (shaping off)');
			await expect(link.locator(`tr[data-dir="${dir}"] button`)).toHaveCount(0);
			await expect(link.locator(`tr[data-dir="${dir}"]`)).toContainText('No limit');
		}
		await expect(link.locator('.cake-autorate-calibrate')).toBeDisabled();
		// The 10 Gbit/s of the shaper are not in the graph: the y axis
		// fits the achieved rates.
		await expect.poll(async () => +(await link.locator('[data-graph="rate"]').getAttribute('data-max')), { timeout: 15000 }).toBeLessThan(1000);
		const rate = link.locator('[data-graph="rate"]');
		const box = await rate.locator('svg').boundingBox();
		await page.mouse.move(box.x + box.width * 0.99, box.y + box.height / 2);
		await expect(rate.locator('.graph-tip')).toContainText('Download shaper: no limit');

		await button.click();
		await expect(link.locator('h3 [data-state]')).not.toHaveText('Shaping off', { timeout: 10000 });
		await expect(button).toHaveText('Turn shaping off');
		expect(unshapedInFile()).toBe('false');
		expect(vm('tc qdisc show dev br-lan')).toContain('bandwidth 20Mbit');
		await expect(link.locator('tr[data-dir="dl"] button')).toHaveText('Pause');
		await expect(link.locator('.cake-autorate-calibrate')).toBeEnabled();
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});

test('a shaper rate above 10 Gbit/s is a rate, not "No limit"', async ({ page }) => {
	await mockStatus(page, (s) => { s.links[0].download.shaper_kbps = 12000000; });
	await page.goto(PAGE);
	const dl = page.locator('.cbi-section[data-link="wan"] tr[data-dir="dl"]');
	await expect(dl).toContainText('12000.0 Mbit/s');
	await expect(dl).not.toContainText('No limit');
});

test('during a calibration, the shaping button is disabled', async ({ page }) => {
	await mockStatus(page, (s) => { s.links[0].calibrating = true; });
	await page.goto(PAGE);
	await expect(page.locator('.cbi-section[data-link="wan"] .cake-autorate-shaping')).toBeDisabled();
});

test('Overview shows a link without shaping', async ({ page }) => {
	await mockStatus(page, (s) => { s.links[0].unshaped = true; });
	await page.goto(OVERVIEW);
	const label = page.locator('tr[data-link="wan"] [data-state]');
	await expect(label).toHaveText('Shaping off', { timeout: 15000 });
	await expect(label).toHaveClass('label warning');
});

test('Overview shows a stopped service', async ({ page }) => {
	vm('/etc/init.d/cake-autorate stop');
	try {
		await page.goto(OVERVIEW);
		await expect(page.getByText('The service does not run.')).toBeVisible({ timeout: 15000 });
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
});
