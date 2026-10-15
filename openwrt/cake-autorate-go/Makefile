# SPDX-License-Identifier: GPL-2.0
#
# OpenWrt package of cake-autorate-go. The source is this repository: the
# package does not download it. Build it in the OpenWrt SDK with this
# directory in a feed (see .github/workflows/openwrt.yml).

include $(TOPDIR)/rules.mk

PKG_NAME:=cake-autorate-go
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_LICENSE:=GPL-2.0-only
PKG_LICENSE_FILES:=LICENSE

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

# The root of the repository. CURDIR is the real path, also if the feed
# links to this directory.
REPO_DIR:=$(CURDIR)/../..

# The version that the program shows. CI can set a different value, for
# example the output of "git describe".
CAKE_AUTORATE_VERSION?=$(PKG_VERSION)-r$(PKG_RELEASE)

GO_PKG:=github.com/megglos/cake-autorate-go
GO_PKG_LDFLAGS_X:=main.version=$(CAKE_AUTORATE_VERSION)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/cake-autorate-go
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Adaptive CAKE shaper rate control (Go)
  URL:=https://github.com/megglos/cake-autorate-go
  DEPENDS:=$(GO_ARCH_DEPENDS) +kmod-sched-cake +ucode +ucode-mod-fs +ucode-mod-uci
endef

define Package/cake-autorate-go/description
  cake-autorate-go changes the bandwidth of CAKE shapers to follow the
  capacity of a variable link (for example LTE, 5G or Starlink). It
  measures the delay with pings and the load of the link. It decreases
  the rate when the delay increases, and increases the rate when the
  link is busy and the delay is low.
endef

define Package/cake-autorate-go/conffiles
/etc/config/cake-autorate
endef

define Build/Prepare
	$(INSTALL_DIR) $(PKG_BUILD_DIR)
	$(CP) $(REPO_DIR)/*.go $(REPO_DIR)/go.mod $(REPO_DIR)/go.sum $(REPO_DIR)/LICENSE $(PKG_BUILD_DIR)/
endef

define Package/cake-autorate-go/install
	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/cake-autorate-go $(1)/usr/sbin/
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/cake-autorate.init $(1)/etc/init.d/cake-autorate
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/cake-autorate.config $(1)/etc/config/cake-autorate
	$(INSTALL_DIR) $(1)/usr/share/cake-autorate
	$(INSTALL_DATA) ./files/convert.uc ./files/uci2yaml.uc ./files/schema.json $(1)/usr/share/cake-autorate/
endef

$(eval $(call GoBinPackage,cake-autorate-go))
$(eval $(call BuildPackage,cake-autorate-go))
