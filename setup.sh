#!/bin/sh
#
# cake-autorate-go setup script for OpenWrt
#
# Installs the cake-autorate-go binary and procd service.
# Usage: ./setup.sh [path-to-binary]
#
# If no binary path is given, looks for cake-autorate-go in the current directory.

set -e

INSTALL_DIR="/usr/sbin"
BINARY_NAME="cake-autorate-go"
SERVICE_NAME="cake-autorate-go"
CONFIG_DIR="/etc/cake-autorate"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

# --- helpers ---

die() {
    echo "ERROR: $1" >&2
    exit 1
}

info() {
    echo ">>> $1"
}

# --- pre-flight checks ---

[ "$(id -u)" -eq 0 ] || die "This script must be run as root."

# Verify we're on OpenWrt
[ -f /etc/openwrt_release ] || die "This script is intended for OpenWrt systems."

# Check if the service is currently running
if "${INIT_SCRIPT}" running >/dev/null 2>&1; then
    die "${SERVICE_NAME} is currently running. Stop it first: ${INIT_SCRIPT} stop"
fi

# --- locate binary ---

BINARY_SRC="${1:-$(pwd)/${BINARY_NAME}}"

if [ ! -f "${BINARY_SRC}" ]; then
    printf "ERROR: Binary not found: %s\nUsage: %s [path-to-binary]\n" "${BINARY_SRC}" "$0" >&2
    exit 1
fi
[ -x "${BINARY_SRC}" ] || chmod +x "${BINARY_SRC}"

# --- install binary ---

info "Installing binary to ${INSTALL_DIR}/${BINARY_NAME}"
cp "${BINARY_SRC}" "${INSTALL_DIR}/${BINARY_NAME}"
chmod 755 "${INSTALL_DIR}/${BINARY_NAME}"

# --- install default config ---

if [ -f "${CONFIG_FILE}" ]; then
    info "Config already exists at ${CONFIG_FILE}, skipping"
else
    info "Generating default config at ${CONFIG_FILE}"
    mkdir -p "${CONFIG_DIR}"
    "${INSTALL_DIR}/${BINARY_NAME}" --defaults > "${CONFIG_FILE}"
    info "IMPORTANT: Edit ${CONFIG_FILE} to match your network setup before starting the service"
fi

# --- install procd init script ---

info "Installing init script to ${INIT_SCRIPT}"
cat > "${INIT_SCRIPT}" <<'INITEOF'
#!/bin/sh /etc/rc.common

START=97
STOP=4
USE_PROCD=1

start_service() {
    procd_open_instance
    procd_set_param command /usr/sbin/cake-autorate-go
    procd_set_param respawn
    procd_set_param stderr 1
    procd_close_instance
}
INITEOF
chmod 755 "${INIT_SCRIPT}"

# --- done ---

echo ""
info "Installation complete!"
echo ""
echo "Next steps:"
echo "  1. Edit your config:        vi ${CONFIG_FILE}"
echo "  2. Enable on boot:          service ${SERVICE_NAME} enable"
echo "  3. Start the service:       service ${SERVICE_NAME} start"
echo "  4. Check status:            service ${SERVICE_NAME} status"
echo "  5. View logs:               logread -e ${BINARY_NAME}"
echo ""
echo "To uninstall:"
echo "  service ${SERVICE_NAME} stop"
echo "  service ${SERVICE_NAME} disable"
echo "  rm ${INIT_SCRIPT} ${INSTALL_DIR}/${BINARY_NAME}"
echo ""
