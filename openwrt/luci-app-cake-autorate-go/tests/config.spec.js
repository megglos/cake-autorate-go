// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the configuration page in the OpenWrt VM. Start the VM
// and install the two packages first (see scripts/openwrt-vm.sh). The
// tests set their own configuration in the VM.

const { test, expect } = require('@playwright/test');
const { execFileSync } = require('child_process');
const path = require('path');

const VM = path.resolve(__dirname, '../../../scripts/openwrt-vm.sh');
const PAGE = '/cgi-bin/luci/admin/services/cake-autorate/config';

// Runs the shell command cmd in the VM and returns its output.
function vm(cmd) {
	return execFileSync(VM, [ 'ssh', cmd ], { encoding: 'utf8' });
}

// The configuration of the tests: one link "wan" with CAKE on eth1 and
// br-lan, like scripts/openwrt-vm-test.sh.
function resetConfig() {
	vm(`tc qdisc replace dev eth1 root cake bandwidth 20mbit &&
		tc qdisc replace dev br-lan root cake bandwidth 80mbit &&
		for s in $(uci -q show cake-autorate | sed -n 's/^cake-autorate\\.\\([^.=]*\\)=link$/\\1/p'); do
			uci delete cake-autorate.$s
		done
		uci -q delete cake-autorate.global
		uci set cake-autorate.global=global
		uci set cake-autorate.global.enabled=1
		uci set cake-autorate.global.ping_method=icmp
		uci set cake-autorate.wan=link
		uci set cake-autorate.wan.dl_interface=br-lan
		uci set cake-autorate.wan.dl_min_rate_kbps=5000
		uci set cake-autorate.wan.dl_base_rate_kbps=20000
		uci set cake-autorate.wan.dl_max_rate_kbps=80000
		uci set cake-autorate.wan.ul_interface=eth1
		uci set cake-autorate.wan.ul_min_rate_kbps=5000
		uci set cake-autorate.wan.ul_base_rate_kbps=12000
		uci set cake-autorate.wan.ul_max_rate_kbps=35000
		uci add_list cake-autorate.wan.reflectors=1.1.1.1
		uci add_list cake-autorate.wan.reflectors=8.8.8.8
		uci commit cake-autorate
		/etc/init.d/cake-autorate restart`);
}

// The input element of the option name in section sid.
function input(page, sid, name) {
	return page.locator(`[id="widget.cbid.cake-autorate.${sid}.${name}"]`);
}

async function setValue(page, sid, name, value) {
	const el = input(page, sid, name);
	await el.fill(value);
	await el.press('Tab');
}

async function openTab(scope, tab) {
	await scope.locator(`.cbi-tabmenu li[data-tab="${tab}"] a`).first().click();
}

const INVALID = /cbi-input-invalid/;

let errors;

test.beforeAll(() => {
	resetConfig();
});

test.afterAll(() => {
	resetConfig();
});

test.beforeEach(async ({ page }) => {
	await page.goto('/cgi-bin/luci/');
	await page.fill('input[name=luci_username]', 'root');
	await page.fill('input[name=luci_password]', '');
	await page.locator('button.cbi-button-positive, button[type=submit]').first().click();
	await page.waitForLoadState('networkidle');
	// Only the errors of the page under test, not of the login page.
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
	await page.goto(PAGE);
	await page.locator('.cbi-section').first().waitFor();
});

test.afterEach(() => {
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

test('page shows the service and the links', async ({ page }) => {
	await expect(page.locator('h2')).toHaveText('CAKE Autorate (Go)');
	await expect(page.locator('.cbi-tabmenu li').filter({ hasText: /^(General|Idle and stall|Delay thresholds|Advanced|Logging)$/ }))
		.toHaveCount(5);
	const row = page.locator('.cbi-section-table-row[data-sid="wan"]');
	await expect(row).toContainText('br-lan');
	await expect(row).toContainText('eth1');
	await expect(row).toContainText('20.0 / 80.0');
	await expect(row).toContainText('12.0 / 35.0');
	await expect(page.locator('.cbi-section-table-titles th').filter({ hasText: /^Name$/ })).toHaveCount(1);
	await expect(page.locator('.alert-message', { hasText: 'did not start' })).toHaveCount(0);
});

test('integer settings and global thresholds', async ({ page }) => {
	await setValue(page, 'global', 'pinger_count', '1.5');
	await expect(input(page, 'global', 'pinger_count')).toHaveClass(INVALID);
	await setValue(page, 'global', 'pinger_count', '4');
	await expect(input(page, 'global', 'pinger_count')).not.toHaveClass(INVALID);

	await openTab(page, 'thresholds');
	await setValue(page, 'global', 'dl_owd_delta_delay_thr_ms', '0');
	await expect(input(page, 'global', 'dl_owd_delta_delay_thr_ms')).toHaveClass(INVALID);
	await setValue(page, 'global', 'dl_owd_delta_delay_thr_ms', '25.5');
	await expect(input(page, 'global', 'dl_owd_delta_delay_thr_ms')).not.toHaveClass(INVALID);
});

test('pairs use the default value of an empty field', async ({ page }) => {
	await openTab(page, 'advanced');
	for (const [ name, value ] of [
		[ 'bufferbloat_detection_window', '2' ],          // threshold default 3
		[ 'bufferbloat_detection_thr', '7' ],             // window default 6
		[ 'reflector_misbehaving_detection_window', '2' ], // threshold default 3
		[ 'shaper_rate_min_adjust_down_bufferbloat', '0.5' ], // maximum default 0.75
		[ 'shaper_rate_min_adjust_up_load_high', '2' ]    // maximum default 1.04
	]) {
		await setValue(page, 'global', name, value);
		await expect(input(page, 'global', name), name + '=' + value).toHaveClass(INVALID);
		await setValue(page, 'global', name, '');
		await expect(input(page, 'global', name), name + ' empty').not.toHaveClass(INVALID);
	}
	// A change of one field also checks the other field.
	await setValue(page, 'global', 'bufferbloat_detection_window', '8');
	await setValue(page, 'global', 'bufferbloat_detection_thr', '8');
	await setValue(page, 'global', 'bufferbloat_detection_window', '4');
	await expect(input(page, 'global', 'bufferbloat_detection_window')).toHaveClass(INVALID);
});

test('rate errors mark only the fields with the error', async ({ page }) => {
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	await openTab(modal, 'rates');

	await setValue(page, 'wan', 'dl_base_rate_kbps', '1000');
	await expect(input(page, 'wan', 'dl_min_rate_kbps')).toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_base_rate_kbps')).toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_max_rate_kbps')).not.toHaveClass(INVALID);

	await setValue(page, 'wan', 'dl_base_rate_kbps', '90000');
	await expect(input(page, 'wan', 'dl_min_rate_kbps')).not.toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_base_rate_kbps')).toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_max_rate_kbps')).toHaveClass(INVALID);

	await setValue(page, 'wan', 'dl_base_rate_kbps', '20000');
	await setValue(page, 'wan', 'ul_max_rate_kbps', '');
	await expect(input(page, 'wan', 'ul_max_rate_kbps')).toHaveClass(INVALID);
	await expect(modal.locator('.cbi-input-invalid')).toHaveCount(1);
});

test('new link gets the default rates and Save & Apply starts it', async ({ page }) => {
	await page.locator('.cbi-section-create input').pressSequentially('lte');
	await page.locator('.cbi-section-create .cbi-button-add').click();
	const modal = page.locator('.modal');
	await modal.waitFor();

	await openTab(modal, 'rates');
	await expect(input(page, 'lte', 'dl_base_rate_kbps')).toHaveValue('20000');
	await expect(input(page, 'lte', 'ul_max_rate_kbps')).toHaveValue('35000');

	await openTab(modal, 'interfaces');
	const dl = modal.locator('[data-name="dl_interface"] .cbi-dropdown');
	await dl.click();
	const custom = dl.locator('input.create-item-input').first();
	await custom.fill('ifb4lte');
	await custom.press('Enter');
	// eth1 is the upload interface of the link wan.
	const ul = modal.locator('[data-name="ul_interface"] .cbi-dropdown');
	await ul.click();
	await ul.locator('li[data-value="eth1"][tabindex]').click();
	await expect(page.locator('[id="cbid.cake-autorate.lte.ul_interface"]')).toHaveClass(INVALID);
	await ul.click();
	await ul.locator('li[data-value="eth0"][tabindex]').click();
	await expect(page.locator('[id="cbid.cake-autorate.lte.ul_interface"]')).not.toHaveClass(INVALID);

	await modal.locator('.cbi-button-positive').click();
	await expect(page.locator('.cbi-section-table-row[data-sid="lte"]')).toContainText('ifb4lte');
	await page.locator('.cbi-page-actions .cbi-button-apply').click();

	await expect.poll(() => vm('uci -q get cake-autorate.lte.ul_max_rate_kbps || true').trim(), { timeout: 30000 })
		.toBe('35000');
	expect(vm('uci get cake-autorate.lte.dl_min_rate_kbps').trim()).toBe('5000');
	await expect.poll(() => vm('cat /var/etc/cake-autorate.yaml 2>/dev/null || true'), { timeout: 30000 })
		.toContain('"name": "lte"');
});

test('page shows the errors of an invalid configuration', async ({ page }) => {
	// No reflector of the link replies to ICMP timestamps.
	vm(`uci set cake-autorate.global.ping_method=icmp-timestamp &&
		uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.wan.reflectors=8.8.8.8 &&
		uci commit cake-autorate &&
		/etc/init.d/cake-autorate restart; true`);
	await page.reload();
	const box = page.locator('.alert-message', { hasText: 'did not start' });
	await expect(box).toContainText('no reflector replies to ICMP timestamp requests');

	// The page reads the error file again each 5 seconds.
	resetConfig();
	await expect(box).toHaveCount(0, { timeout: 15000 });
});

test('missing enabled option shows the service as enabled', async ({ page }) => {
	// The converter starts the service if the option is not set.
	vm('uci delete cake-autorate.global.enabled && uci commit cake-autorate');
	await page.reload();
	await expect(page.locator('input[data-widget-id="widget.cbid.cake-autorate.global.enabled"]')).toBeChecked();
	resetConfig();
});

test('interface names like JavaScript object keys are not reported as used', async ({ page }) => {
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	const ul = modal.locator('[data-name="ul_interface"] .cbi-dropdown');
	await ul.click();
	const custom = ul.locator('input.create-item-input').first();
	await custom.fill('constructor');
	await custom.press('Enter');
	await expect(page.locator('[id="cbid.cake-autorate.wan.ul_interface"]')).not.toHaveClass(INVALID);
});

test('errors after Save & Apply show without a reload', async ({ page }) => {
	// Only 8.8.8.8: it does not reply to ICMP timestamps.
	vm(`uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.wan.reflectors=8.8.8.8 &&
		uci commit cake-autorate`);
	await page.reload();
	await input(page, 'global', 'ping_method').selectOption('icmp-timestamp');
	await page.locator('.cbi-page-actions .cbi-button-apply').click();
	await expect(page.locator('.alert-message', { hasText: 'did not start' }))
		.toContainText('no reflector replies to ICMP timestamp requests', { timeout: 30000 });
	resetConfig();
});
