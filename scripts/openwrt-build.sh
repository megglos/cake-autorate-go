#!/bin/bash
# SPDX-License-Identifier: GPL-2.0
#
# Builds the OpenWrt packages of this repository in the OpenWrt SDK
# (Docker image openwrt/sdk). It does the same steps as the CI workflow.
#
# Usage: scripts/openwrt-build.sh [ARCH] [PACKAGES]
#   ARCH      SDK image tag, default aarch64_cortex-a53-openwrt-25.12
#             (GL-MT6000). For x86 use x86_64-openwrt-25.12.
#   PACKAGES  packages to build, default "cake-autorate-go
#             luci-app-cake-autorate-go"
#
# The packages go to bin/openwrt/ARCH/. The script keeps a container with
# the name cake-autorate-sdk-ARCH. Thus the second build does not download
# the SDK and the feeds again. Remove the container to start again:
#   docker rm -f cake-autorate-sdk-ARCH

set -euo pipefail

ARCH="${1:-aarch64_cortex-a53-openwrt-25.12}"
PACKAGES="${2:-cake-autorate-go luci-app-cake-autorate-go}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
NAME="cake-autorate-sdk-${ARCH//[^A-Za-z0-9_.-]/_}"
OUT="$REPO/bin/openwrt/$ARCH"

# A container of another checkout builds the source of that checkout.
if docker container inspect "$NAME" >/dev/null 2>&1; then
	src=$(docker container inspect -f '{{range .Mounts}}{{if eq .Destination "/feed"}}{{.Source}}{{end}}{{end}}' "$NAME")
	if [ "$src" != "$REPO" ]; then
		echo "error: container $NAME builds $src, not $REPO. Remove it: docker rm -f $NAME" >&2
		exit 1
	fi
fi

if ! docker container inspect "$NAME" >/dev/null 2>&1; then
	docker run -d --name "$NAME" -v "$REPO:/feed:ro" \
		"openwrt/sdk:$ARCH" sleep infinity >/dev/null
	docker exec "$NAME" bash -c '
		set -e
		[ -f feeds.conf.default ] || bash setup.sh
		sed -e "s,https://git.openwrt.org/feed/,https://github.com/openwrt/," \
			-e "s,https://git.openwrt.org/openwrt/,https://github.com/openwrt/," \
			-e "s,https://git.openwrt.org/project/,https://github.com/openwrt/," \
			feeds.conf.default > feeds.conf
		echo "src-link action /feed/" >> feeds.conf
		./scripts/feeds update -a
		make defconfig'
else
	docker start "$NAME" >/dev/null
	docker exec "$NAME" ./scripts/feeds update action
fi

for pkg in $PACKAGES; do
	docker exec "$NAME" bash -c "
		set -e
		./scripts/feeds install -p action -f $pkg
		make defconfig >/dev/null
		make package/$pkg/clean V=s >/dev/null
		make package/$pkg/compile -j\$(nproc) V=s"
done

mkdir -p "$OUT"
docker exec "$NAME" bash -c 'find bin/packages -path "*/action/*.apk"' | while read -r f; do
	docker cp "$NAME:/builder/$f" "$OUT/"
done
ls -l "$OUT"
