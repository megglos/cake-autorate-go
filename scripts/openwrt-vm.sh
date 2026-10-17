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
#   scripts/openwrt-vm.sh reset          stop and remove the disk and the
#                                        image (the next start downloads it)
#
# Network:
#   eth0 (lan, 192.168.1.1): QEMU user network. The host connects through
#        port forwards: SSH localhost:2222, LuCI http://localhost:8080
#   eth1 (wan, DHCP): QEMU user network with internet access. QEMU sends
#        ICMP echo with host ping sockets (net.ipv4.ping_group_range).
#        ICMP timestamps do not go through.
#        With VM_WAN_TAP=TAP, eth1 uses the tap device TAP of the host. The
#        host routes and NATs it: then ICMP timestamps go through too. The
#        VM gets a static address: the address of TAP plus 1. Set up TAP
#        before the start (as root, once after each boot of the host):
#          ip tuntap add dev TAP mode tap user USER
#          ip addr add 10.77.0.1/24 dev TAP && ip link set TAP up
#          sysctl -w net.ipv4.ip_forward=1
#          nft add table ip cavnat
#          nft add chain ip cavnat post '{ type nat hook postrouting priority 100; }'
#          nft add rule ip cavnat post ip saddr 10.77.0.0/24 masquerade
#        The static address stays on the disk. For the user network again,
#        set network.wan.proto=dhcp in the VM, or use "reset".
#   eth2 (lan, only with VM_LAN_TAP=TAP): a port of the LAN bridge on the
#        tap device TAP of the host. A client on TAP (for example a
#        network namespace from scripts/openwrt-vm-client.sh) uses the VM
#        as its router, like a PC behind a router.
#
# VM_CPUS sets the number of CPUs of the VM (default 2).
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
WAN_TAP="${VM_WAN_TAP:-}"
LAN_TAP="${VM_LAN_TAP:-}"
CPUS="${VM_CPUS:-2}"

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
		# Extract to a temporary file, thus a stopped extraction does not
		# leave an incomplete image.
		gunzip -c "$DIR/$IMAGE.gz" >"$DIR/$IMAGE.tmp" || [ $? -eq 2 ]
		mv "$DIR/$IMAGE.tmp" "$DIR/$IMAGE"
		rm -f "$DIR/$IMAGE.gz"
	fi
	if [ ! -f "$DISK" ]; then
		cp "$DIR/$IMAGE" "$DISK"
	fi
	[ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -f "$KEY"
}

# wan_netdev: prints the QEMU netdev of the WAN.
wan_netdev() {
	if [ -z "$WAN_TAP" ]; then
		echo "user,id=wan"
		return
	fi
	if ! ip -4 -o addr show dev "$WAN_TAP" 2>/dev/null | grep -q inet; then
		echo "error: tap device $WAN_TAP has no IPv4 address (see the setup in $0)" >&2
		exit 1
	fi
	echo "tap,id=wan,ifname=$WAN_TAP,script=no,downscript=no"
}

# wan_static: sets a static WAN in the VM, from the address of the tap
# device: the host is the gateway, the VM has the next address.
wan_static() {
	local cidr ip prefix last
	cidr=$(ip -4 -o addr show dev "$WAN_TAP" | awk '{ print $4; exit }')
	ip=${cidr%/*}
	prefix=${cidr#*/}
	last=${ip##*.}
	ssh_vm "uci set network.wan.proto=static &&
		uci set network.wan.ipaddr=${ip%.*}.$((last + 1)) &&
		uci set network.wan.netmask=$(python3 -c "import ipaddress; print(ipaddress.ip_network('0.0.0.0/$prefix').netmask)") &&
		uci set network.wan.gateway=$ip &&
		{ uci -q delete network.wan.dns || true; } &&
		uci add_list network.wan.dns=1.1.1.1 &&
		uci add_list network.wan.dns=9.9.9.9 &&
		uci set network.wan6.disabled=1 &&
		uci commit network && /etc/init.d/network reload"
}

# lan_port: adds eth2 (the tap device of the client) to the LAN bridge.
lan_port() {
	ssh_vm "uci -q get network.@device[0].ports | grep -qw eth2 ||
		{ uci add_list network.@device[0].ports=eth2 && uci commit network && /etc/init.d/network reload; }"
}

cmd_start() {
	if running; then
		echo "VM runs already"
		return
	fi
	local wan lan=() lan_ipv6=""
	wan=$(wan_netdev)
	if [ -n "$LAN_TAP" ]; then
		if ! ip link show dev "$LAN_TAP" >/dev/null 2>&1; then
			echo "error: tap device $LAN_TAP does not exist (scripts/openwrt-vm-client.sh up)" >&2
			exit 1
		fi
		lan=(-netdev "tap,id=client,ifname=$LAN_TAP,script=no,downscript=no" -device virtio-net-pci,netdev=client)
		# The user network on eth0 sends IPv6 router advertisements. A
		# client in the LAN bridge then sends its IPv6 traffic through
		# QEMU, not through the WAN of the VM.
		lan_ipv6=",ipv6=off"
	fi
	prepare_disk
	rm -f "$SERIAL" "$MONITOR"
	qemu-system-x86_64 -enable-kvm -cpu host -smp "$CPUS" -m 512 \
		-drive "file=$DISK,format=raw,if=virtio" \
		-netdev "user,id=lan,net=192.168.1.0/24,host=192.168.1.2,dhcpstart=192.168.1.100,hostfwd=tcp:127.0.0.1:$SSH_PORT-192.168.1.1:22,hostfwd=tcp:127.0.0.1:$HTTP_PORT-192.168.1.1:80$lan_ipv6" \
		-device virtio-net-pci,netdev=lan \
		-netdev "$wan" -device virtio-net-pci,netdev=wan \
		"${lan[@]}" \
		-serial "unix:$SERIAL,server,nowait" \
		-monitor "unix:$MONITOR,server,nowait" \
		-display none -daemonize -pidfile "$PIDFILE"
	echo "VM started, waiting for the console"
	local pub
	pub=$(cat "$KEY.pub")
	serial_run "mkdir -p /etc/dropbear && echo '$pub' > /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys"
	for _ in $(seq 60); do
		if ssh_vm true 2>/dev/null; then
			[ -z "$WAN_TAP" ] || wan_static
			[ -z "$LAN_TAP" ] || lan_port
			echo "VM is ready: ssh -p $SSH_PORT root@localhost (key $KEY), LuCI http://localhost:$HTTP_PORT"
			return
		fi
		sleep 2
	done
	echo "error: no SSH connection to the VM" >&2
	exit 1
}

cmd_install() {
	if [ $# -eq 0 ]; then
		echo "usage: $0 install APK..." >&2
		exit 1
	fi
	# Use generated names in the VM: a file name can have characters that
	# the shell of the VM interprets.
	local remote="" i=0
	for f in "$@"; do
		i=$((i + 1))
		ssh_vm "cat > /tmp/cake-autorate-install-$i.apk" <"$f"
		remote="$remote /tmp/cake-autorate-install-$i.apk"
	done
	ssh_vm "apk update >/dev/null && apk add --allow-untrusted$remote"
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
	rm -f "$DISK" "$DIR/$IMAGE" "$DIR/$IMAGE.tmp" "$DIR/$IMAGE.gz"
	;;
*)
	sed -n '4,15p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
