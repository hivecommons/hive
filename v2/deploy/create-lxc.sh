#!/usr/bin/env bash
set -euo pipefail

# create-lxc.sh — Create a Proxmox LXC container for Hive v2 (Docker-based).
#
# Run this on the Proxmox host.
# Usage:
#   bash create-lxc.sh [--ctid 111] [--hostname hive-certus] [--password maver1ck]
#
# Or from your Mac:
#   sshpass -p <pw> ssh root@<proxmox-ip> 'bash -s' < create-lxc.sh
#
# After creation, run bootstrap-lxc.sh inside the container:
#   pct exec <CTID> -- bash -c 'curl -fsSL https://raw.githubusercontent.com/kubestellar/hive/v2/v2/deploy/bootstrap-lxc.sh | bash'

CTID="${CTID:-111}"
HOSTNAME="${LXC_HOSTNAME:-hive}"
PASSWORD="${LXC_PASSWORD:-}"
TEMPLATE="local:vztmpl/ubuntu-24.04-standard_24.04-2_amd64.tar.zst"
STORAGE="local-lvm"
DISK_SIZE=16
RAM_MB=4096
SWAP_MB=512
CORES=4

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ctid)     CTID="$2"; shift 2 ;;
    --hostname) HOSTNAME="$2"; shift 2 ;;
    --password) PASSWORD="$2"; shift 2 ;;
    # --no-password creates the container without SSH password auth (key-only after bootstrap)
    --no-password) PASSWORD=""; shift ;;
    --disk)     DISK_SIZE="$2"; shift 2 ;;
    --memory)   RAM_MB="$2"; shift 2 ;;
    --cores)    CORES="$2"; shift 2 ;;
    *)          echo "Unknown option: $1"; exit 1 ;;
  esac
done

# Require an explicit password — no insecure default.
if [[ -z "${PASSWORD}" ]]; then
  echo "ERROR: --password <password> is required (no default to prevent accidental weak credentials)."
  echo "  Supply a strong password: --password \$(openssl rand -hex 16)"
  echo "  After bootstrap, disable SSH password auth and add your public key instead."
  exit 1
fi

# Check if template exists, download if not
if ! pveam list local | grep -q "ubuntu-24.04-standard"; then
  echo "=== Downloading Ubuntu 24.04 template ==="
  pveam update
  pveam download local ubuntu-24.04-standard_24.04-2_amd64.tar.zst
fi

# Check if CTID is in use
if pct status "${CTID}" &>/dev/null; then
  echo "ERROR: CTID ${CTID} already exists. Pick a different ID or destroy first."
  exit 1
fi

echo "=== Creating LXC ${CTID} (${HOSTNAME}) ==="
pct create "${CTID}" "${TEMPLATE}" \
  --hostname "${HOSTNAME}" \
  --storage "${STORAGE}" \
  --rootfs "${STORAGE}:${DISK_SIZE}" \
  --memory "${RAM_MB}" \
  --swap "${SWAP_MB}" \
  --cores "${CORES}" \
  --net0 "name=eth0,bridge=vmbr0,ip=dhcp" \
  --features "nesting=1,keyctl=1" \
  --unprivileged 0 \
  --start 0 \
  --password "${PASSWORD}"

# Docker-in-LXC requires AppArmor unconfined + no cap drop
echo "=== Configuring LXC for Docker compatibility ==="
cat >> "/etc/pve/lxc/${CTID}.conf" <<'EOF'
lxc.apparmor.profile: unconfined
lxc.cap.drop:
EOF

echo "=== Starting LXC ==="
pct start "${CTID}"
sleep 5

echo "=== Enabling SSH with root password auth (temporary — disable after adding SSH key) ==="
pct exec "${CTID}" -- bash -c '
  sed -i "s/^#*PermitRootLogin.*/PermitRootLogin yes/" /etc/ssh/sshd_config
  sed -i "s/^#*PasswordAuthentication.*/PasswordAuthentication yes/" /etc/ssh/sshd_config
  systemctl restart sshd
'

echo "=== LXC IP address ==="
LXC_IP=$(pct exec "${CTID}" -- ip -4 addr show eth0 | grep -oP '(?<=inet )\d+\.\d+\.\d+\.\d+' || echo "unknown")
echo "  IP: ${LXC_IP}"

echo ""
echo "=== LXC ${CTID} (${HOSTNAME}) created ==="
echo ""
echo "SSH:  ssh root@${LXC_IP}  (password: ${PASSWORD})"
echo ""
echo "⚠️  SECURITY: SSH root password auth is enabled for initial bootstrap only."
echo "   After installing your SSH public key, disable password auth:"
echo "   pct exec ${CTID} -- bash -c 'sed -i s/PasswordAuthentication yes/PasswordAuthentication no/ /etc/ssh/sshd_config && systemctl restart sshd'"
echo ""
echo "Next: run the bootstrap script inside the LXC:"
echo "  pct exec ${CTID} -- bash -c 'apt-get update -qq && apt-get install -y -qq curl && curl -fsSL https://raw.githubusercontent.com/kubestellar/hive/v2/v2/deploy/bootstrap-lxc.sh | bash'"
echo ""
echo "Or via SSH:"
echo "  sshpass -p ${PASSWORD} ssh root@${LXC_IP} 'apt-get update -qq && apt-get install -y -qq curl && curl -fsSL https://raw.githubusercontent.com/kubestellar/hive/v2/v2/deploy/bootstrap-lxc.sh | bash'"
