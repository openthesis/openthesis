#!/usr/bin/env bash
#
# Usage: ./deploy/provision.sh <USER>@<IP>
#
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <USER>@<IP>"
    echo "Example: $0 root@65.21.x.x"
    echo "Example: $0 ubuntu@206.223.236.99"
    exit 1
fi

TARGET="$1"
IP="$(echo "$TARGET" | cut -d@ -f2)"
SSH_USER="$(echo "$TARGET" | cut -d@ -f1)"

echo "==> Provisioning $TARGET"

ssh -o StrictHostKeyChecking=accept-new "$TARGET" bash -s "$IP" "$SSH_USER" <<'REMOTE_SCRIPT'
set -euo pipefail
SERVER_IP="$1"
SSH_USER="$2"

export DEBIAN_FRONTEND=noninteractive

# Use sudo if not root.
if [[ "$(id -u)" -ne 0 ]]; then
    SUDO="sudo"
else
    SUDO=""
fi

# Fix hostname resolution (suppresses "sudo: unable to resolve host" noise).
if ! grep -q "$(hostname)" /etc/hosts 2>/dev/null; then
    echo "127.0.1.1 $(hostname)" | $SUDO tee -a /etc/hosts > /dev/null
fi

# Wait for apt lock (cloud-init / unattended-upgrades on first boot)

echo "--- Clearing apt locks ---"
$SUDO systemctl stop unattended-upgrades apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
$SUDO kill -9 $(pgrep -f "apt-get|apt |dpkg|unattended-upgrade" 2>/dev/null) 2>/dev/null || true
sleep 2
$SUDO rm -f /var/lib/apt/lists/lock /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
$SUDO dpkg --configure -a 2>/dev/null || true

# Verify KVM

echo "--- Verifying /dev/kvm ---"
if [[ ! -e /dev/kvm ]]; then
    echo "!!! /dev/kvm not found."
    echo "    OpenThesis requires bare-metal with KVM for acceptable TCG performance."
    echo "    Cloud VMs without nested virtualization will not work."
    exit 1
fi
echo "    /dev/kvm found."

# System update

echo "--- Updating system ---"
$SUDO apt-get update
$SUDO apt-get upgrade -y

echo "--- Installing base packages ---"
$SUDO apt-get install -y -qq \
    curl \
    ca-certificates \
    gnupg \
    nftables \
    fail2ban \
    unattended-upgrades \
    apt-listchanges \
    jq \
    rsync

echo "--- Installing QEMU/kernel build dependencies ---"
$SUDO apt-get install -y -qq \
    build-essential \
    git \
    ninja-build \
    python3 \
    python3-venv \
    pkg-config \
    libglib2.0-dev \
    libpixman-1-dev \
    libslirp-dev \
    flex \
    bison \
    bc \
    libelf-dev \
    libssl-dev \
    cpio \
    e2fsprogs \
    xfsprogs \
    debootstrap \
    libclang-dev \
    clang \
    libseccomp-dev

# Create openthesis user

echo "--- Creating openthesis user ---"
if ! id openthesis &>/dev/null; then
    $SUDO useradd --system --create-home --shell /bin/bash openthesis
fi

# Grant KVM access to both the openthesis user and the connecting user.
if getent group kvm > /dev/null; then
    $SUDO usermod -aG kvm openthesis
    $SUDO usermod -aG kvm "$SSH_USER" 2>/dev/null || true
fi
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0660"' | $SUDO tee /etc/udev/rules.d/99-kvm.rules > /dev/null
$SUDO udevadm trigger /dev/kvm 2>/dev/null || true

# XFS with reflink for instant snapshot copies (mounted at ~/.openthesis)

echo "--- Setting up XFS reflink storage ---"
OT_DIR="$HOME/.openthesis"
mkdir -p "$OT_DIR"

if mount | grep -q "$OT_DIR.*xfs"; then
    echo "    $OT_DIR already mounted as XFS"
else
    XFS_DEV=""
    for dev in /dev/nvme1n1 /dev/nvme2n1 /dev/sdb; do
        if [[ -b "$dev" ]] && ! mount | grep -q "$dev" && ! grep -q "$(basename "$dev")" /proc/mdstat 2>/dev/null; then
            XFS_DEV="$dev"
            break
        fi
    done

    if [[ -n "$XFS_DEV" ]]; then
        echo "    Formatting $XFS_DEV as XFS with reflink"
        $SUDO mkfs.xfs -f -m reflink=1 "$XFS_DEV"
        $SUDO mount "$XFS_DEV" "$OT_DIR"
        if ! grep -q "$OT_DIR" /etc/fstab; then
            echo "$XFS_DEV $OT_DIR xfs defaults 0 0" | $SUDO tee -a /etc/fstab > /dev/null
        fi
    else
        XFS_IMG="$HOME/.openthesis.img"
        if [[ ! -f "$XFS_IMG" ]]; then
            echo "    Creating loopback XFS image at $XFS_IMG"
            truncate -s 50G "$XFS_IMG"
            $SUDO mkfs.xfs -m reflink=1 "$XFS_IMG"
        fi
        $SUDO mount -o loop "$XFS_IMG" "$OT_DIR"
        if ! grep -q "$OT_DIR" /etc/fstab; then
            echo "$XFS_IMG $OT_DIR xfs loop 0 0" | $SUDO tee -a /etc/fstab > /dev/null
        fi
    fi
    echo "    Mounted $OT_DIR as XFS with reflink"
fi

if xfs_info "$OT_DIR" 2>/dev/null | grep -q "reflink=1"; then
    echo "    reflink: enabled (instant CoW snapshots)"
else
    echo "    WARNING: reflink not enabled - snapshot copies will be slow"
fi

# Directory structure

echo "--- Creating directories ---"
mkdir -p "$OT_DIR"/{bin,build/src,snapshots,vms,rootfs,kernel,runs,output}

# Firewall (nftables)

echo "--- Configuring firewall ---"
$SUDO tee /etc/nftables.conf > /dev/null <<'NFT'
#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;

        # Loopback
        iif lo accept

        # Established/related
        ct state established,related accept

        # ICMP
        ip protocol icmp accept
        ip6 nexthdr icmpv6 accept

        # SSH
        tcp dport 22 accept

        # OpenThesis API
        tcp dport 8080 accept
    }

    chain forward {
        type filter hook forward priority 0; policy drop;

        # Allow QEMU guest traffic
        ct state established,related accept
    }

    chain output {
        type filter hook output priority 0; policy accept;
    }
}
NFT

$SUDO systemctl enable --now nftables
$SUDO nft -f /etc/nftables.conf

echo "--- Loading vhost_vsock ---"
$SUDO modprobe vhost_vsock 2>/dev/null || true
grep -qx "vhost_vsock" /etc/modules 2>/dev/null || echo "vhost_vsock" | $SUDO tee -a /etc/modules > /dev/null

# Harden SSH

echo "--- Hardening SSH ---"
HAS_SSH_KEY=""
if [[ -s /root/.ssh/authorized_keys ]] || [[ -s "/home/$SSH_USER/.ssh/authorized_keys" ]]; then
    HAS_SSH_KEY="yes"
fi

if [[ -n "$HAS_SSH_KEY" ]]; then
    $SUDO sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
    $SUDO sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
    echo "    Password auth disabled (SSH key found)"
else
    echo "    WARNING: No SSH key found, skipping password auth disable."
fi
$SUDO sed -i 's/^#\?X11Forwarding.*/X11Forwarding no/' /etc/ssh/sshd_config
$SUDO systemctl reload ssh 2>/dev/null || $SUDO systemctl reload sshd

# fail2ban

echo "--- Configuring fail2ban ---"
$SUDO tee /etc/fail2ban/jail.local > /dev/null <<'F2B'
[sshd]
enabled = true
port = ssh
maxretry = 5
bantime = 3600
findtime = 600
F2B

$SUDO systemctl enable --now fail2ban

# Unattended upgrades

echo "--- Configuring CPU isolation for deterministic VM execution ---"
NCPUS=$(nproc)
if [[ "$NCPUS" -gt 2 ]]; then
    ISOLATED_RANGE="2-$((NCPUS - 1))"
    GRUB_FILE=/etc/default/grub
    ISOLATION_PARAM="isolcpus=${ISOLATED_RANGE} nohz_full=${ISOLATED_RANGE} rcu_nocbs=${ISOLATED_RANGE} irqaffinity=0-1 nosoftlockup"

    if ! grep -q "isolcpus" "$GRUB_FILE" 2>/dev/null; then
        $SUDO sed -i "s/GRUB_CMDLINE_LINUX_DEFAULT=\"\(.*\)\"/GRUB_CMDLINE_LINUX_DEFAULT=\"\1 ${ISOLATION_PARAM}\"/" "$GRUB_FILE"
        $SUDO update-grub 2>/dev/null || $SUDO grub-mkconfig -o /boot/grub/grub.cfg 2>/dev/null || true
        echo "    CPU isolation configured: cores ${ISOLATED_RANGE} isolated"
        echo "    NOTE: reboot required for isolcpus to take effect"
    else
        echo "    CPU isolation already configured in GRUB"
    fi

    for irq_dir in /proc/irq/*/smp_affinity_list; do
        echo "0-1" | $SUDO tee "$irq_dir" > /dev/null 2>&1 || true
    done
    echo "    IRQ affinity pinned to CPU 0-1"

    echo "kernel.perf_event_paranoid=0" | $SUDO tee -a /etc/sysctl.d/99-openthesis-dst.conf > /dev/null
    $SUDO sysctl -w kernel.perf_event_paranoid=0 > /dev/null
    echo "    perf_event_paranoid=0 (PMC instruction counting enabled)"
else
    echo "    WARNING: Only ${NCPUS} CPUs - skipping isolation (need >2 cores)"
fi

echo "--- Enabling unattended security upgrades ---"
$SUDO tee /etc/apt/apt.conf.d/50unattended-upgrades > /dev/null <<'UU'
Unattended-Upgrade::Allowed-Origins {
    "${distro_id}:${distro_codename}-security";
};
Unattended-Upgrade::AutoFixInterruptedDpkg "true";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "false";
UU

$SUDO tee /etc/apt/apt.conf.d/20auto-upgrades > /dev/null <<'AU'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
AU

echo ""
echo "=== Provision complete ==="
echo ""
echo "KVM:   $(ls -la /dev/kvm)"
echo "State: $HOME/.openthesis"

REMOTE_SCRIPT
rc=$?
[ $rc -eq 0 ] || { echo "!!! Provision failed (exit $rc)"; exit $rc; }
echo "==> Provision finished."
