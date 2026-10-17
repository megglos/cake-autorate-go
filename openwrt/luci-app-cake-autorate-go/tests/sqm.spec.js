// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the SQM fields in the link dialog of the configuration
// page. The tests write /etc/config/sqm in the VM. sqm-scripts is not
// necessary: the page reads only the configuration.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/network/cake-autorate/config';

// Writes /etc/config/sqm with the queues. Each queue is a list of UCI
// options.
function setSqm(queues) {
	let cmd = 'rm -f /etc/config/sqm && touch /etc/config/sqm';
	for (const [ name, opts ] of Object.entries(queues)) {
		cmd += ` && uci set sqm.${name}=queue`;
		for (const [ k, v ] of Object.entries(opts))
			cmd += ` && uci set sqm.${name}.${k}=${v}`;
	}
	vm(cmd + ' && uci commit sqm');
}

const CAKE_QUEUE = {
	enabled: 1, interface: 'eth1', download: 85000, upload: 10000,
	qdisc: 'cake', script: 'piece_of_cake.qos'
};

let errors;

test.beforeAll(() => {
	resetConfig();
});

test.afterAll(() => {
	resetConfig();
});

test.beforeEach(async ({ page }) => {
	await login(page);
	// Only the errors of the page under test, not of the login page.
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
});

// Opens the dialog of the link "wan".
async function openLink(page) {
	await page.goto(PAGE);
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	await modal.waitFor();
	return modal;
}

test.afterEach(() => {
	// The browser tests run in shards by test: the next test can be of
	// another file.
	vm('rm -f /etc/config/sqm');
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('an SQM queue sets the interfaces of the link', async ({ page }) => {
	setSqm({
		eth1: CAKE_QUEUE,
		// A disabled queue is not in the list.
		eth0: { ...CAKE_QUEUE, enabled: 0, interface: 'eth0' }
	});
	const modal = await openLink(page);
	const select = modal.locator('[data-name="_sqm_queue"] select');
	await expect(select.locator('option')).toHaveText([ 'Select a queue', 'eth1 (download: ifb4eth1)' ]);
	await expect(select).toHaveValue('');
	// The link "wan" uses br-lan for the download: SQM does not shape it.
	const check = modal.locator('[data-name="_sqm_check"]');
	await expect(check).toContainText('No enabled SQM queue shapes the download on br-lan.');
	await expect(check).not.toContainText('upload');

	await select.selectOption('eth1');
	const dl = modal.locator('[data-name="dl_interface"] .cbi-dropdown');
	const ul = modal.locator('[data-name="ul_interface"] .cbi-dropdown');
	await expect(dl.locator('li[data-value="ifb4eth1"][selected]')).toHaveCount(1);
	await expect(ul.locator('li[data-value="eth1"][selected]')).toHaveCount(1);
	await expect(check.locator('.alert-message')).toHaveCount(0);
	await expect(modal.locator('.cbi-input-invalid')).toHaveCount(0);

	await modal.locator('.cbi-button-positive').click();
	await expect(modal).toBeHidden();
	await expect(page.locator('.cbi-section-table-row[data-sid="wan"]')).toContainText('ifb4eth1');
});

test('the dialog selects the SQM queue of the link', async ({ page }) => {
	setSqm({ eth1: CAKE_QUEUE });
	vm('uci set cake-autorate.wan.dl_interface=ifb4eth1 && uci commit cake-autorate');
	const modal = await openLink(page);
	await expect(modal.locator('[data-name="_sqm_queue"] select')).toHaveValue('eth1');
	await expect(modal.locator('[data-name="_sqm_check"] .alert-message')).toHaveCount(0);
	resetConfig();
});

test('the SQM check shows a queue without CAKE and without a download rate', async ({ page }) => {
	setSqm({ eth1: { ...CAKE_QUEUE, download: 0, qdisc: 'fq_codel', script: 'simple.qos' } });
	vm('uci set cake-autorate.wan.dl_interface=ifb4eth1 && uci commit cake-autorate');
	const modal = await openLink(page);
	const check = modal.locator('[data-name="_sqm_check"]');
	await expect(check).toContainText('The SQM queue on eth1 does not shape the download (download rate 0).');
	await expect(check).toContainText('The SQM queue on eth1 does not use CAKE as the root qdisc.');
	// A direction that the service does not adjust has no check.
	await modal.locator('input[data-widget-id="widget.cbid.cake-autorate.wan.dl_adjust"]').uncheck();
	await expect(check).not.toContainText('download rate 0');
	await expect(check).toContainText('does not use CAKE');
	await modal.locator('input[data-widget-id="widget.cbid.cake-autorate.wan.ul_adjust"]').uncheck();
	await expect(check.locator('.alert-message')).toHaveCount(0);
	resetConfig();
});

test('the SQM check shows an upload interface without a queue', async ({ page }) => {
	setSqm({ eth0: { ...CAKE_QUEUE, interface: 'eth0' } });
	const modal = await openLink(page);
	await expect(modal.locator('[data-name="_sqm_check"]')).toContainText('No enabled SQM queue shapes the upload on eth1.');
});

test('the SQM fields are not in the dialog without an SQM configuration', async ({ page }) => {
	vm('rm -f /etc/config/sqm');
	const modal = await openLink(page);
	await expect(modal.locator('[data-name="dl_interface"]')).toBeVisible();
	await expect(modal.locator('[data-name="_sqm_queue"]')).toHaveCount(0);
	await expect(modal.locator('[data-name="_sqm_check"]')).toHaveCount(0);
});
