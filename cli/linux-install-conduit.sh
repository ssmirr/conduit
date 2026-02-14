#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="conduit"
SERVICE_USER="conduit"
INSTALL_DIR="/opt/conduit"
BIN_NAME="conduit"
SYSTEMD_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
GITHUB_REPO="ssmirr/conduit"
GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

echo "==> Installing Conduit as a secure systemd service"

# --------------------------------------------------
# 1. Root check
# --------------------------------------------------
if [[ "$EUID" -ne 0 ]]; then
  echo "❌ Please run this script as root"
  exit 1
fi

# --------------------------------------------------
# 2. Detect architecture
# --------------------------------------------------
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64)
    ASSET_SUFFIX="linux-amd64"
    ;;
  aarch64|arm64)
    ASSET_SUFFIX="linux-arm64"
    ;;
  armv7l)
    ASSET_SUFFIX="linux-armv7"
    ;;
  *)
    echo "❌ Unsupported architecture: ${ARCH}"
    exit 1
    ;;
esac

echo "==> Detected architecture: ${ARCH} (${ASSET_SUFFIX})"

# --------------------------------------------------
# 3. Get latest release download URL
# --------------------------------------------------
echo "==> Fetching latest release info from GitHub"

DOWNLOAD_URL="$(curl -fsSL "${GITHUB_API}" \
  | grep browser_download_url \
  | grep "${ASSET_SUFFIX}" \
  | cut -d '"' -f 4)"

if [[ -z "${DOWNLOAD_URL}" ]]; then
  echo "❌ Could not find a release asset for ${ASSET_SUFFIX}"
  exit 1
fi

echo "==> Downloading: ${DOWNLOAD_URL}"

# --------------------------------------------------
# 4. Create system user (if not exists)
# --------------------------------------------------
if ! id "${SERVICE_USER}" &>/dev/null; then
  echo "==> Creating system user: ${SERVICE_USER}"
  useradd \
    --system \
    --no-create-home \
    --shell /usr/sbin/nologin \
    "${SERVICE_USER}"
else
  echo "==> User ${SERVICE_USER} already exists"
fi

# --------------------------------------------------
# 5. Download & install binary
# --------------------------------------------------
echo "==> Installing binary to ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"

curl -fL "${DOWNLOAD_URL}" -o "${INSTALL_DIR}/${BIN_NAME}"
chmod +x "${INSTALL_DIR}/${BIN_NAME}"
chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"

# --------------------------------------------------
# 6. Create systemd service file
# --------------------------------------------------
echo "==> Creating systemd service: ${SYSTEMD_FILE}"

cat > "${SYSTEMD_FILE}" <<EOF
[Unit]
Description=Conduit Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}

ExecStart=${INSTALL_DIR}/${BIN_NAME} start -b -1 -m 200
ExecStop=/bin/kill -TERM \$MAINPID

Restart=always
RestartSec=5
TimeoutStopSec=30
KillMode=process

# ---------- Security Hardening ----------
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true

ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${INSTALL_DIR}

ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true

RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true

LockPersonality=true
MemoryDenyWriteExecute=true

SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF

chmod 644 "${SYSTEMD_FILE}"

# --------------------------------------------------
# 7. Enable & start service
# --------------------------------------------------
echo "==> Reloading systemd"
systemctl daemon-reload

echo "==> Enabling service"
systemctl enable "${SERVICE_NAME}"

echo "==> Starting service"
systemctl restart "${SERVICE_NAME}"

# --------------------------------------------------
# 8. Final status
# --------------------------------------------------
echo
echo "✅ Installation complete!"
systemctl --no-pager status "${SERVICE_NAME}"
