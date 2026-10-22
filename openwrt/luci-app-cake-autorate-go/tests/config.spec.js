// SPDX-License-Identifier: GPL-2.0
//
// Browser tests of the configuration page in the OpenWrt VM. Start the VM
// and install the two packages first (see scripts/openwrt-vm.sh). The
// tests set their own configuration in the VM.

const { test, expect } = require('@playwright/test');
const { vm, resetConfig, login } = require('./helpers');

const PAGE = '/cgi-bin/luci/admin/network/cake-autorate/config';

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

// Clicks Save & Apply and waits for the end of the apply. LuCI applies
// with a rollback timer: a second apply before the end of the first one
// is lost.
async function saveApply(page) {
	await page.locator('.cbi-page-actions .cbi-button-apply').click();
	await expect(page.getByText('Configuration changes applied')).toBeVisible({ timeout: 60000 });
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
	await login(page);
	// Only the errors of the page under test, not of the login page.
	errors = [];
	page.on('pageerror', (e) => errors.push(e.message));
	await page.goto(PAGE);
	await page.locator('.cbi-section').first().waitFor();
});

test.afterEach(() => {
	expect(errors, 'JavaScript errors on the page').toEqual([]);
});

// Selects a value in a LuCI dropdown. A click while the dropdown opens,
// closes or the page refreshes can get lost. Thus, try again until the
// dropdown shows the value.
async function selectInterface(dropdown, value) {
	await expect(async () => {
		await dropdown.click();
		await dropdown.locator(`li[data-value="${value}"][tabindex]`).click({ timeout: 2000 });
		await expect(dropdown.locator(`li[data-value="${value}"][selected]`)).toHaveCount(1, { timeout: 1000 });
	}).toPass({ timeout: 15000 });
}

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
	await selectInterface(ul, 'eth1');
	await expect(page.locator('[id="cbid.cake-autorate.lte.ul_interface"]')).toHaveClass(INVALID);
	await selectInterface(ul, 'eth0');
	await expect(page.locator('[id="cbid.cake-autorate.lte.ul_interface"]')).not.toHaveClass(INVALID);

	await modal.locator('.cbi-button-positive').click();
	await expect(page.locator('.cbi-section-table-row[data-sid="lte"]')).toContainText('ifb4lte');
	await saveApply(page);

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
	// The page does not show unknown options. The converter rejects them.
	vm('uci set cake-autorate.wan.unknown_option=1 && uci commit cake-autorate');
	await page.reload();
	await setValue(page, 'global', 'pinger_count', '5');
	await saveApply(page);
	await expect(page.locator('.alert-message', { hasText: 'did not start' }))
		.toContainText('unknown option "unknown_option"', { timeout: 30000 });
	resetConfig();
});

test('error box shows UCI values as text, not as HTML', async ({ page }) => {
	vm(`uci set cake-autorate.wan.dl_min_rate_kbps='<img src=x onerror=window.xss=1>' &&
		uci commit cake-autorate &&
		/etc/init.d/cake-autorate restart; true`);
	await page.reload();
	const box = page.locator('.alert-message', { hasText: 'did not start' });
	await expect(box).toContainText('<img src=x onerror=window.xss=1>');
	await expect(box.locator('img')).toHaveCount(0);
	expect(await page.evaluate(() => window.xss)).toBeUndefined();
	resetConfig();
});

test('anonymous link must have a name', async ({ page }) => {
	const sid = vm('uci add cake-autorate link').trim();
	vm(`uci set cake-autorate.${sid}.name=lte &&
		uci set cake-autorate.${sid}.enabled=0 &&
		uci commit cake-autorate`);
	await page.reload();
	await page.locator(`.cbi-section-table-row[data-sid="${sid}"] .cbi-button-edit`).click();
	const name = input(page, sid, 'name');
	await name.fill('');
	await name.press('Tab');
	await expect(name).toHaveClass(INVALID);
	await name.fill('lte2');
	await name.press('Tab');
	await expect(name).not.toHaveClass(INVALID);
	resetConfig();
});

test('decimal values need a digit before the point, like in the converter', async ({ page }) => {
	await openTab(page, 'advanced');
	await setValue(page, 'global', 'alpha_baseline_decrease', '.5');
	await expect(input(page, 'global', 'alpha_baseline_decrease')).toHaveClass(INVALID);
	await setValue(page, 'global', 'alpha_baseline_decrease', '0.5');
	await expect(input(page, 'global', 'alpha_baseline_decrease')).not.toHaveClass(INVALID);
});

test('anonymous global section shows the service settings', async ({ page }) => {
	// The ID of an anonymous section in LuCI is not the ID that the uci
	// command shows. Find the fields by the option name.
	vm(`uci delete cake-autorate.global &&
		s=$(uci add cake-autorate global) &&
		uci set cake-autorate.$s.enabled=0 &&
		uci set cake-autorate.$s.pinger_count=4 &&
		uci commit cake-autorate`);
	await page.reload();
	await expect(page.locator('[id^="widget.cbid.cake-autorate.cfg"][id$=".pinger_count"]')).toHaveValue('4');
	const enabled = page.locator('input[data-widget-id^="widget.cbid.cake-autorate.cfg"][data-widget-id$=".enabled"]').first();
	await expect(enabled).not.toBeChecked();
	await enabled.check();
	await saveApply(page);
	await expect.poll(() => vm('uci -q get cake-autorate.@global[0].enabled || true').trim(), { timeout: 30000 })
		.toBe('1');
	expect(vm("uci show cake-autorate | grep -c '=global$'").trim()).toBe('1');
	resetConfig();
});

test('missing global section: the page adds one', async ({ page }) => {
	vm('uci delete cake-autorate.global && uci commit cake-autorate');
	await page.reload();
	await expect(page.locator('input[data-widget-id="widget.cbid.cake-autorate.global.enabled"]')).toBeChecked();
	await setValue(page, 'global', 'pinger_count', '5');
	await saveApply(page);
	await expect.poll(() => vm('uci -q get cake-autorate.global.pinger_count || true').trim(), { timeout: 30000 })
		.toBe('5');
	resetConfig();
});

test('icmp-timestamp needs a reflector that replies to timestamps', async ({ page }) => {
	// The link wan has only reflectors that do not reply (1.1.1.1, 8.8.8.8).
	await input(page, 'global', 'ping_method').selectOption('icmp-timestamp');
	await expect(input(page, 'global', 'ping_method')).toHaveClass(INVALID);
	await input(page, 'global', 'ping_method').selectOption('icmp');
	await expect(input(page, 'global', 'ping_method')).not.toHaveClass(INVALID);

	// In the link: other reflectors make the method valid.
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	await openTab(modal, 'pings');
	await input(page, 'wan', 'ping_method').selectOption('icmp-timestamp');
	await expect(input(page, 'wan', 'ping_method')).toHaveClass(INVALID);
	const add = modal.locator('[data-name="reflectors"] input[type=text]').last();
	await add.fill('94.140.14.14');
	await add.press('Enter');
	await expect(input(page, 'wan', 'ping_method')).not.toHaveClass(INVALID);
});

test('log file must be an absolute path', async ({ page }) => {
	await openTab(page, 'logging');
	for (const bad of [ 'cake.log', '/var/log/' ]) {
		await setValue(page, 'global', 'log_file_path', bad);
		await expect(input(page, 'global', 'log_file_path'), bad).toHaveClass(INVALID);
	}
	await setValue(page, 'global', 'log_file_path', '/tmp/cake.log');
	await expect(input(page, 'global', 'log_file_path')).not.toHaveClass(INVALID);
});

test('placeholders show the defaults of the program', async ({ page }) => {
	// The checks of the page use these values for empty fields.
	const defaults = {};
	for (const line of vm('cake-autorate-go -defaults').split('\n')) {
		const m = line.match(/^([a-z_]+): ([0-9.]+)\s*(#.*)?$/);
		if (m)
			defaults[m[1]] = +m[2];
	}
	expect(Object.keys(defaults).length).toBeGreaterThan(20);
	const fields = await page.locator('[id^="widget.cbid.cake-autorate.global."][placeholder]').evaluateAll((l) =>
		l.map((e) => [ e.id.replace('widget.cbid.cake-autorate.global.', ''), e.placeholder ]));
	let checked = 0;
	for (const [ name, placeholder ] of fields) {
		if (!(name in defaults) || isNaN(+placeholder))
			continue;
		expect(+placeholder, name).toBe(defaults[name]);
		checked++;
	}
	expect(checked).toBeGreaterThan(20);
});

test('ping check: a link with its own icmp-timestamp uses changed global reflectors', async ({ page }) => {
	vm(`uci set cake-autorate.wan.ping_method=icmp-timestamp &&
		uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.global.reflectors=94.140.14.14 &&
		uci commit cake-autorate`);
	await page.reload();
	const method = input(page, 'global', 'ping_method');
	await expect(method).not.toHaveClass(INVALID);
	// Replace the global reflector with one that does not reply to
	// timestamps. The link dialog stays closed.
	const list = page.locator('[id="cbid.cake-autorate.global.reflectors"]');
	const add = list.locator('input[type=text]').last();
	await add.fill('8.8.8.8');
	await add.press('Enter');
	// The "×" of an item is on its right side, outside of the text.
	const item = list.locator('.item', { hasText: '94.140.14.14' });
	const box = await item.boundingBox();
	await item.click({ position: { x: box.width - 6, y: box.height / 2 } });
	await expect(list.locator('.item', { hasText: '94.140.14.14' })).toHaveCount(0);
	await expect(method).toHaveClass(INVALID);
	resetConfig();
});

test('ping check: pending changes of the global method and the link are used', async ({ page }) => {
	// Global icmp-timestamp with reflectors that reply. The link wan has
	// its own method icmp and only 8.8.8.8.
	vm(`uci set cake-autorate.global.ping_method=icmp-timestamp &&
		uci add_list cake-autorate.global.reflectors=94.140.14.14 &&
		uci set cake-autorate.wan.ping_method=icmp &&
		uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.wan.reflectors=8.8.8.8 &&
		uci commit cake-autorate`);
	await page.reload();
	await input(page, 'global', 'ping_method').selectOption('icmp');
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	await openTab(page.locator('.modal'), 'pings');
	await input(page, 'wan', 'ping_method').selectOption('');
	await expect(input(page, 'wan', 'ping_method')).not.toHaveClass(INVALID);
	await input(page, 'wan', 'ping_method').selectOption('icmp-timestamp');
	await expect(input(page, 'wan', 'ping_method')).toHaveClass(INVALID);
	resetConfig();
});

test('ping check: a disabled link is not checked', async ({ page }) => {
	vm(`uci add_list cake-autorate.global.reflectors=94.140.14.14 &&
		uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.wan.reflectors=8.8.8.8 &&
		uci commit cake-autorate`);
	await page.reload();
	const method = input(page, 'global', 'ping_method');
	await method.selectOption('icmp-timestamp');
	await expect(method).toHaveClass(INVALID);
	await page.locator('input[data-widget-id="widget.cbid.cake-autorate.wan.enabled"]').uncheck();
	await expect(method).not.toHaveClass(INVALID);
	resetConfig();
});

test('rate error only on the empty field', async ({ page }) => {
	vm(`uci set cake-autorate.wan.dl_min_rate_kbps=5000 &&
		uci set cake-autorate.wan.dl_base_rate_kbps=7500 &&
		uci set cake-autorate.wan.dl_max_rate_kbps=10000 &&
		uci commit cake-autorate`);
	await page.reload();
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	await openTab(page.locator('.modal'), 'rates');
	await setValue(page, 'wan', 'dl_base_rate_kbps', '');
	await expect(input(page, 'wan', 'dl_base_rate_kbps')).toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_min_rate_kbps')).not.toHaveClass(INVALID);
	await expect(input(page, 'wan', 'dl_max_rate_kbps')).not.toHaveClass(INVALID);
	resetConfig();
});

test('ping check: a disabled service is not checked', async ({ page }) => {
	vm(`uci delete cake-autorate.wan.reflectors &&
		uci add_list cake-autorate.wan.reflectors=8.8.8.8 &&
		uci set cake-autorate.global.ping_method=icmp-timestamp &&
		uci commit cake-autorate`);
	await page.reload();
	const method = input(page, 'global', 'ping_method');
	await expect(method).toHaveClass(INVALID);
	await page.locator('input[data-widget-id="widget.cbid.cake-autorate.global.enabled"]').uncheck();
	await expect(method).not.toHaveClass(INVALID);
	resetConfig();
});

test('a disabled link does not use its interfaces', async ({ page }) => {
	vm(`uci set cake-autorate.old=link &&
		uci set cake-autorate.old.dl_interface=ifb4old &&
		uci set cake-autorate.old.ul_interface=eth0 &&
		uci commit cake-autorate`);
	await page.reload();
	const ulInvalid = () => page.locator('[id="cbid.cake-autorate.wan.ul_interface"]');
	const selectEth0 = async () => {
		await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
		const ul = page.locator('.modal [data-name="ul_interface"] .cbi-dropdown');
		await selectInterface(ul, 'eth0');
	};
	// The enabled link "old" uses eth0.
	await selectEth0();
	await expect(ulInvalid()).toHaveClass(INVALID);
	await page.locator('.modal button', { hasText: 'Dismiss' }).click();
	// Clear the Enabled box of "old" in the table, without a save.
	await page.locator('input[data-widget-id="widget.cbid.cake-autorate.old.enabled"]').uncheck();
	await selectEth0();
	await expect(ulInvalid()).not.toHaveClass(INVALID);
	resetConfig();
});

test('bounded decimal fields reject exponents, like the converter', async ({ page }) => {
	await openTab(page, 'advanced');
	for (const [ name, value ] of [
		[ 'shaper_rate_min_adjust_up_load_high', '1e0' ],
		[ 'shaper_rate_max_adjust_up_load_high', '1.5e0' ],
		[ 'shaper_rate_adjust_up_load_low', '1e0' ],
		[ 'high_load_thr', '0.5e0' ]
	]) {
		await setValue(page, 'global', name, value);
		await expect(input(page, 'global', name), name + '=' + value).toHaveClass(INVALID);
		await setValue(page, 'global', name, '');
	}
});

test('saving a link dialog keeps pending service edits', async ({ page }) => {
	await setValue(page, 'global', 'pinger_count', '5');
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	await page.locator('.modal .cbi-button-positive').click();
	await expect(page.locator('.modal')).toBeHidden();
	await expect(input(page, 'global', 'pinger_count')).toHaveValue('5');
	await saveApply(page);
	await expect.poll(() => vm('uci -q get cake-autorate.global.pinger_count || true').trim(), { timeout: 30000 })
		.toBe('5');
	resetConfig();
});

test('a link section with the name "global" does not hide the service settings', async ({ page }) => {
	vm(`uci delete cake-autorate.global &&
		uci set cake-autorate.global=link &&
		uci set cake-autorate.global.enabled=0 &&
		uci set cake-autorate.global.dl_interface=ifb4g &&
		uci set cake-autorate.global.ul_interface=eth0 &&
		uci commit cake-autorate`);
	await page.reload();
	await expect(page.locator('.cbi-section-table-row[data-sid="global"]')).toContainText('ifb4g');
	const count = page.locator('[id^="widget.cbid.cake-autorate."][id$=".pinger_count"]').first();
	await count.fill('5');
	await count.press('Tab');
	await saveApply(page);
	await expect.poll(() => vm("uci show cake-autorate | grep -c '=global$' || true").trim(), { timeout: 30000 })
		.toBe('1');
	expect(vm('uci get cake-autorate.@global[0].pinger_count').trim()).toBe('5');
	expect(vm('uci get cake-autorate.global.dl_interface').trim()).toBe('ifb4g');
	resetConfig();
});

test('enabling a link in the table checks its interfaces', async ({ page }) => {
	// The disabled link "old" uses eth1, the upload interface of "wan".
	vm(`uci set cake-autorate.old=link &&
		uci set cake-autorate.old.enabled=0 &&
		uci set cake-autorate.old.dl_interface=ifb4old &&
		uci set cake-autorate.old.ul_interface=eth1 &&
		uci commit cake-autorate`);
	await page.reload();
	const box = page.locator('input[data-widget-id="widget.cbid.cake-autorate.old.enabled"]');
	await box.check();
	// A checkbox has no error mark. The save shows the error.
	await page.locator('.cbi-page-actions .cbi-button-save').click();
	// Both links use eth1 now. One of them shows the error.
	await expect(page.locator('.modal')).toContainText(/The link "(wan|old)" uses the interface eth1/);
	expect(vm('uci get cake-autorate.old.enabled').trim()).toBe('0');
	// LuCI sends the save error also to the page as an exception.
	errors = errors.filter((e) => !e.includes('uses the interface eth1'));
	resetConfig();
});

test('enabling a link in its dialog uses the changed interfaces', async ({ page }) => {
	// The disabled link "old" uses eth1, the upload interface of "wan".
	vm(`uci set cake-autorate.old=link &&
		uci set cake-autorate.old.enabled=0 &&
		uci set cake-autorate.old.dl_interface=ifb4old &&
		uci set cake-autorate.old.ul_interface=eth1 &&
		uci commit cake-autorate`);
	await page.reload();
	await page.locator('.cbi-section-table-row[data-sid="old"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	const ul = modal.locator('[data-name="ul_interface"] .cbi-dropdown');
	await selectInterface(ul, 'eth0');
	await modal.locator('input[data-widget-id="widget.cbid.cake-autorate.old.enabled"]').check();
	await modal.locator('.cbi-button-positive').click();
	await expect(modal).toBeHidden();
	await saveApply(page);
	await expect.poll(() => vm('uci -q get cake-autorate.old.enabled || true').trim(), { timeout: 30000 })
		.toBe('1');
	expect(vm('uci get cake-autorate.old.ul_interface').trim()).toBe('eth0');
	resetConfig();
});

test('5G options reach the configuration of the program', async ({ page }) => {
	await input(page, 'global', 'mode').selectOption('custom');
	for (const name of [ 'increase_to_delay_thr', 'auto_max_rate' ])
		await page.locator(`input[data-widget-id="widget.cbid.cake-autorate.global.${name}"]`).check();
	await openTab(page, 'advanced');
	await setValue(page, 'global', 'auto_max_rate_limit_factor', '0.5');
	await expect(input(page, 'global', 'auto_max_rate_limit_factor')).toHaveClass(INVALID);
	await setValue(page, 'global', 'auto_max_rate_limit_factor', '2.5');
	await expect(input(page, 'global', 'auto_max_rate_limit_factor')).not.toHaveClass(INVALID);
	await saveApply(page);
	await expect.poll(() => vm('cat /var/etc/cake-autorate.yaml 2>/dev/null || true'), { timeout: 30000 })
		.toContain('"auto_max_rate_limit_factor": 2.5');
	const yaml = vm('cat /var/etc/cake-autorate.yaml');
	expect(yaml).toContain('"auto_max_rate": true');
	expect(yaml).toContain('"increase_to_delay_thr": true');
	expect(yaml).toContain('"mode": "custom"');
	resetConfig();
});

// The checkbox of an option of the modes.
function modeOption(page, name) {
	return page.locator(`input[data-widget-id="widget.cbid.cake-autorate.global.${name}"]`);
}

test('only the mode custom shows the options of the modes', async ({ page }) => {
	await expect(input(page, 'global', 'mode')).toHaveValue('balanced');
	for (const name of [ 'refractory_on_decrease', 'increase_to_delay_thr', 'auto_max_rate' ])
		await expect(modeOption(page, name)).toBeHidden();
	await input(page, 'global', 'mode').selectOption('custom');
	for (const name of [ 'refractory_on_decrease', 'increase_to_delay_thr', 'auto_max_rate' ])
		await expect(modeOption(page, name)).toBeVisible();
	await input(page, 'global', 'mode').selectOption('bandwidth');
	await expect(modeOption(page, 'auto_max_rate')).toBeHidden();
	await saveApply(page);
	await expect.poll(() => vm('cat /var/etc/cake-autorate.yaml 2>/dev/null || true'), { timeout: 30000 })
		.toContain('"mode": "bandwidth"');
	resetConfig();
});

test('a link in the mode custom has its own options of the modes', async ({ page }) => {
	await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
	const modal = page.locator('.modal');
	await openTab(modal, 'rates');
	await expect(input(page, 'wan', 'auto_max_rate')).toBeHidden();
	await input(page, 'wan', 'mode').selectOption('custom');
	await expect(input(page, 'wan', 'auto_max_rate')).toBeVisible();
	await input(page, 'wan', 'auto_max_rate').selectOption('1');
	await input(page, 'wan', 'refractory_on_decrease').selectOption('0');
	await modal.locator('.cbi-button-positive').click();
	await saveApply(page);
	await expect.poll(() => vm('cat /var/etc/cake-autorate.yaml 2>/dev/null || true'), { timeout: 30000 })
		.toContain('"auto_max_rate": true');
	const yaml = vm('cat /var/etc/cake-autorate.yaml');
	expect(yaml).toContain('"mode": "custom"');
	expect(yaml).toContain('"refractory_on_decrease": false');
	resetConfig();
});

test('a configuration from before the modes shows the mode custom', async ({ page }) => {
	vm('uci set cake-autorate.global.refractory_on_decrease=1 && uci commit cake-autorate');
	try {
		await page.reload();
		await expect(input(page, 'global', 'mode')).toHaveValue('custom');
		await expect(modeOption(page, 'refractory_on_decrease')).toBeChecked();
		// Without the last option that is on, the mode stays custom.
		await modeOption(page, 'refractory_on_decrease').uncheck();
		await saveApply(page);
		expect(vm('uci -q get cake-autorate.global.mode').trim()).toBe('custom');
		expect(vm('uci -q get cake-autorate.global.refractory_on_decrease || true').trim()).not.toBe('1');
	}
	finally {
		resetConfig();
	}
});

test('a link from before the global mode keeps the mode custom', async ({ page }) => {
	vm('uci set cake-autorate.wan.auto_max_rate=1 && uci commit cake-autorate');
	try {
		await page.reload();
		await saveApply(page);
		expect(vm('uci -q get cake-autorate.global.mode').trim()).toBe('balanced');
		expect(vm('uci -q get cake-autorate.wan.mode').trim()).toBe('custom');
		await page.locator('.cbi-section-table-row[data-sid="wan"] .cbi-button-edit').click();
		await openTab(page.locator('.modal'), 'rates');
		await expect(input(page, 'wan', 'mode')).toHaveValue('custom');
	}
	finally {
		resetConfig();
	}
});

test('a hidden option of the modes keeps its value', async ({ page }) => {
	vm('uci set cake-autorate.global.mode=custom && uci set cake-autorate.global.refractory_on_decrease=1 && uci commit cake-autorate');
	try {
		await page.reload();
		await expect(modeOption(page, 'refractory_on_decrease')).toBeChecked();
		await input(page, 'global', 'mode').selectOption('balanced');
		await expect(modeOption(page, 'refractory_on_decrease')).toBeHidden();
		await saveApply(page);
		expect(vm('uci -q get cake-autorate.global.mode').trim()).toBe('balanced');
		expect(vm('uci -q get cake-autorate.global.refractory_on_decrease').trim()).toBe('1');
	}
	finally {
		resetConfig();
	}
});
