// SPDX-License-Identifier: GPL-2.0
//
// Configuration page of cake-autorate-go: the global section and the links.
// An empty field uses the default value of the program. The placeholders
// show these values (see "cake-autorate-go -defaults").

'use strict';
'require view';
'require form';
'require fs';
'require poll';
'require uci';
'require tools.widgets as widgets';

const ERROR_FILE = '/var/run/cake-autorate/config-error';

// The rules of the program (config.go).
const IFNAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$/;
const LINK_NAME_RE = /^[A-Za-z0-9_.-]{1,64}$/;

// The default reflectors for ICMP echo. They do not reply to ICMP
// timestamp requests (config.go, noTimestampReply).
const ECHO_REFLECTORS = [
	'1.1.1.1', '1.0.0.1', '8.8.8.8', '8.8.4.4',
	'9.9.9.9', '149.112.112.112', '208.67.222.222', '208.67.220.220'
];

const DIRECTIONS = [
	[ 'dl', _('Download') ],
	[ 'ul', _('Upload') ]
];

// Returns the value of the option name in the form. If it is empty, returns
// def.
function formNumber(section, sid, name, def) {
	const v = section.formvalue(sid, name);
	return (v == null || v === '') ? def : +v;
}

// Returns true if the value is empty. An empty value uses the default.
function isEmpty(value) {
	return value == null || value === '';
}

// Returns a validate function for the half-open range (lo, hi].
function rangeOpenClosed(lo, hi) {
	return function(sid, value) {
		if (isEmpty(value))
			return true;
		const v = +value;
		if (isNaN(v) || v <= lo || v > hi)
			return _('Must be more than %s and not more than %s').format(lo, hi);
		return true;
	};
}

// Returns a validate function for a value that must be more than 0.
function positive(sid, value) {
	return (isEmpty(value) || +value > 0) ? true : _('Must be more than 0');
}

// Adds the validate function fn to the option o. The checks run in order,
// the first error stops them.
function addCheck(o, fn) {
	const prev = o.validate;
	o.validate = function(sid, value) {
		const r = prev ? prev.call(this, sid, value) : true;
		return r !== true ? r : fn.call(this, sid, value);
	};
}

// Checks that the option low is not more than the option high. An empty
// field uses its default value. Both fields get the check, thus a change
// of one field also checks it against the default value of the other.
function pair(s, low, lowDef, high, highDef, lowMsg, highMsg) {
	const check = function(msg) {
		return function(sid, value) {
			if (formNumber(this.section, sid, low, lowDef) > formNumber(this.section, sid, high, highDef))
				return msg;
			return true;
		};
	};
	addCheck(s.children.find(o => o.option == low), check(lowMsg));
	addCheck(s.children.find(o => o.option == high), check(highMsg));
}

// Returns the list value v as an array.
function toList(v) {
	if (v == null || v === '')
		return [];
	return Array.isArray(v) ? v : [ v ];
}

// Returns an error if the ping method is icmp-timestamp and no reflector
// replies to ICMP timestamp requests. The program does not start then. An
// empty list uses the default list of the method, thus it is correct.
function timestampError(method, reflectors, where) {
	if (method != 'icmp-timestamp' || reflectors.length == 0)
		return true;
	if (reflectors.some((r) => !ECHO_REFLECTORS.includes(r)))
		return true;
	return _('%s: no reflector replies to ICMP timestamp requests. Add other reflectors, or remove all reflectors to use the default list for icmp-timestamp.').format(where);
}

// The global section and the link section of the page. The ping checks
// read the values of the form from them.
const page = { global: null, globalSid: null, links: null };

// Returns the value of the option name of section sid: the value in the
// form if the form shows the field, else the value in UCI. An empty field
// in the form stays empty.
function live(section, sid, name) {
	const v = section ? section.formvalue(sid, name) : null;
	return v != null ? v : uci.get('cake-autorate', sid, name);
}

// Checks the effective ping method of each enabled link with its effective
// reflectors. section and sid are the section and the ID of the field
// under check: a link dialog has its own form. If only is set, checks only
// that link.
function pingMethodError(section, sid, only) {
	const gsec = section && sid == page.globalSid ? section : page.global;
	const gmethod = live(gsec, page.globalSid, 'ping_method') || 'icmp';
	const grefl = toList(live(gsec, page.globalSid, 'reflectors'));
	for (const l of uci.sections('cake-autorate', 'link')) {
		const lsid = l['.name'];
		if (only && lsid != only)
			continue;
		const lsec = lsid == sid ? section : page.links;
		if (live(lsec, lsid, 'enabled') == '0')
			continue;
		const method = live(lsec, lsid, 'ping_method') || gmethod;
		const refl = toList(live(lsec, lsid, 'reflectors'));
		const r = timestampError(method, refl.length ? refl : grefl,
			_('Link "%s"').format(live(lsec, lsid, 'name') || lsid));
		if (r !== true)
			return r;
	}
	return true;
}

function validateGlobalPingMethod(sid, value) {
	return pingMethodError(this.section, sid, null);
}

function validateLinkPingMethod(sid, value) {
	return pingMethodError(this.section, sid, sid);
}

function validateIfname(sid, value) {
	if (isEmpty(value) || IFNAME_RE.test(value))
		return true;
	return _('Use 1 to 15 characters of a-z, A-Z, 0-9, ".", "_" and "-". The first character must be a letter or a digit.');
}

// Adds a field to the tab of the section s. def is the default value of the
// program, shown as placeholder.
function field(s, tab, cls, name, title, def, description) {
	const o = s.taboption(tab, cls, name, title, description);
	if (def != null)
		o.placeholder = String(def);
	o.rmempty = true;
	return o;
}

// Adds a flag. A flag that is on by default must write "0", because an
// empty value is the default (on).
function flag(s, tab, name, title, def, description) {
	const o = s.taboption(tab, form.Flag, name, title, description);
	o.default = def ? '1' : '0';
	o.rmempty = !def;
	return o;
}

function number(s, tab, name, title, def, datatype, description) {
	const o = field(s, tab, form.Value, name, title, def, description);
	o.datatype = datatype;
	return o;
}

// Returns the names of the interfaces that the other link sections use.
function otherInterfaces(sid) {
	// No prototype: names such as "constructor" are valid interface names.
	const used = Object.create(null);
	for (const s of uci.sections('cake-autorate', 'link')) {
		if (s['.name'] == sid)
			continue;
		for (const d of [ 'dl', 'ul' ])
			if (s[d + '_interface'])
				used[s[d + '_interface']] = s.name || s['.name'];
	}
	return used;
}

// sid is the name of the global section.
function globalSection(m, sid) {
	const s = m.section(form.NamedSection, sid, 'global', _('Service'));
	page.global = s;
	page.globalSid = sid;
	s.addremove = false;

	s.tab('general', _('General'));
	s.tab('idle', _('Idle and stall'));
	s.tab('thresholds', _('Delay thresholds'));
	s.tab('advanced', _('Advanced'));
	s.tab('logging', _('Logging'));

	let o;

	o = s.taboption('general', form.Flag, 'enabled', _('Enabled'),
		_('Start the service. The service also needs at least one enabled link.'));
	// The converter starts the service if the option is not set. The
	// package configuration sets '0'.
	o.default = '1';
	o.rmempty = false;

	o = field(s, 'general', form.ListValue, 'ping_method', _('Ping method'), null,
		_('ICMP timestamps measure the delay of each direction. Not all reflectors reply to them.'));
	o.value('', _('Default (icmp)'));
	o.value('icmp', _('ICMP echo (icmp)'));
	o.value('icmp-timestamp', _('ICMP timestamp (icmp-timestamp)'));
	o.validate = validateGlobalPingMethod;

	number(s, 'general', 'pinger_count', _('Pingers'), 6, 'and(uinteger,min(1))',
		_('The number of reflectors that the service pings at the same time.'));
	number(s, 'general', 'ping_interval_ms', _('Ping interval (ms)'), 300, 'and(uinteger,min(1))',
		_('The interval between two pings to one reflector.'));

	o = field(s, 'general', widgets.DeviceSelect, 'ping_interface', _('Ping interface'), null,
		_('Send the pings through this interface. A link can set a different interface.'));
	o.noaliases = true;
	o.optional = true;
	o.validate = validateIfname;

	o = number(s, 'general', 'ping_source_addr', _('Ping source address'), null, 'ip4addr("nomask")',
		_('The source address of the pings. A link can set a different address.'));

	o = s.taboption('general', form.DynamicList, 'reflectors', _('Reflectors'),
		_('The hosts that the service pings, for the links that do not set reflectors. Empty: the default list for the ping method.'));
	o.datatype = 'or(ip4addr("nomask"),hostname)';

	flag(s, 'general', 'learn_cross_delay', _('Learn the cross delay'), false,
		_('Learn the upload delay that download traffic causes (for example on 5G). A link can set a different value.'));
	flag(s, 'general', 'refractory_on_decrease', _('Refractory period after a decrease'), false);
	flag(s, 'general', 'repeat_decrease_on_rise', _('Decrease again when the delay increases'), false);

	flag(s, 'idle', 'enable_sleep_function', _('Sleep when idle'), true,
		_('Stop the pings when the link is idle for some time.'));
	number(s, 'idle', 'connection_active_thr_kbps', _('Active threshold (kbit/s)'), 2000, 'uinteger',
		_('Below this load, the link is idle.'));
	number(s, 'idle', 'sustained_idle_sleep_thr_s', _('Idle time before sleep (s)'), 60, 'ufloat');
	flag(s, 'idle', 'min_shaper_rates_enforcement', _('Minimum rate when idle'), false,
		_('Set the minimum rate when the link is idle or stalled.'));
	number(s, 'idle', 'stall_detection_thr', _('Stall threshold (lost pings)'), 5, 'and(uinteger,min(1))');
	number(s, 'idle', 'connection_stall_thr_kbps', _('Stall load threshold (kbit/s)'), 10, 'uinteger');
	number(s, 'idle', 'global_ping_response_timeout_s', _('Ping response timeout (s)'), 10, 'ufloat',
		_('When no reflector replies for this time, the service starts the pingers again.'));
	number(s, 'idle', 'startup_wait_s', _('Wait at start (s)'), 0, 'ufloat');

	for (const [ d, title ] of DIRECTIONS) {
		let t;
		t = number(s, 'thresholds', d + '_owd_delta_delay_thr_ms', _('%s: bufferbloat threshold (ms)').format(title), 30, 'ufloat',
			_('A delay increase above this value is a bufferbloat sample. Applies to the links that do not set it.'));
		t.validate = positive;
		t = number(s, 'thresholds', d + '_avg_owd_delta_max_adjust_up_thr_ms', _('%s: maximum increase threshold (ms)').format(title), 10, 'ufloat',
			_('Below this average delay increase, the rate increases by the maximum step.'));
		t.validate = positive;
		t = number(s, 'thresholds', d + '_avg_owd_delta_max_adjust_down_thr_ms', _('%s: maximum decrease threshold (ms)').format(title), 60, 'ufloat',
			_('Above this average delay increase, the rate decreases by the maximum step.'));
		t.validate = positive;
	}

	o = s.taboption('advanced', form.DummyValue, '_advanced_note');
	o.rawhtml = true;
	o.default = '<em>%s</em>'.format(_('Change these values only if you know the effect. The defaults are good for most links.'));

	o = number(s, 'advanced', 'bufferbloat_detection_window', _('Bufferbloat window (samples)'), 6, 'and(uinteger,min(1))');
	o = number(s, 'advanced', 'bufferbloat_detection_thr', _('Bufferbloat threshold (samples)'), 3, 'and(uinteger,min(1))',
		_('The number of bufferbloat samples in the window that decreases the rate.'));

	o = number(s, 'advanced', 'alpha_baseline_increase', _('Baseline increase factor'), 0.001, 'ufloat');
	o.validate = rangeOpenClosed(0, 1);
	o = number(s, 'advanced', 'alpha_baseline_decrease', _('Baseline decrease factor'), 0.9, 'ufloat');
	o.validate = rangeOpenClosed(0, 1);
	o = number(s, 'advanced', 'alpha_delta_ewma', _('Delay average factor'), 0.095, 'ufloat');
	o.validate = rangeOpenClosed(0, 1);

	o = number(s, 'advanced', 'shaper_rate_min_adjust_down_bufferbloat', _('Minimum decrease on bufferbloat'), 0.99, 'ufloat');
	o.validate = rangeOpenClosed(0, 1);
	o = number(s, 'advanced', 'shaper_rate_max_adjust_down_bufferbloat', _('Maximum decrease on bufferbloat'), 0.75, 'ufloat',
		_('A smaller factor is a larger decrease.'));
	o.validate = rangeOpenClosed(0, 1);
	o = number(s, 'advanced', 'shaper_rate_min_adjust_up_load_high', _('Minimum increase on high load'), 1, 'min(1)');
	o = number(s, 'advanced', 'shaper_rate_max_adjust_up_load_high', _('Maximum increase on high load'), 1.04, 'min(1)');
	o = number(s, 'advanced', 'shaper_rate_adjust_down_load_low', _('Decrease on low load'), 0.99, 'ufloat');
	o.validate = rangeOpenClosed(0, 1);
	number(s, 'advanced', 'shaper_rate_adjust_up_load_low', _('Increase on low load'), 1.01, 'min(1)');
	number(s, 'advanced', 'high_load_thr', _('High load threshold'), 0.75, 'range(0,1)',
		_('The part of the shaper rate (0 to 1) above which the load is high.'));

	number(s, 'advanced', 'monitor_interval_ms', _('Load monitor interval (ms)'), 200, 'and(uinteger,min(1))');
	number(s, 'advanced', 'bufferbloat_refractory_period_ms', _('Bufferbloat refractory period (ms)'), 300, 'uinteger');
	number(s, 'advanced', 'decay_refractory_period_ms', _('Decay refractory period (ms)'), 1000, 'uinteger');
	o = number(s, 'advanced', 'reflector_response_deadline_s', _('Reflector response deadline (s)'), 1, 'ufloat');
	o.validate = positive;
	number(s, 'advanced', 'reflector_misbehaving_detection_window', _('Reflector check window (samples)'), 60, 'and(uinteger,min(1))');
	o = number(s, 'advanced', 'reflector_misbehaving_detection_thr', _('Reflector check threshold (samples)'), 3, 'and(uinteger,min(1))',
		_('The number of bad samples in the window that replaces a reflector.'));

	pair(s, 'bufferbloat_detection_thr', 3, 'bufferbloat_detection_window', 6,
		_('Must not be more than the bufferbloat window'),
		_('Must not be less than the bufferbloat threshold'));
	pair(s, 'reflector_misbehaving_detection_thr', 3, 'reflector_misbehaving_detection_window', 60,
		_('Must not be more than the reflector check window'),
		_('Must not be less than the reflector check threshold'));
	// The maximum decrease is the smaller factor.
	pair(s, 'shaper_rate_max_adjust_down_bufferbloat', 0.75, 'shaper_rate_min_adjust_down_bufferbloat', 0.99,
		_('Must not be more than the minimum decrease factor'),
		_('Must not be less than the maximum decrease factor'));
	pair(s, 'shaper_rate_min_adjust_up_load_high', 1, 'shaper_rate_max_adjust_up_load_high', 1.04,
		_('Must not be more than the maximum increase factor'),
		_('Must not be less than the minimum increase factor'));

	flag(s, 'logging', 'log_to_file', _('Log to a file'), true);
	o = field(s, 'logging', form.Value, 'log_file_path', _('Log file'), '/var/log/cake-autorate.log');
	// The program stops at the start if it cannot open the file.
	o.validate = function(sid, value) {
		if (isEmpty(value) || /^\/[^\0]*[^\/]$/.test(value))
			return true;
		return _('Use an absolute path of a file, for example /var/log/cake-autorate.log');
	};
	number(s, 'logging', 'log_file_max_size_kb', _('Maximum log file size (KB)'), 2000, 'and(uinteger,min(1))');
	flag(s, 'logging', 'debug', _('Debug messages'), false);
}

// Validates the rate key of the direction d: min <= base <= max, all > 0.
// Each field shows only the errors of its own value. A link must set its
// rates: the program does not use default rates for a link.
function validateRate(d, key) {
	return function(sid, value) {
		if (isEmpty(value))
			return _('Enter a rate');
		if (+value <= 0)
			return _('Must be more than 0');
		// An empty partner field has its own error. Do not compare with it.
		const min = formNumber(this.section, sid, d + '_min_rate_kbps', NaN);
		const base = formNumber(this.section, sid, d + '_base_rate_kbps', NaN);
		const max = formNumber(this.section, sid, d + '_max_rate_kbps', NaN);
		if (key != 'max_rate_kbps' && base < min)
			return key == 'min_rate_kbps'
				? _('Must not be more than the base rate')
				: _('Must not be less than the minimum rate');
		if (key != 'min_rate_kbps' && max < base)
			return key == 'max_rate_kbps'
				? _('Must not be less than the base rate')
				: _('Must not be more than the maximum rate');
		return true;
	};
}

// Shows the rates in Mbit/s in the grid.
function mbit(kbps) {
	return isEmpty(kbps) ? '-' : '%.1f'.format(kbps / 1000);
}

function linkSection(m) {
	const s = m.section(form.GridSection, 'link', _('Links'),
		_('A link is one WAN connection with its download and upload shaper. The shapers must have CAKE (for example from SQM).'));
	page.links = s;
	s.addremove = true;
	s.anonymous = false;
	s.sortable = true;
	s.nodescriptions = true;
	s.addbtntitle = _('Add link');

	s.tab('interfaces', _('Interfaces'));
	s.tab('rates', _('Rates'));
	s.tab('thresholds', _('Delay thresholds'));
	s.tab('pings', _('Pings'));

	let o;

	o = s.taboption('interfaces', form.Flag, 'enabled', _('Enabled'));
	o.default = '1';
	o.rmempty = false;
	o.editable = true;

	o = s.taboption('interfaces', form.Value, 'name', _('Name'),
		_('The name in the log and the status. Empty: the section name.'));
	o.placeholder = _('section name');
	o.validate = function(sid, value) {
		// The converter uses the section name only for a named section.
		if (isEmpty(value) && uci.get('cake-autorate', sid, '.anonymous'))
			return _('Enter a name');
		const name = isEmpty(value) ? sid : value;
		if (!LINK_NAME_RE.test(name))
			return _('Use 1 to 64 characters of a-z, A-Z, 0-9, ".", "_" and "-"');
		for (const other of uci.sections('cake-autorate', 'link'))
			if (other['.name'] != sid && (other.name || other['.name']) == name)
				return _('The link "%s" has this name').format(other['.name']);
		return true;
	};
	o.modalonly = true;

	for (const [ d, title ] of DIRECTIONS) {
		o = s.taboption('interfaces', widgets.DeviceSelect, d + '_interface', _('%s interface').format(title),
			d == 'dl'
				? _('The interface with the CAKE shaper for the download, usually an IFB (for example ifb4wan).')
				: _('The interface with the CAKE shaper for the upload, usually the WAN device.'));
		o.noaliases = true;
		o.rmempty = false;
		o.validate = function(sid, value) {
			if (isEmpty(value))
				return _('Select an interface');
			const r = validateIfname(sid, value);
			if (r !== true)
				return r;
			const other = this.section.formvalue(sid, (d == 'dl' ? 'ul' : 'dl') + '_interface');
			if (other == value)
				return _('Download and upload must use different interfaces');
			const used = otherInterfaces(sid);
			if (used[value])
				return _('The link "%s" uses this interface').format(used[value]);
			return true;
		};

		o = s.taboption('interfaces', form.Flag, d + '_adjust', _('Adjust the %s rate').format(title.toLowerCase()),
			_('Off: the service measures, but does not change the rate of this shaper.'));
		o.default = '1';
		o.rmempty = false;
		o.modalonly = true;
	}

	for (const [ d, title ] of DIRECTIONS) {
		const defMax = d == 'dl' ? 80000 : 35000;
		for (const [ key, label, def ] of [
			[ 'min_rate_kbps', _('%s minimum rate (kbit/s)'), 5000 ],
			[ 'base_rate_kbps', _('%s base rate (kbit/s)'), 20000 ],
			[ 'max_rate_kbps', _('%s maximum rate (kbit/s)'), defMax ]
		]) {
			o = number(s, 'rates', d + '_' + key, label.format(title), def, 'uinteger',
				key == 'base_rate_kbps'
					? _('The rate at the start, after an idle time and on a pause. 1000 kbit/s = 1 Mbit/s.')
					: _('1000 kbit/s = 1 Mbit/s.'));
			o.validate = validateRate(d, key);
			o.modalonly = true;
			// A new link gets the default rates.
			o.default = String(def);
			o.rmempty = false;
		}
	}

	o = s.option(form.DummyValue, '_dl_rates', _('Download base / max (Mbit/s)'));
	o.modalonly = false;
	o.textvalue = function(sid) {
		return '%s / %s'.format(mbit(uci.get('cake-autorate', sid, 'dl_base_rate_kbps')),
			mbit(uci.get('cake-autorate', sid, 'dl_max_rate_kbps')));
	};
	o = s.option(form.DummyValue, '_ul_rates', _('Upload base / max (Mbit/s)'));
	o.modalonly = false;
	o.textvalue = function(sid) {
		return '%s / %s'.format(mbit(uci.get('cake-autorate', sid, 'ul_base_rate_kbps')),
			mbit(uci.get('cake-autorate', sid, 'ul_max_rate_kbps')));
	};

	o = s.taboption('thresholds', form.DummyValue, '_thresholds_note');
	o.rawhtml = true;
	o.modalonly = true;
	o.default = '<em>%s</em>'.format(_('Empty: the value of the global section. Do not set the upload values below the scheduling delay of the link (on 5G approximately 25 ms): the upload rate can then decrease to the minimum.'));

	for (const [ d, title ] of DIRECTIONS) {
		for (const [ key, label ] of [
			[ 'owd_delta_delay_thr_ms', _('%s: bufferbloat threshold (ms)') ],
			[ 'avg_owd_delta_max_adjust_up_thr_ms', _('%s: maximum increase threshold (ms)') ],
			[ 'avg_owd_delta_max_adjust_down_thr_ms', _('%s: maximum decrease threshold (ms)') ]
		]) {
			o = number(s, 'thresholds', d + '_' + key, label.format(title), null, 'ufloat');
			o.placeholder = _('global value');
			o.modalonly = true;
			o.validate = positive;
		}
	}

	o = s.taboption('pings', form.DynamicList, 'reflectors', _('Reflectors'),
		_('Empty: the reflectors of the global section, or the default list for the ping method.'));
	o.datatype = 'or(ip4addr("nomask"),hostname)';
	o.modalonly = true;

	o = s.taboption('pings', form.ListValue, 'ping_method', _('Ping method'));
	o.value('', _('Global value'));
	o.value('icmp', _('ICMP echo (icmp)'));
	o.value('icmp-timestamp', _('ICMP timestamp (icmp-timestamp)'));
	o.validate = validateLinkPingMethod;
	o.modalonly = true;

	o = s.taboption('pings', form.Value, 'pinger_count', _('Pingers'));
	o.datatype = 'and(uinteger,min(1))';
	o.placeholder = _('global value');
	o.modalonly = true;

	o = s.taboption('pings', form.Value, 'ping_interval_ms', _('Ping interval (ms)'));
	o.datatype = 'and(uinteger,min(1))';
	o.placeholder = _('global value');
	o.modalonly = true;

	o = s.taboption('pings', form.ListValue, 'learn_cross_delay', _('Learn the cross delay'));
	o.value('', _('Global value'));
	o.value('1', _('On'));
	o.value('0', _('Off'));
	o.modalonly = true;

	o = s.taboption('pings', widgets.DeviceSelect, 'ping_interface', _('Ping interface'),
		_('Empty: the ping interface of the global section.'));
	o.noaliases = true;
	o.optional = true;
	o.modalonly = true;
	o.validate = validateIfname;

	o = s.taboption('pings', form.Value, 'ping_source_addr', _('Ping source address'),
		_('Empty: the ping source address of the global section.'));
	o.datatype = 'ip4addr("nomask")';
	o.modalonly = true;
}

function renderError(text) {
	if (isEmpty(text) || !text.trim())
		return E('div');
	return E('div', { 'class': 'alert-message warning' }, [
		E('h4', {}, _('The service did not start: the configuration is not valid')),
		// A text node: the errors contain UCI values, E() reads a string
		// as HTML.
		E('pre', { 'style': 'white-space: pre-wrap' }, document.createTextNode(text.trim()))
	]);
}

return view.extend({
	load: function() {
		return Promise.all([
			L.resolveDefault(fs.read(ERROR_FILE), ''),
			uci.load('cake-autorate')
		]);
	},

	render: function(data) {
		const m = new form.Map('cake-autorate', _('CAKE Autorate (Go)'),
			_('cake-autorate-go changes the rate of CAKE shapers to follow the capacity of a variable link (for example LTE, 5G or Starlink). Empty fields use the default value of the program.'));

		// The converter accepts a global section with any name, also an
		// anonymous one. Without a global section, add one.
		const global = uci.sections('cake-autorate', 'global')[0];
		globalSection(m, global ? global['.name'] : uci.add('cake-autorate', 'global', 'global'));
		linkSection(m);

		return m.render().then(function(node) {
			// The box is not in the form: LuCI makes the form again after
			// a save and would remove the box.
			const box = E('div', {}, renderError(data[0]));
			// The service starts after "Save & Apply". Show its errors
			// when they come.
			poll.add(function() {
				return L.resolveDefault(fs.read(ERROR_FILE), '').then(function(text) {
					box.replaceChildren(renderError(text));
				});
			}, 5);
			return E('div', {}, [ box, node ]);
		});
	}
});
