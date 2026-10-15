// SPDX-License-Identifier: GPL-2.0
//
// Converts the UCI configuration of cake-autorate to the configuration of
// cake-autorate-go. The result is a JSON object. JSON is also YAML, thus
// cake-autorate-go reads it as its YAML configuration file.
//
// UCI has no nested sections. A link section has the keys of a direction
// with the prefix "dl_" (download) or "ul_" (upload). The global section can
// have the delay thresholds with these prefixes. They apply to all links.
//
// An empty option is not written. Then the program uses its default value.
// Exceptions, because the program does not apply these values to a link:
//   - "adjust" of a direction is true if it is not set.
//   - "ping_interface" and "ping_source_addr" of the global section apply
//     to each link that does not set them.

'use strict';

const prefixes = { dl_: 'download', ul_: 'upload' };

const true_words = { '1': true, 'true': true, 'yes': true, 'on': true };
const false_words = { '0': true, 'false': true, 'no': true, 'off': true };

// Converts the UCI value v to the type t. Returns [value, error].
function typed(t, v) {
	if (t == 'list')
		return [ type(v) == 'array' ? v : [ v ], null ];
	if (type(v) == 'array')
		return [ null, 'must be an option, not a list' ];
	switch (t) {
	case 'string':
		return [ v, null ];
	case 'bool':
		if (true_words[v])
			return [ true, null ];
		if (false_words[v])
			return [ false, null ];
		return [ null, sprintf('"%s" is not a boolean (use 0 or 1)', v) ];
	case 'int':
		if (match(v, /^-?[0-9]+$/))
			return [ int(v), null ];
		return [ null, sprintf('"%s" is not an integer', v) ];
	case 'float':
		if (match(v, /^-?[0-9]+(\.[0-9]+)?$/))
			return [ +v, null ];
		return [ null, sprintf('"%s" is not a number', v) ];
	}
	return [ null, sprintf('unknown type "%s" in the schema', t) ];
}

// Returns true if the UCI value v is empty.
function empty(v) {
	return v == null || v == '' || (type(v) == 'array' && length(v) == 0);
}

// Returns the user options of the UCI section s, in sorted order.
function options(s) {
	return filter(sort(keys(s)), (k) => substr(k, 0, 1) != '.');
}

// Returns true if the "enabled" option of the section s is not off.
// Adds an error to errors if the value is not a boolean.
function enabled(s, where, errors) {
	if (empty(s.enabled))
		return true;
	let r = typed('bool', s.enabled);
	if (r[1] != null) {
		push(errors, sprintf('%s.enabled: %s', where, r[1]));
		return false;
	}
	return r[0];
}

// Sets out[key] (or out[dir][key] for a direction key) from the option k of
// the section s. keys is the schema of the plain keys, dir_keys the schema
// of the direction keys.
function set_option(out, s, k, keys, dir_keys, where, errors) {
	if (k == 'enabled' || empty(s[k]))
		return;
	let target = out, key = k, t = keys[k];
	let p = substr(k, 0, 3);
	if (t == null && prefixes[p] && dir_keys[substr(k, 3)]) {
		key = substr(k, 3);
		t = dir_keys[key];
		out[prefixes[p]] ??= {};
		target = out[prefixes[p]];
	}
	if (t == null) {
		push(errors, sprintf('%s: unknown option "%s"', where, k));
		return;
	}
	let r = typed(t, s[k]);
	if (r[1] != null)
		push(errors, sprintf('%s.%s: %s', where, k, r[1]));
	else
		target[key] = r[0];
}

// Converts the UCI sections (the result of cursor.get_all) with the schema.
// fixed has the keys that the package sets (for example status_file). They
// replace the keys from UCI. Returns an object:
//   config:   the configuration of cake-autorate-go, or null
//   disabled: true if the service or all links are disabled
//   errors:   a list of error messages
export function convert(sections, schema, fixed) {
	let errors = [], out = {}, links = [], names = {};
	let globals = 0, disabled = false;

	let list = sort(values(sections ?? {}), (a, b) => a['.index'] - b['.index']);
	for (let s in list) {
		let where = sprintf('%s %s', s['.type'], s['.name']);
		switch (s['.type']) {
		case 'global':
			if (++globals > 1) {
				push(errors, sprintf('%s: only one global section is permitted', where));
				break;
			}
			if (!enabled(s, where, errors))
				disabled = true;
			for (let k in options(s))
				set_option(out, s, k, schema.global, schema.global_direction, where, errors);
			break;
		case 'link':
			if (!enabled(s, where, errors))
				break;
			let link = {};
			for (let k in options(s))
				set_option(link, s, k, schema.link, schema.direction, where, errors);
			if (link.name == null) {
				if (s['.anonymous'])
					push(errors, sprintf('%s: set the option "name"', where));
				link.name = s['.name'];
			}
			if (names[link.name])
				push(errors, sprintf('%s: the link name "%s" is used more than one time', where, link.name));
			names[link.name] = true;
			push(links, link);
			break;
		default:
			push(errors, sprintf('%s: unknown section type', where));
		}
	}

	if (length(errors) > 0)
		return { config: null, disabled: false, errors };
	if (disabled || length(links) == 0)
		return { config: null, disabled: true, errors };

	for (let link in links) {
		for (let d in values(prefixes)) {
			link[d] ??= {};
			link[d].adjust ??= true;
		}
		for (let k in [ 'ping_interface', 'ping_source_addr' ])
			if (out[k] != null)
				link[k] ??= out[k];
	}

	for (let k, v in fixed ?? {})
		out[k] = v;
	out.links = links;
	return { config: out, disabled: false, errors };
};
