#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Runs OpenWrt x86-64 in QEMU/KVM, to test the package without a router.
#
# Usage:
#   scripts/openwrt-vm.sh start          download the image (first time), boot,
#                                        enable SSH with a key
#   scripts/openwrt-vm.sh ssh [CMD...]   run a command (or a shell) in the VM
#   scripts/openwrt-vm.sh install APK... copy the packages to the VM and install
#                                        them (apk gets the dependencies)
#   scripts/openwrt-vm.sh stop           power off the VM
#   scripts/openwrt-vm.sh reset          stop and remove the disk (the next
#                                        start uses a new image)
#
# Network:
#   eth0 (lan, 192.168.1.1): QEMU user network. The host connects through
#        port forwards: SSH localhost:2222, LuCI http://localhost:8080
#   eth1 (wan, DHCP): QEMU user network with internet access. QEMU sends
#        ICMP echo with host ping sockets (net.ipv4.ping_group_range).
#        ICMP timestamps do not go through.
#
# Requirements: qemu-system-x86_64, qemu-img, /dev/kvm, python3, curl.
# The state (image, disk, SSH key, serial socket) is in
# ${XDG_CACHE_HOME:-~/.cache}/cake-autorate-vm.

set -euo pipefail

VERSION="${OPENWRT_VERSION:-25.12.5}"
IMAGE="openwrt-$VERSION-x86-64-generic-ext4-combined.img"
URL="https://downloads.openwrt.org/releases/$VERSION/targets/x86/64"
DIR="${XDG_CACHE_HOME:-$HOME/.cache}/cake-autorate-vm"
SSH_PORT="${VM_SSH_PORT:-2222}"
HTTP_PORT="${VM_HTTP_PORT:-8080}"
KEY="$DIR/id_ed25519"
SERIAL="$DIR/serial.sock"
MONITOR="$DIR/monitor.sock"
PIDFILE="$DIR/qemu.pid"
DISK="$DIR/disk-$VERSION.img"

ssh_vm() {
	ssh -q -i "$KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 \
		-o BatchMode=yes root@localhost "$@"
}

running() {
	[ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

# serial_run CMD: types CMD on the serial console and waits for the prompt.
serial_run() {
	python3 - "$SERIAL" "$1" <<'EOF'
import socket, sys, time
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
s.settimeout(1)
def wait_for(text, timeout):
    buf, end = b"", time.time() + timeout
    while time.time() < end:
        try:
            buf += s.recv(4096)
        except socket.timeout:
            s.sendall(b"\n")
        if text in buf:
            return
    sys.exit("timeout: no %r on the serial console" % text)
# The console asks to press Enter, then shows the root prompt.
wait_for(b"root@", 120)
s.sendall(sys.argv[2].encode() + b"\n")
time.sleep(1)
EOF
}

prepare_disk() {
	mkdir -p "$DIR"
	if [ ! -f "$DIR/$IMAGE" ]; then
		curl -fsSL -o "$DIR/$IMAGE.gz" "$URL/$IMAGE.gz"
		curl -fsSL -o "$DIR/sha256sums" "$URL/sha256sums"
		(cd "$DIR" && grep " \*$IMAGE.gz\$" sha256sums | sha256sum -c -)
		# gunzip exits with 2 on the trailing data of the image (a warning).
		gunzip -c "$DIR/$IMAGE.gz" > "$DIR/$IMAGE" || [ $? -eq 2 ]
		rm -f "$DIR/$IMAGE.gz"
	fi
	if [ ! -f "$DISK" ]; then
		cp "$DIR/$IMAGE" "$DISK"
	fi
	[ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -f "$KEY"
}

cmd_start() {
	if running; then
		echo "VM runs already"
		return
	fi
	prepare_disk
	rm -f "$SERIAL" "$MONITOR"
	qemu-system-x86_64 -enable-kvm -cpu host -smp 2 -m 512 \
		-drive "file=$DISK,format=raw,if=virtio" \
		-netdev "user,id=lan,net=192.168.1.0/24,host=192.168.1.2,dhcpstart=192.168.1.100,hostfwd=tcp:127.0.0.1:$SSH_PORT-192.168.1.1:22,hostfwd=tcp:127.0.0.1:$HTTP_PORT-192.168.1.1:80" \
		-device virtio-net-pci,netdev=lan \
		-netdev user,id=wan -device virtio-net-pci,netdev=wan \
		-serial "unix:$SERIAL,server,nowait" \
		-monitor "unix:$MONITOR,server,nowait" \
		-display none -daemonize -pidfile "$PIDFILE"
	echo "VM started, waiting for the console"
	local pub
	pub=$(cat "$KEY.pub")
	serial_run "mkdir -p /etc/dropbear && echo '$pub' > /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys"
	for _ in $(seq 60); do
		if ssh_vm true 2>/dev/null; then
			echo "VM is ready: ssh -p $SSH_PORT root@localhost (key $KEY), LuCI http://localhost:$HTTP_PORT"
			return
		fi
		sleep 2
	done
	echo "error: no SSH connection to the VM" >&2
	exit 1
}

cmd_install() {
	[ $# -gt 0 ] || { echo "usage: $0 install APK..." >&2; exit 1; }
	local remote=()
	for f in "$@"; do
		ssh_vm "cat > /tmp/$(basename "$f")" < "$f"
		remote+=("/tmp/$(basename "$f")")
	done
	ssh_vm "apk update >/dev/null && apk add --allow-untrusted ${remote[*]}"
}

cmd_stop() {
	if running; then
		ssh_vm poweroff 2>/dev/null || true
		for _ in $(seq 30); do
			running || break
			sleep 1
		done
		running && kill "$(cat "$PIDFILE")"
	fi
	rm -f "$PIDFILE"
}

case "${1:-}" in
start) cmd_start ;;
ssh)
	shift
	ssh_vm "$@"
	;;
install)
	shift
	cmd_install "$@"
	;;
stop) cmd_stop ;;
reset)
	cmd_stop
	rm -f "$DISK"
	;;
*)
	sed -n '4,15p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
