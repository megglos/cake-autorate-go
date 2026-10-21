# SPDX-License-Identifier: GPL-2.0
#
# LuCI app of cake-autorate-go. It does not use luci.mk of the LuCI feed,
# thus the build needs no other feeds. The files have no translations yet.

include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-cake-autorate-go
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_LICENSE:=GPL-2.0-only

include $(INCLUDE_DIR)/package.mk

define Package/luci-app-cake-autorate-go
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=LuCI support for cake-autorate-go
  URL:=https://github.com/megglos/cake-autorate-go
  PKGARCH:=all
  DEPENDS:=+cake-autorate-go
  # Only in the package, because the SDK does not have the LuCI feed. The
  # rpcd plugin uses ucode-mod-socket for the control socket. The
  # calibration and the bufferbloat test pin the link: with conntrack, the
  # pin also moves the existing connections of the load.
  EXTRA_DEPENDS:=luci-base (>=0), ucode-mod-socket (>=0), conntrack (>=0)
endef

define Package/luci-app-cake-autorate-go/description
  Web pages to configure cake-autorate-go: the global settings and the
  links (interfaces, rates, delay thresholds and reflectors). A status
  page shows the state and the rates of each link and can pause the rate
  control. The Overview page shows one row for each link.
endef

define Build/Compile
endef

define Package/luci-app-cake-autorate-go/install
	$(INSTALL_DIR) $(1)/www
	$(CP) ./htdocs/* $(1)/www/
	$(CP) ./root/* $(1)/
endef

define Package/luci-app-cake-autorate-go/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] || {
	rm -f /tmp/luci-indexcache* /tmp/luci-modulecache/*
	/etc/init.d/rpcd reload 2>/dev/null
}
exit 0
endef

# rpcd loads the ACL again, thus the users lose the permissions of the app.
define Package/luci-app-cake-autorate-go/postrm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] || {
	rm -f /tmp/luci-indexcache* /tmp/luci-modulecache/*
	/etc/init.d/rpcd reload 2>/dev/null
}
exit 0
endef

$(eval $(call BuildPackage,luci-app-cake-autorate-go))
