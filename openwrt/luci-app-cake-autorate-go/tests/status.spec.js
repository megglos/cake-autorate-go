// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the status page and of the Overview section in the
// OpenWrt VM. Start the VM and install the two packages first (see
// scripts/openwrt-vm.sh). The tests set their own configuration in the VM.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/services/cake-autorate/status';
const OVERVIEW = '/cgi-bin/luci/admin/status/overview';

// Returns the "paused" value of the direction key in the status file.
function pausedInFile(key) {
	return vm(`jsonfilter -i /var/run/cake-autorate/status.json -e "@.links[0].${key}.paused" 2>/dev/null || true`).trim();
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
	await page.goto('/cgi-bin/luci/admin/services/cake-autorate');
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

test('a user with only read permission sees no buttons', async ({ browser }) => {
	const page = await browser.newPage();
	try {
		await login(page, 'cakeread');
		page.on('pageerror', (e) => errors.push(e.message));
		await page.goto(PAGE);
		const dl = page.locator('tr[data-dir="dl"]');
		await expect(dl.locator('.label')).toHaveText('Active');
		await expect(page.locator('.cbi-section[data-link="wan"] button')).toHaveCount(0);
	}
	finally {
		await page.close();
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
		await expect(rate.locator('svg line[visibility="visible"]')).toHaveCount(1);

		// A refresh makes a new graph with the cursor at the same position.
		const old = await tip.elementHandle();
		await expect.poll(() => old.evaluate((e) => e.isConnected), { timeout: 10000 }).toBe(false);
		await expect(tip).toBeVisible();
		await expect(tip).toContainText('Upload shaper: 12.0 Mbit/s');

		// Only the graph under the pointer shows a cursor.
		await expect(page.locator('[data-graph="delay"] .graph-tip')).toBeHidden();

		await page.mouse.move(box.x + box.width / 2, box.y + box.height + 200);
		await expect(tip).toBeHidden();
		await expect(rate.locator('svg line[visibility="visible"]')).toHaveCount(0);
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
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
	await expect(row).toContainText(/RUNNING|IDLE|STALL/);
	await expect(row).toContainText('Mbit/s');
	await expect(row).not.toContainText('paused');

	vm('ucode -e \'import * as socket from "socket"; let s = socket.connect("/var/run/cake-autorate/control.sock"); s.send("pause wan ul\\n"); s.recv(256);\'');
	try {
		await expect(row.locator('td').nth(3)).toContainText('paused', { timeout: 15000 });
		await expect(row.locator('td').nth(2)).not.toContainText('paused');
	}
	finally {
		resetConfig();
		await waitForStatus();
	}
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
