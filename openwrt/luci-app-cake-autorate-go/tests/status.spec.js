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

test('page warns if the status file is old', async ({ page }) => {
	await page.goto(PAGE);
	await expect(page.locator('.cbi-section[data-link="wan"]')).toBeVisible();
	// A stopped process does not write the status file and does not
	// remove it.
	vm('kill -STOP $(pidof cake-autorate-go)');
	try {
		await expect(page.getByText('The program does not write the status file.')).toBeVisible({ timeout: 30000 });
	}
	finally {
		vm('kill -CONT $(pidof cake-autorate-go)');
	}
	await expect(page.getByText('The program does not write the status file.')).toHaveCount(0, { timeout: 15000 });
});

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
