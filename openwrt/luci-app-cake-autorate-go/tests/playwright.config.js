// SPDX-License-Identifier: GPL-2.0
//
// The tests use the LuCI of the VM of scripts/openwrt-vm.sh (port 8080).
// They change the configuration in the VM, thus they run one after the
// other.

const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
	testDir: '.',
	workers: 1,
	fullyParallel: false,
	retries: 0,
	timeout: 60000,
	reporter: [ [ 'list' ] ],
	outputDir: 'test-results',
	use: {
		baseURL: process.env.LUCI_URL || 'http://localhost:' + (process.env.VM_HTTP_PORT || '8080'),
		viewport: { width: 1280, height: 1000 },
		screenshot: 'only-on-failure',
		trace: 'retain-on-failure',
		launchOptions: {
			// A local Chromium, if the Playwright download is not possible.
			executablePath: process.env.CHROMIUM_PATH || undefined
		}
	}
});
