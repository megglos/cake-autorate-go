// SPDX-License-Identifier: GPL-2.0
//
// Makes the screenshots of the LuCI app for the documentation, from the
// OpenWrt VM. Start the VM, install the packages and put load on the link
// first, so that the graphs show data.
//
// Usage: node screenshots.js [OUTPUT_DIR] [SECONDS]
//   OUTPUT_DIR  directory for the PNG files (default: ../../../docs/images)
//   SECONDS     time that the status page collects data before the
//               screenshot (default: 240)
//
// Environment: LUCI_URL (default http://localhost:8080), CHROMIUM_PATH
// (optional, a Chromium binary).

const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.LUCI_URL || 'http://localhost:' + (process.env.VM_HTTP_PORT || '8080');
const OUT = path.resolve(process.argv[2] || path.join(__dirname, '../../../docs/images'));
const SECONDS = +(process.argv[3] || 240);

// The time for the fade animations of LuCI.
const SETTLE_MS = 1500;

// Removes the LuCI warning about an empty root password: the VM has no
// password.
function hidePasswordWarning(page) {
	return page.evaluate(() => {
		for (const e of document.querySelectorAll('.alert-message'))
			if (/No password set/.test(e.textContent))
				e.remove();
	});
}

(async () => {
	const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined });
	const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
	try {
		await page.goto(BASE + '/cgi-bin/luci/');
		await page.fill('input[name=luci_username]', 'root');
		await page.fill('input[name=luci_password]', '');
		await page.locator('button.cbi-button-positive, button[type=submit]').first().click();
		await page.waitForLoadState('networkidle');

		// Status page: the graphs of the last 5 minutes, with the cursor.
		await page.goto(BASE + '/cgi-bin/luci/admin/network/cake-autorate/status');
		const link = page.locator('#cake-autorate-status .cbi-section[data-link]').first();
		await link.waitFor();
		console.log('status page: collecting data for %d s', SECONDS);
		await page.waitForTimeout(SECONDS * 1000);
		const box = await link.locator('[data-graph="rate"] svg').boundingBox();
		await page.mouse.move(box.x + box.width * 0.7, box.y + box.height / 2);
		await link.screenshot({ path: path.join(OUT, 'luci-status.png') });

		// Configuration page and the dialog of a link.
		await page.goto(BASE + '/cgi-bin/luci/admin/network/cake-autorate/config');
		await page.locator('.cbi-section-table-row[data-sid]').first().waitFor();
		await hidePasswordWarning(page);
		await page.waitForTimeout(SETTLE_MS);
		await page.locator('#maincontent').screenshot({ path: path.join(OUT, 'luci-config.png') });
		await page.locator('.cbi-section-table-row[data-sid] .cbi-button-edit').first().click();
		const modal = page.locator('#modal_overlay .modal');
		await modal.waitFor();
		await page.waitForTimeout(SETTLE_MS);
		await modal.screenshot({ path: path.join(OUT, 'luci-link.png') });

		// Overview section.
		await page.goto(BASE + '/cgi-bin/luci/admin/status/overview');
		const row = page.locator('tr[data-link]').first();
		await row.waitFor({ timeout: 30000 });
		await page.waitForTimeout(SETTLE_MS);
		await row.locator('xpath=ancestor::div[contains(@class,"cbi-section")][1]').screenshot({ path: path.join(OUT, 'luci-overview.png') });

		console.log('screenshots in %s', OUT);
	}
	finally {
		await browser.close();
	}
})().catch((e) => {
	console.error(e);
	process.exit(1);
});
