// SPDX-License-Identifier: GPL-2.0
//
// Helpers of the browser tests: commands in the OpenWrt VM, the test
// configuration and the login.

const { execFileSync } = require('child_process');
const path = require('path');

const VM = path.resolve(__dirname, '../../../scripts/openwrt-vm.sh');

// Runs the shell command cmd in the VM and returns its output.
function vm(cmd) {
	return execFileSync(VM, [ 'ssh', cmd ], { encoding: 'utf8' });
}

// The configuration of the tests: one link "wan" with CAKE on eth1 and
// br-lan, like scripts/openwrt-vm-test.sh.
function resetConfig() {
	vm(`tc qdisc replace dev eth1 root cake bandwidth 20mbit &&
		tc qdisc replace dev br-lan root cake bandwidth 80mbit &&
		for s in $(uci -q show cake-autorate | sed -n 's/^cake-autorate\\.\\([^.=]*\\)=\\(link\\|global\\)$/\\1/p'); do
			uci delete cake-autorate.$s
		done
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

// Logs in to LuCI as user (default root, with the empty password of the
// VM).
async function login(page, user = 'root') {
	await page.goto('/cgi-bin/luci/');
	await page.fill('input[name=luci_username]', user);
	await page.fill('input[name=luci_password]', '');
	await page.locator('button.cbi-button-positive, button[type=submit]').first().click();
	await page.waitForLoadState('networkidle');
}

module.exports = { vm, resetConfig, login };
