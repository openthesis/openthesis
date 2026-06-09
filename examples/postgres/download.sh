#!/usr/bin/env bash
#
# download.sh - build sut.ext4 containing PostgreSQL 16 binaries, shared libs,
# and a pre-initialized data directory so initdb doesn't run inside the VM.
#
# The sut.ext4 is mounted read-only at /mnt/sut inside the Firecracker guest.
# pg-server finds binaries at /mnt/sut/bin/ and shared libs at
# /mnt/sut/lib/x86_64-linux-gnu/.  The pre-initialized data dir lives at
# /mnt/sut/pgdata-seed/.  pg-server copies it to /var/lib/pg/data at startup
# (copy-on-write via hard-link safe cp -r), bypassing the slow initdb step.
#
# Strategy (in order of preference):
#   1. Docker: pull postgres:16-bookworm, extract binaries + ldd-resolved libs.
#   2. apt-get: install postgresql-16 on Ubuntu/Debian, copy from system paths.
#
# Requires root (for mount -o loop, initdb run, mkfs.ext4).
# On macOS, exits early - deploy-example.sh re-runs this on the server.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT_EXT4="$SCRIPT_DIR/sut.ext4"
SUT_SIZE_MB=512
PG_IMAGE="postgres:16-bookworm"
PG_VERSION=16
PG_UID=999
PG_GID=999

if [[ "$(uname)" == "Darwin" ]]; then
    echo "macOS detected: sut.ext4 creation requires Linux + root."
    echo "deploy-example.sh will re-run this script on the server."
    exit 0
fi

if [[ -f "$SUT_EXT4" ]]; then
    echo "sut.ext4 already present ($(du -h "$SUT_EXT4" | cut -f1)); skipping."
    exit 0
fi

if [[ "$(id -u)" -ne 0 ]]; then
    SUDO="sudo"
else
    SUDO=""
fi

TMPDIR=$(mktemp -d)
MNT=""
cleanup() {
    if [[ -n "$MNT" ]]; then
        $SUDO umount "$MNT" 2>/dev/null || true
        rmdir "$MNT" 2>/dev/null || true
    fi
    rm -rf "$TMPDIR"
    docker rm "${CONTAINER:-}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$TMPDIR/bin" "$TMPDIR/lib/x86_64-linux-gnu" "$TMPDIR/lib64" "$TMPDIR/pgdata-seed" "$TMPDIR/pgshare"

PG_BIN=""

# ── Method 1: Docker ─────────────────────────────────────────────────────────

if command -v docker &>/dev/null; then
    echo "==> Using Docker to extract postgres ${PG_VERSION} binaries..."
    docker pull --platform linux/amd64 "$PG_IMAGE" --quiet

    CONTAINER=$(docker create --platform linux/amd64 "$PG_IMAGE")

    for bin in postgres pg_ctl initdb pg_basebackup pg_isready psql; do
        docker cp "$CONTAINER:/usr/lib/postgresql/${PG_VERSION}/bin/$bin" "$TMPDIR/bin/$bin"
        chmod +x "$TMPDIR/bin/$bin"
        echo "    $bin ($(du -h "$TMPDIR/bin/$bin" | cut -f1))"
    done

    LIBS_RAW=$(docker run --rm --platform linux/amd64 "$PG_IMAGE" \
        sh -c "for b in postgres pg_ctl initdb pg_basebackup; do
                   ldd /usr/lib/postgresql/${PG_VERSION}/bin/\$b 2>/dev/null
               done" | grep "=> /" | awk '{print $3}' | sort -u)

    # Exclude only libc.so itself (already in initramfs via iptables bundling).
    # All other glibc companion libs (libm, libresolv, libpthread stub, etc.) must
    # be bundled here so openthesis-init can symlink them from sut.ext4 →
    # /lib/x86_64-linux-gnu/ - they are not present in the minimal initramfs.
    GLIBC_PATTERN="^libc\.so\."
    for lib_path in $LIBS_RAW; do
        lib_name=$(basename "$lib_path")
        echo "$lib_name" | grep -qE "$GLIBC_PATTERN" && continue
        docker cp "$CONTAINER:$lib_path" "$TMPDIR/lib/x86_64-linux-gnu/$lib_name" 2>/dev/null || true
    done
    docker cp "$CONTAINER:/lib64/ld-linux-x86-64.so.2" "$TMPDIR/lib64/ld-linux-x86-64.so.2" 2>/dev/null || true
    docker rm "$CONTAINER" >/dev/null 2>&1 || true
    CONTAINER=""
    PG_BIN="$TMPDIR/bin"

# ── Method 2: apt-get ────────────────────────────────────────────────────────
else
    echo "==> Docker not found; installing postgresql-${PG_VERSION} via apt-get..."

    if ! dpkg -l "postgresql-${PG_VERSION}" &>/dev/null; then
        if ! apt-cache show "postgresql-${PG_VERSION}" &>/dev/null; then
            $SUDO apt-get install -y -qq curl gnupg lsb-release
            curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | \
                $SUDO gpg --dearmor -o /etc/apt/trusted.gpg.d/postgresql.gpg
            echo "deb https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" | \
                $SUDO tee /etc/apt/sources.list.d/pgdg.list
            $SUDO apt-get update -qq
        fi
        $SUDO apt-get install -y -qq \
            "postgresql-${PG_VERSION}" "postgresql-client-${PG_VERSION}" \
            libssl3 libreadline8 liblz4-1 libzstd1 libpam0g libselinux1 2>&1 | tail -3
    fi

    SYS_BIN="/usr/lib/postgresql/${PG_VERSION}/bin"
    [[ -f "$SYS_BIN/postgres" ]] || { echo "!!! postgres not found at $SYS_BIN"; exit 1; }

    for bin in postgres pg_ctl initdb pg_basebackup pg_isready psql; do
        [[ -f "$SYS_BIN/$bin" ]] && { cp "$SYS_BIN/$bin" "$TMPDIR/bin/$bin"; chmod +x "$TMPDIR/bin/$bin"; echo "    $bin"; }
    done

    GLIBC_PATTERN="^libc\.so\."
    declare -A SEEN_LIBS
    for bin in postgres pg_ctl initdb pg_basebackup; do
        while IFS= read -r line; do
            lib_path=$(echo "$line" | awk '{print $3}')
            lib_name=$(basename "$lib_path" 2>/dev/null || true)
            [[ -z "$lib_path" || "$lib_path" == "not" ]] && continue
            echo "$lib_name" | grep -qE "$GLIBC_PATTERN" && continue
            [[ -n "${SEEN_LIBS[$lib_name]+x}" ]] && continue
            SEEN_LIBS[$lib_name]=1
            [[ -f "$lib_path" ]] && cp "$lib_path" "$TMPDIR/lib/x86_64-linux-gnu/$lib_name" && echo "    $lib_name"
        done < <(ldd "$SYS_BIN/$bin" 2>/dev/null | grep "=> /")
    done

    LD="/lib64/ld-linux-x86-64.so.2"
    [[ -f "$LD" ]] && cp "$LD" "$TMPDIR/lib64/ld-linux-x86-64.so.2"
    [[ -L "$LD" ]] && cp "$(readlink -f "$LD")" "$TMPDIR/lib64/ld-linux-x86-64.so.2" 2>/dev/null || true
    PG_BIN="$SYS_BIN"
fi

echo "==> Binaries: $(ls "$TMPDIR/bin/" | tr '\n' ' ')"
echo "==> Libs:     $(ls "$TMPDIR/lib/x86_64-linux-gnu/" | wc -l) shared libraries"

# ── Pre-initialize postgres data directory ───────────────────────────────────
# Running initdb inside the VM takes ~3 minutes (VM speed constraint).
# We pre-init here on the host and bake the result into sut.ext4 so pg-server
# can skip initdb entirely and start postgres in a few seconds.

echo ""
echo "==> Pre-initializing postgres data directory..."

# Run initdb as the postgres system user (avoids uid/gid conflicts from package install).
# If no postgres system user exists, fall back to running as the current user.
PGDATA="$TMPDIR/pgdata-seed"

RUN_AS_USER=""
if id postgres &>/dev/null; then
    RUN_AS_USER="postgres"
    # Allow the postgres user to traverse the tmpdir (mktemp creates 0700).
    chmod 755 "$TMPDIR" "$TMPDIR/bin" "$TMPDIR/lib" "$TMPDIR/lib/x86_64-linux-gnu"
    chown -R postgres "$PGDATA"
fi

# Prefer the system initdb binary: it has $libdir baked in to the installed
# extension directory and can run the bootstrap script successfully.
# Fall back to our extracted binary only if the system one is not found.
SYS_INITDB="/usr/lib/postgresql/${PG_VERSION}/bin/initdb"
if [[ ! -x "$SYS_INITDB" ]]; then
    SYS_INITDB="$TMPDIR/bin/initdb"
fi

if [[ -n "$RUN_AS_USER" ]]; then
    su "$RUN_AS_USER" -s /bin/sh -c \
        "'$SYS_INITDB' -D '$PGDATA' -U postgres --auth=trust -E UTF8 --no-locale 2>&1"
else
    # No non-root user available - create a minimal one.
    PGUSER="pgbuild_dst"
    useradd -M -U -s /bin/sh "$PGUSER" 2>/dev/null || true
    chown -R "$PGUSER" "$PGDATA"
    su "$PGUSER" -s /bin/sh -c \
        "'$SYS_INITDB' -D '$PGDATA' -U postgres --auth=trust -E UTF8 --no-locale 2>&1"
    userdel "$PGUSER" 2>/dev/null || true
fi

# Comment out timezone settings set by initdb - the VM initramfs has no
# /usr/share/zoneinfo, so named timezone lookups fail on startup.  With these
# commented out, postgres falls back to its compile-time default (GMT).
sed -i "s/^log_timezone = /#log_timezone = /" "$PGDATA/postgresql.conf"
sed -i "s/^timezone = /#timezone = /" "$PGDATA/postgresql.conf"

# Append DST-optimized postgresql.conf settings.
# These are appended last so they override any initdb-generated values.
cat >> "$PGDATA/postgresql.conf" <<'PG_CONF'

# OpenThesis DST settings
listen_addresses = '127.0.0.1'
port = 5432
# synchronous_commit=off: ack before WAL flush; creates crash data-loss window.
synchronous_commit = off
fsync = on
full_page_writes = on
max_connections = 16
shared_buffers = 32MB
wal_writer_delay = 200ms
checkpoint_timeout = 30s
log_min_messages = warning
logging_collector = off
# Minimal autovacuum to reduce background activity during DST bursts.
autovacuum_naptime = 30s
bgwriter_delay = 500ms
PG_CONF

# Trust auth: inside the FC guest there is no network exposure.
cat > "$PGDATA/pg_hba.conf" <<'PG_HBA'
local all all trust
host  all all 127.0.0.1/32 trust
PG_HBA

# ── Pre-create application schema ────────────────────────────────────────────
# Start postgres temporarily (as the initdb user), create the DST tables, then
# stop it.  The resulting pgdata-seed will have the correct relfilenodes baked
# in, so every fresh copy on a worker's scratch.ext4 already has the schema.

echo ""
echo "==> Pre-creating application schema in pgdata-seed..."

SYS_PSQL="/usr/lib/postgresql/${PG_VERSION}/bin/psql"
if [[ ! -x "$SYS_PSQL" ]]; then
    SYS_PSQL="$TMPDIR/bin/psql"
fi
SYS_PGCTL="/usr/lib/postgresql/${PG_VERSION}/bin/pg_ctl"
if [[ ! -x "$SYS_PGCTL" ]]; then
    SYS_PGCTL="$TMPDIR/bin/pg_ctl"
fi

# pg_ctl needs write access to the socket dir; create it as root first.
mkdir -p /var/run/postgresql
if [[ -n "$RUN_AS_USER" ]]; then
    chown "$RUN_AS_USER" /var/run/postgresql
fi

# Write the schema SQL to a temp file to avoid quoting issues.
SCHEMA_FILE="$TMPDIR/schema.sql"
cat > "$SCHEMA_FILE" <<'SCHEMA_EOF'
CREATE TABLE IF NOT EXISTS dst_writes (
    id    BIGINT PRIMARY KEY,
    value TEXT NOT NULL,
    ts    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS dst_pairs (
    pair_id BIGINT   NOT NULL,
    slot    SMALLINT NOT NULL CHECK (slot IN (0,1)),
    value   TEXT     NOT NULL,
    PRIMARY KEY (pair_id, slot)
);
CREATE TABLE IF NOT EXISTS dst_counter (
    id  INT   PRIMARY KEY DEFAULT 1,
    seq BIGINT NOT NULL DEFAULT 0
);
INSERT INTO dst_counter (id, seq) VALUES (1, 0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS dst_accounts (
    id      BIGINT PRIMARY KEY,
    balance BIGINT NOT NULL DEFAULT 10000 CHECK (balance >= 0)
);
INSERT INTO dst_accounts (id, balance)
    SELECT g, 10000 FROM generate_series(1, 3) AS g
    ON CONFLICT DO NOTHING;
SCHEMA_EOF
chmod 644 "$SCHEMA_FILE"

# Use port 5433 and unix socket in /tmp to avoid conflicting with any running
# system postgres on port 5432. -c listen_addresses= disables TCP so we don't
# need 127.0.0.1 binding at all - psql connects via the unix socket.
_pg_opts="-p 5433 -c listen_addresses= -k /tmp"
_pg_start="'$SYS_PGCTL' -D '$PGDATA' -l '$PGDATA/pg_schema_init.log' start -w -t 30 -o '$_pg_opts'"
_pg_schema="'$SYS_PSQL' -h /tmp -p 5433 -U postgres -f '$SCHEMA_FILE'"
_pg_stop="'$SYS_PGCTL' -D '$PGDATA' stop -m fast -o '-p 5433'"

if [[ -n "$RUN_AS_USER" ]]; then
    su "$RUN_AS_USER" -s /bin/sh -c "$_pg_start" 2>&1
    su "$RUN_AS_USER" -s /bin/sh -c "$_pg_schema" 2>&1
    su "$RUN_AS_USER" -s /bin/sh -c "$_pg_stop" 2>&1
else
    PGUSER="pgbuild_dst"
    useradd -M -U -s /bin/sh "$PGUSER" 2>/dev/null || true
    chown -R "$PGUSER" "$PGDATA"
    [[ -d /var/run/postgresql ]] && chown "$PGUSER" /var/run/postgresql
    su "$PGUSER" -s /bin/sh -c "$_pg_start" 2>&1
    su "$PGUSER" -s /bin/sh -c "$_pg_schema" 2>&1
    su "$PGUSER" -s /bin/sh -c "$_pg_stop" 2>&1
    userdel "$PGUSER" 2>/dev/null || true
fi

echo "==> Schema created in pgdata-seed."

# Ensure the pgdata dir is owned by the target uid/gid used inside the VM.
chown -R "$PG_UID:$PG_GID" "$PGDATA"

echo "==> pgdata-seed: $(du -sh "$PGDATA" | cut -f1)"

# Copy postgres share directory (timezonesets, tsearch_data, system SQL files etc.).
# pg-server sets PGSHAREDIR=/mnt/sut/pgshare so postgres can find its data files.
SYS_SHARE="/usr/share/postgresql/${PG_VERSION}"
if [[ -d "$SYS_SHARE" ]]; then
    cp -r "$SYS_SHARE/." "$TMPDIR/pgshare/"
    echo "==> pgshare: $(du -sh "$TMPDIR/pgshare" | cut -f1)"
fi

# ── Build sut.ext4 ───────────────────────────────────────────────────────────

echo ""
echo "==> Building sut.ext4 (${SUT_SIZE_MB} MiB)..."

dd if=/dev/zero of="$SUT_EXT4" bs=1M count="$SUT_SIZE_MB" status=progress
mkfs.ext4 -F -q "$SUT_EXT4"

MNT=$(mktemp -d)
$SUDO mount -o loop "$SUT_EXT4" "$MNT"
$SUDO cp -r "$TMPDIR/bin" "$TMPDIR/lib" "$TMPDIR/lib64" "$TMPDIR/pgdata-seed" "$TMPDIR/pgshare" "$MNT/"
$SUDO sync
$SUDO umount "$MNT"
rmdir "$MNT"
MNT=""

echo ""
echo "==> sut.ext4 ready: $(du -h "$SUT_EXT4" | cut -f1)"
echo "    binaries:  $(ls "$TMPDIR/bin/" | tr '\n' ' ')"
echo "    libs:      $(ls "$TMPDIR/lib/x86_64-linux-gnu/" | wc -l) shared libraries"
echo "    pgdata:    pre-initialized with synchronous_commit=off + application schema"
