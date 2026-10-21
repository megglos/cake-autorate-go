#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Runs scripts/record-session.sh in the OpenWrt VM of scripts/openwrt-vm.sh.
# The VM makes the load itself (load-gen.sh with curl), and CAKE shapes it
# on the WAN of the VM (eth1 and ifb4eth1). Thus the session needs no sudo
# on the host. Use it with a WAN tap on a real link (VM_WAN_TAP, for
# example a 5G modem), for example to compare cake-autorate-go with the
# original cake-autorate (-U).
#
# Usage:
#   scripts/openwrt-vm-session.sh setup
#       install bash, curl, fping, coreutils-date, coreutils-realpath and
#       procps-ng-pkill (load-gen.sh stops its workers with pkill) in the
#       VM, and copy the session scripts. If UPSTREAM_DIR (default ../cake-autorate) exists,
#       copy the original cake-autorate too.
#   scripts/openwrt-vm-session.sh start NAME OPTIONS...
#       start a session in the background. The OPTIONS go to
#       record-session.sh (-i eth1 and -x are set). The script copies the
#       config files of -c, -A and -U from the host into the VM. The session
#       stops the services cake-autorate and sqm, and starts them again at
#       the end.
#   scripts/openwrt-vm-session.sh status NAME
#       show the last lines of the session log
#   scripts/openwrt-vm-session.sh stop NAME
#       stop the session (record-session.sh removes its CAKE setup)
#
# Only one session can run in the VM at a time (a lock in the VM).
#   scripts/openwrt-vm-session.sh fetch NAME
#       copy the output of the session to recordings/vm-NAME
#
# The environment variables of scripts/openwrt-vm.sh (XDG_CACHE_HOME,
# VM_SSH_PORT, ...) select the VM.
#
# Example (5G, ours against the original, 60 s blocks, 15 GB limit):
#   scripts/openwrt-vm-session.sh setup
#   scripts/openwrt-vm-session.sh start cmp1 -c 5g.yaml \
#       -U fts=upstream-5g-fping-ts.sh -m "dl both" -b 60 -r 2 -s -w 8 -L 15000

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(dirname "$SCRIPT_DIR")
VM="$SCRIPT_DIR/openwrt-vm.sh"
UPSTREAM_DIR="${UPSTREAM_DIR:-$REPO_DIR/../cake-autorate}"
VM_DIR=/root/cav
IFACE=eth1

die() { echo "error: $*" >&2; exit 1; }
vm() { "$VM" ssh "$@"; }

# put copies the host file $1 to the path $2 in the VM.
put() {
	vm "cat > '$2'" < "$1"
}

check_name() {
	[[ ${1:-} =~ ^[A-Za-z0-9_-]+$ ]] || die "NAME must contain only letters, digits, _ and -"
}

cmd_setup() {
	vm "apk update >/dev/null && apk add bash curl fping coreutils-date coreutils-realpath procps-ng-pkill >/dev/null && mkdir -p $VM_DIR/scripts $VM_DIR/conf $VM_DIR/out"
	put "$SCRIPT_DIR/record-session.sh" "$VM_DIR/scripts/record-session.sh"
	put "$SCRIPT_DIR/cake-pc.sh" "$VM_DIR/scripts/cake-pc.sh"
	put "$SCRIPT_DIR/summarize-session.sh" "$VM_DIR/scripts/summarize-session.sh"
	put "$REPO_DIR/load-gen.sh" "$VM_DIR/load-gen.sh"
	vm "chmod +x $VM_DIR/scripts/*.sh $VM_DIR/load-gen.sh"
	if [ -f "$UPSTREAM_DIR/cake-autorate.sh" ]; then
		vm "mkdir -p $VM_DIR/upstream"
		for f in cake-autorate.sh lib.sh defaults.sh; do
			put "$UPSTREAM_DIR/$f" "$VM_DIR/upstream/$f"
		done
		echo "copied the original cake-autorate ($(git -C "$UPSTREAM_DIR" describe --always 2>/dev/null || echo unknown))"
	fi
	echo "setup done: $(vm 'cake-autorate-go -version')"
}

cmd_start() {
	local name=${1:-} args=() opt val dest n=0
	check_name "$name"
	shift
	vm "test ! -e $VM_DIR/out/$name" || die "session $name exists in the VM"
	# Only one session at a time: all sessions use the CAKE setup of eth1.
	# The lock (mkdir is atomic) stays until the services run again.
	vm "mkdir $VM_DIR/lock 2>/dev/null && echo $name > $VM_DIR/lock/name" ||
		die "a session runs in the VM ($(vm "cat $VM_DIR/lock/name 2>/dev/null"))"
	trap 'vm "rm -rf $VM_DIR/lock" || true' EXIT
	# Copy the config files of -c, -A and -U, and change the paths. Each
	# file gets its own number: two files can have the same name.
	while [ $# -gt 0 ]; do
		opt=$1
		case $opt in
			-i | -x | -o) die "$opt is set by this script" ;;
			-c)
				val=${2:?-c needs a file}
				n=$((n + 1))
				dest="$VM_DIR/conf/$name-$n-$(basename "$val")"
				put "$val" "$dest"
				args+=(-c "$dest")
				shift 2 ;;
			-A | -U)
				val=${2:?$opt needs NAME=FILE}
				[[ $val != *,* ]] || die "$opt: a different binary is not supported in the VM"
				n=$((n + 1))
				dest="$VM_DIR/conf/$name-$n-$(basename "${val#*=}")"
				put "${val#*=}" "$dest"
				args+=("$opt" "${val%%=*}=$dest")
				shift 2 ;;
			*)
				args+=("$1")
				shift ;;
		esac
	done
	local q
	q=$(printf '%q ' "${args[@]}")
	# The services must not change the CAKE setup of the session. The
	# session starts them again at the end, also after an error.
	vm "cd $VM_DIR && service cake-autorate stop; service sqm stop; \
		setsid bash -c 'UPSTREAM_DIR=$VM_DIR/upstream scripts/record-session.sh \
		-i $IFACE -x /usr/sbin/cake-autorate-go -o out/$name $q; \
		service sqm start; service cake-autorate start; rm -rf $VM_DIR/lock' \
		> out/$name.log 2>&1 < /dev/null & \
		echo \$! > out/$name.pid" || {
		vm "rm -rf $VM_DIR/lock; service sqm start; service cake-autorate start" || true
		die "the start of the session failed"
	}
	# The session shell removes the lock at its end.
	trap - EXIT
	echo "session $name started. Log: $0 status $name"
}

cmd_status() {
	check_name "${1:-}"
	vm "tail -n 20 $VM_DIR/out/$1.log; kill -0 \$(cat $VM_DIR/out/$1.pid) 2>/dev/null && echo '(running)' || echo '(not running)'"
}

cmd_stop() {
	check_name "${1:-}"
	# record-session.sh is the child of the session shell: its TERM trap
	# removes the CAKE setup, then the shell starts the services again.
	# SIGINT does not work: a background process ignores it.
	vm "kill -TERM \$(pgrep -P \$(cat $VM_DIR/out/$1.pid)) 2>/dev/null || true"
}

cmd_fetch() {
	check_name "${1:-}"
	mkdir -p "$REPO_DIR/recordings"
	vm "cd $VM_DIR/out && tar -cf - $1 $1.log" |
		tar -xf - -C "$REPO_DIR/recordings" --transform "s,^$1,vm-$1,"
	echo "output: $REPO_DIR/recordings/vm-$1"
}

case ${1:-} in
	setup) cmd_setup ;;
	start) shift; cmd_start "$@" ;;
	status) cmd_status "${2:-}" ;;
	stop) cmd_stop "${2:-}" ;;
	fetch) cmd_fetch "${2:-}" ;;
	*) sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 1 ;;
esac
