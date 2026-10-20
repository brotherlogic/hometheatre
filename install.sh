#!/usr/bin/env bash
# ==============================================================================
# Home Theatre: Idempotent Linux Host Installer for DevPod (install.sh)
# ==============================================================================
# Responsibilities (per INTENT.md 4.1, DRIVE.md 3, METADATA.md 3.4, DOCKER.md 3):
# 1. Check host prerequisites (devpod / devpod-cli, util-linux / setpriv,
#    lsscsi, udev, curl).
# 2. Load and persist the generic SCSI kernel module (modprobe sg +
#    /etc/modules-load.d/hometheatre-sg.conf).
# 3. Auto-detect physical optical drives via sysfs (/sys/class/block/sr* and
#    /device/scsi_generic/sg*), handling 0, 1, or multiple drives cleanly.
# 4. Prompt for or validate .env configuration (HOST_NFS_DIR, TMDB_API_KEY,
#    SCRATCH_DIR, WEB_PORT, optional WEBHOOK_URL, DAEMON_WORKSPACE_ID).
# 5. Verify SCRATCH_DIR has at least 25 GiB (26,843,545,600 bytes) of free space.
# 6. Verify the host bind-mount directory HOST_NFS_DIR (default /mnt/nfs),
#    auto-detect share owner PUID/PGID, and test root_squash-compatible write
#    access via setpriv to initialize movies/, tv/, and unidentified/.
# 7. Validate TMDB_API_KEY against the TMDB API (v3 ?api_key= & v4 Bearer eyJ...).
# 8. Render deploy/devcontainer.json with host bind mounts (SCRATCH_DIR -> /scratch,
#    HOST_NFS_DIR -> /mnt/nfs) and optical device passthrough, then build & launch
#    the standalone DevPod daemon workspace (hometheatre-daemon), completely
#    separate from the development devcontainer.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-${SCRIPT_DIR}/.env}"
DEPLOY_DEVCONTAINER_FILE="${DEPLOY_DEVCONTAINER_FILE:-${SCRIPT_DIR}/deploy/devcontainer.json}"
SYSFS_BLOCK_DIR="${SYSFS_BLOCK_DIR:-/sys/class/block}"
DEV_DIR="${DEV_DIR:-/dev}"
MODULES_LOAD_CONF="${MODULES_LOAD_CONF:-/etc/modules-load.d/hometheatre-sg.conf}"
TMDB_API_BASE_URL="${TMDB_API_BASE_URL:-https://api.themoviedb.org/3}"

MIN_SCRATCH_BYTES=26843545600 # 25 GiB (25 * 1024^3)
REC_SCRATCH_BYTES=32212254720 # 30 GiB (30 * 1024^3)

RECONFIGURE=0
NON_INTERACTIVE=0
ENV_FILE_EXISTED=0
EXPLICIT_OPTICAL_SR=0
EXPLICIT_OPTICAL_SG=0
EXPLICIT_PUID=0
EXPLICIT_PGID=0
DEVPOD_BIN=""

# ------------------------------------------------------------------------------
# Logging Helpers
# ------------------------------------------------------------------------------
log_info() {
    printf '\033[1;34m[INFO]\033[0m %s\n' "$*"
}

log_ok() {
    printf '\033[1;32m[ OK ]\033[0m %s\n' "$*"
}

log_warn() {
    printf '\033[1;33m[WARN]\033[0m %s\n' "$*" >&2
}

log_error() {
    printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2
}

usage() {
    cat <<EOF
Usage: sudo ./install.sh [OPTIONS]

Options:
  -r, --reconfigure       Prompt interactively for configuration values even if .env exists
  -y, --non-interactive   Never prompt interactively; fail if required .env values are missing
  -h, --help              Show this help message
EOF
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -r|--reconfigure)
                RECONFIGURE=1
                shift
                ;;
            -y|--non-interactive)
                NON_INTERACTIVE=1
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                usage >&2
                exit 1
                ;;
        esac
    done
}

is_interactive() {
    [[ "$NON_INTERACTIVE" -eq 0 && -t 0 ]]
}

# ------------------------------------------------------------------------------
# 1. Root & Prerequisite Checks (DevPod + Host Tools)
# ------------------------------------------------------------------------------
check_root() {
    if [[ "${HOMETHEATRE_SKIP_ROOT_CHECK:-0}" == "1" ]]; then
        return 0
    fi
    if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
        log_error "install.sh must be run as root (e.g., 'sudo ./install.sh')."
        exit 1
    fi
}

pkg_installed() {
    dpkg -s "$1" >/dev/null 2>&1
}

resolve_devpod_bin() {
    if [[ -n "${DEVPOD_CLI:-}" ]] && command -v "${DEVPOD_CLI}" >/dev/null 2>&1; then
        DEVPOD_BIN="$(command -v "${DEVPOD_CLI}")"
        return 0
    fi
    if command -v devpod >/dev/null 2>&1; then
        DEVPOD_BIN="$(command -v devpod)"
        return 0
    fi
    if command -v devpod-cli >/dev/null 2>&1; then
        DEVPOD_BIN="$(command -v devpod-cli)"
        return 0
    fi
    if [[ -n "${SUDO_USER:-}" ]]; then
        local user_home
        user_home=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6 || true)
        if [[ -n "$user_home" ]]; then
            for candidate in "$user_home/.local/bin/devpod" "$user_home/.local/bin/devpod-cli" "$user_home/bin/devpod"; do
                if [[ -x "$candidate" ]]; then
                    DEVPOD_BIN="$candidate"
                    return 0
                fi
            done
        fi
    fi
    return 1
}

run_devpod() {
    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
        local user_home
        user_home=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6 || true)
        if [[ -n "$user_home" && -d "$user_home/.devpod" ]]; then
            sudo -u "$SUDO_USER" HOME="$user_home" "$DEVPOD_BIN" "$@"
            return $?
        fi
    fi
    "$DEVPOD_BIN" "$@"
}

install_prerequisites() {
    log_info "Checking host prerequisites and DevPod CLI..."

    if [[ "${EUID:-$(id -u)}" -eq 0 ]] && command -v apt-get >/dev/null 2>&1 && command -v dpkg >/dev/null 2>&1; then
        local base_pkgs=(
            util-linux
            lsscsi
            udev
            curl
        )
        local missing_pkgs=()

        for pkg in "${base_pkgs[@]}"; do
            if ! pkg_installed "$pkg"; then
                missing_pkgs+=("$pkg")
            fi
        done

        if [[ "${#missing_pkgs[@]}" -gt 0 ]]; then
            log_info "Updating apt package index..."
            apt-get update -qq
            log_info "Installing missing host packages: ${missing_pkgs[*]}"
            DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing_pkgs[@]}"
        fi
    fi

    if ! resolve_devpod_bin; then
        log_error "DevPod CLI ('devpod' or 'devpod-cli') was not found in PATH."
        log_error "Please install DevPod on this host (https://devpod.sh/docs/getting-started/install) or set DEVPOD_CLI=/path/to/devpod."
        return 1
    fi

    log_ok "Host prerequisites verified (DevPod CLI: ${DEVPOD_BIN})."
}

# ------------------------------------------------------------------------------
# 2. Generic SCSI (sg) Kernel Module Persistence
# ------------------------------------------------------------------------------
setup_sg_module() {
    log_info "Loading and persisting generic SCSI kernel module (sg)..."

    if ! modprobe sg 2>/dev/null; then
        if [[ -d /sys/class/scsi_generic || -d /sys/module/sg ]]; then
            log_info "Kernel 'sg' support is already active."
        else
            log_warn "'modprobe sg' returned non-zero; verify your kernel includes generic SCSI (CONFIG_CHR_DEV_SG) support."
        fi
    fi

    mkdir -p "$(dirname "$MODULES_LOAD_CONF")"
    if [[ ! -f "$MODULES_LOAD_CONF" ]] || ! grep -qx "sg" "$MODULES_LOAD_CONF" 2>/dev/null; then
        printf 'sg\n' > "$MODULES_LOAD_CONF"
        chmod 0644 "$MODULES_LOAD_CONF"
        log_ok "Persisted 'sg' module in ${MODULES_LOAD_CONF}."
    else
        log_ok "'sg' module persistence already configured in ${MODULES_LOAD_CONF}."
    fi

    if command -v udevadm >/dev/null 2>&1; then
        udevadm settle 2>/dev/null || true
    fi
}

# ------------------------------------------------------------------------------
# 3. Optical Drive Auto-Detection via sysfs
# ------------------------------------------------------------------------------
discover_optical_drives() {
    modprobe sg 2>/dev/null || true
    local sr_sys sr_name sr_dev sg_sys sg_name sg_dev vendor model

    for sr_sys in "${SYSFS_BLOCK_DIR}"/sr*; do
        [[ -e "$sr_sys" ]] || continue
        sr_name=$(basename "$sr_sys")
        sr_dev="${DEV_DIR}/${sr_name}"

        sg_dev=""
        for sg_sys in "$sr_sys"/device/scsi_generic/sg*; do
            if [[ -e "$sg_sys" ]]; then
                sg_name=$(basename "$sg_sys")
                sg_dev="${DEV_DIR}/${sg_name}"
                break
            fi
        done

        if [[ -z "$sg_dev" ]]; then
            continue
        fi

        vendor=$(tr -d '[:space:]' < "$sr_sys/device/vendor" 2>/dev/null || echo "Unknown")
        model=$(sed 's/[[:space:]]*$//' "$sr_sys/device/model" 2>/dev/null || echo "Optical Drive")
        [[ -n "$vendor" ]] || vendor="Unknown"
        [[ -n "$model" ]] || model="Optical Drive"

        echo "${sr_dev}|${sg_dev}|${vendor} ${model}"
    done
}

configure_optical_drive() {
    log_info "Scanning sysfs (${SYSFS_BLOCK_DIR}/sr*) for physical optical drives..."

    local drives=()
    local line
    while IFS= read -r line; do
        [[ -n "$line" ]] && drives+=("$line")
    done < <(discover_optical_drives)

    local count="${#drives[@]}"

    if [[ "$count" -eq 1 ]]; then
        IFS='|' read -r OPTICAL_SR_DEVICE OPTICAL_SG_DEVICE drive_label <<< "${drives[0]}"
        log_ok "Detected optical drive: ${drive_label} (${OPTICAL_SR_DEVICE} -> ${OPTICAL_SG_DEVICE})"
    elif [[ "$count" -gt 1 ]]; then
        local matched=0
        if [[ "$RECONFIGURE" -eq 0 && "$EXPLICIT_OPTICAL_SR" -eq 1 && "$EXPLICIT_OPTICAL_SG" -eq 1 ]]; then
            for entry in "${drives[@]}"; do
                IFS='|' read -r sr_candidate sg_candidate drive_label <<< "$entry"
                if [[ "$sr_candidate" == "$OPTICAL_SR_DEVICE" && "$sg_candidate" == "$OPTICAL_SG_DEVICE" ]]; then
                    log_ok "Using configured optical drive from .env: ${drive_label} (${OPTICAL_SR_DEVICE} -> ${OPTICAL_SG_DEVICE})"
                    matched=1
                    break
                fi
            done
        fi

        if [[ "$matched" -eq 0 ]]; then
            log_info "Multiple optical drives detected on this host:"
            local i=1
            for entry in "${drives[@]}"; do
                IFS='|' read -r sr_candidate sg_candidate drive_label <<< "$entry"
                printf '  %d) %s (%s + %s)\n' "$i" "$drive_label" "$sr_candidate" "$sg_candidate"
                ((i+=1))
            done

            if ! is_interactive; then
                log_error "Multiple optical drives found and running non-interactively. Set OPTICAL_SR_DEVICE and OPTICAL_SG_DEVICE in ${ENV_FILE}."
                return 1
            fi

            local selection=""
            while true; do
                read -r -p "Select the optical drive to use [1-${count}]: " selection
                if [[ "$selection" =~ ^[0-9]+$ ]] && (( selection >= 1 && selection <= count )); then
                    IFS='|' read -r OPTICAL_SR_DEVICE OPTICAL_SG_DEVICE drive_label <<< "${drives[$((selection - 1))]}"
                    log_ok "Selected optical drive: ${drive_label} (${OPTICAL_SR_DEVICE} -> ${OPTICAL_SG_DEVICE})"
                    break
                fi
                log_warn "Invalid selection '${selection}'. Please enter a number between 1 and ${count}."
            done
        fi
    else
        if [[ "$EXPLICIT_OPTICAL_SR" -eq 1 && "$EXPLICIT_OPTICAL_SG" -eq 1 && -n "${OPTICAL_SR_DEVICE:-}" && -n "${OPTICAL_SG_DEVICE:-}" ]]; then
            log_warn "No physical optical drive detected in ${SYSFS_BLOCK_DIR}/sr*, using explicit override from .env (${OPTICAL_SR_DEVICE} + ${OPTICAL_SG_DEVICE})."
        else
            log_error "No physical optical drive detected in ${SYSFS_BLOCK_DIR}/sr* (with matching /device/scsi_generic/sg*)."
            log_error "Please connect a DVD drive to this host (or explicitly set OPTICAL_SR_DEVICE and OPTICAL_SG_DEVICE in ${ENV_FILE}) and re-run ./install.sh."
            return 1
        fi
    fi

    local detected_gid=""
    if [[ -e "$OPTICAL_SR_DEVICE" ]]; then
        detected_gid=$(stat -c '%g' "$OPTICAL_SR_DEVICE" 2>/dev/null || true)
    fi
    if [[ -z "$detected_gid" || "$detected_gid" == "0" ]]; then
        detected_gid=$(getent group cdrom 2>/dev/null | cut -d: -f3 || true)
    fi
    CDROM_GID="${CDROM_GID:-${detected_gid:-24}}"

    if [[ -e "$OPTICAL_SR_DEVICE" && ! -b "$OPTICAL_SR_DEVICE" ]]; then
        log_warn "${OPTICAL_SR_DEVICE} exists but is not a block device."
    fi
    if [[ -e "$OPTICAL_SG_DEVICE" && ! -c "$OPTICAL_SG_DEVICE" ]]; then
        log_warn "${OPTICAL_SG_DEVICE} exists but is not a character device."
    fi

    log_ok "Optical device mapping ready: ${OPTICAL_SR_DEVICE}:/dev/sr0, ${OPTICAL_SG_DEVICE}:${OPTICAL_SG_DEVICE} (CDROM_GID=${CDROM_GID})."
}

# ------------------------------------------------------------------------------
# 4. Load & Prompt/Validate .env Configuration
# ------------------------------------------------------------------------------
load_env_file() {
    [[ -n "${OPTICAL_SR_DEVICE:-}" ]] && EXPLICIT_OPTICAL_SR=1
    [[ -n "${OPTICAL_SG_DEVICE:-}" ]] && EXPLICIT_OPTICAL_SG=1
    [[ -n "${PUID:-}" ]] && EXPLICIT_PUID=1
    [[ -n "${PGID:-}" ]] && EXPLICIT_PGID=1

    if [[ ! -f "$ENV_FILE" ]]; then
        ENV_FILE_EXISTED=0
        return 0
    fi

    ENV_FILE_EXISTED=1
    log_info "Loading existing configuration from ${ENV_FILE}..."

    local env_sr="${EXPLICIT_OPTICAL_SR}"
    local env_sg="${EXPLICIT_OPTICAL_SG}"
    local env_puid="${EXPLICIT_PUID}"
    local env_pgid="${EXPLICIT_PGID}"

    local line key val
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [[ -z "$line" || "$line" == \#* ]] && continue

        if [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
            key="${BASH_REMATCH[1]}"
            val="${BASH_REMATCH[2]}"

            if [[ "$val" =~ ^\"(.*)\"$ ]]; then
                val="${BASH_REMATCH[1]}"
            elif [[ "$val" =~ ^\'(.*)\'$ ]]; then
                val="${BASH_REMATCH[1]}"
            fi

            case "$key" in
                OPTICAL_SR_DEVICE)
                    if [[ "$EXPLICIT_OPTICAL_SR" -eq 0 && -n "$val" ]]; then
                        OPTICAL_SR_DEVICE="$val"
                        EXPLICIT_OPTICAL_SR=1
                    fi
                    ;;
                OPTICAL_SG_DEVICE)
                    if [[ "$EXPLICIT_OPTICAL_SG" -eq 0 && -n "$val" ]]; then
                        OPTICAL_SG_DEVICE="$val"
                        EXPLICIT_OPTICAL_SG=1
                    fi
                    ;;
                PUID)
                    if [[ "$EXPLICIT_PUID" -eq 0 && -n "$val" ]]; then
                        PUID="$val"
                        EXPLICIT_PUID=1
                    fi
                    ;;
                PGID)
                    if [[ "$EXPLICIT_PGID" -eq 0 && -n "$val" ]]; then
                        PGID="$val"
                        EXPLICIT_PGID=1
                    fi
                    ;;
                CDROM_GID|HOST_NFS_DIR|SCRATCH_DIR|TMDB_API_KEY|WEB_PORT|WEBHOOK_URL|DAEMON_WORKSPACE_ID)
                    if [[ -z "${!key:-}" ]]; then
                        printf -v "$key" '%s' "$val"
                    fi
                    ;;
            esac
        fi
    done < "$ENV_FILE"

    if [[ "${TMDB_API_KEY:-}" == "your_tmdb_api_key_here" ]]; then
        TMDB_API_KEY=""
        EXPLICIT_OPTICAL_SR="$env_sr"
        EXPLICIT_OPTICAL_SG="$env_sg"
        EXPLICIT_PUID="$env_puid"
        EXPLICIT_PGID="$env_pgid"
        if is_interactive; then
            RECONFIGURE=1
        fi
    fi
}

prompt_value() {
    local var_name="$1"
    local prompt_text="$2"
    local default_val="${3:-}"
    local required="${4:-1}"
    local current_val="${!var_name:-}"

    if [[ "$RECONFIGURE" -eq 1 ]] || [[ -z "$current_val" ]]; then
        local effective_default="${current_val:-$default_val}"
        if is_interactive; then
            local input=""
            while true; do
                if [[ -n "$effective_default" ]]; then
                    read -r -p "${prompt_text} [${effective_default}]: " input
                    input="${input:-$effective_default}"
                else
                    read -r -p "${prompt_text}: " input
                fi
                if [[ "$required" -eq 0 || -n "$input" ]]; then
                    printf -v "$var_name" '%s' "$input"
                    break
                fi
                log_warn "${var_name} cannot be empty."
            done
        else
            if [[ -n "$effective_default" ]]; then
                printf -v "$var_name" '%s' "$effective_default"
            elif [[ "$required" -eq 1 ]]; then
                log_error "Required configuration '${var_name}' is missing in ${ENV_FILE} (and running non-interactively)."
                return 1
            fi
        fi
    fi
}

prompt_and_validate_config() {
    log_info "Validating configuration values..."

    if [[ "${TMDB_API_KEY:-}" == "your_tmdb_api_key_here" ]]; then
        TMDB_API_KEY=""
    fi

    prompt_value "HOST_NFS_DIR" "Enter host directory where the NAS media share is mounted (bind-mounted to /mnt/nfs)" "/mnt/nfs" 1
    if [[ "${HOST_NFS_DIR}" != /* ]]; then
        log_error "HOST_NFS_DIR ('${HOST_NFS_DIR}') must be an absolute path starting with '/'."
        return 1
    fi
    if [[ "${#HOST_NFS_DIR}" -gt 1 ]]; then
        HOST_NFS_DIR="${HOST_NFS_DIR%/}"
    fi

    prompt_value "TMDB_API_KEY" "Enter TMDB API Key (v3 32-char hex key or v4 Bearer Read Access Token)" "" 1
    prompt_value "SCRATCH_DIR" "Enter local scratch directory (>= 25 GiB free required)" "/var/lib/hometheatre/scratch" 1
    prompt_value "WEB_PORT" "Enter host port for the embedded Web UI" "8080" 1

    if [[ ! "${WEB_PORT}" =~ ^[0-9]+$ ]] || (( WEB_PORT < 1 || WEB_PORT > 65535 )); then
        log_error "Invalid WEB_PORT '${WEB_PORT}'. Must be a TCP port between 1 and 65535."
        return 1
    fi

    if [[ "$ENV_FILE_EXISTED" -eq 0 || "$RECONFIGURE" -eq 1 ]]; then
        prompt_value "WEBHOOK_URL" "Enter optional webhook URL for notifications (leave blank to skip)" "" 0
    else
        WEBHOOK_URL="${WEBHOOK_URL:-}"
    fi

    if [[ -n "${WEBHOOK_URL:-}" && ! "${WEBHOOK_URL}" =~ ^https?:// ]]; then
        log_error "Invalid WEBHOOK_URL '${WEBHOOK_URL}'. Must start with http:// or https:// (or be empty)."
        return 1
    fi

    DAEMON_WORKSPACE_ID="${DAEMON_WORKSPACE_ID:-hometheatre-daemon}"
}

# ------------------------------------------------------------------------------
# 5. Scratch Directory Free Space Verification (>= 25 GiB)
# ------------------------------------------------------------------------------
verify_scratch_dir() {
    log_info "Checking local scratch directory (${SCRATCH_DIR})..."

    if [[ "${SCRATCH_DIR}" != /* ]]; then
        log_error "SCRATCH_DIR ('${SCRATCH_DIR}') must be an absolute path."
        return 1
    fi

    mkdir -p "$SCRATCH_DIR"

    local avail_bytes
    avail_bytes=$(df -B1 --output=avail "$SCRATCH_DIR" 2>/dev/null | awk 'NR==2 {gsub(/[[:space:]]/, "", $1); print $1}')

    if [[ -z "$avail_bytes" || ! "$avail_bytes" =~ ^[0-9]+$ ]]; then
        log_error "Unable to determine available disk space for SCRATCH_DIR '${SCRATCH_DIR}'."
        return 1
    fi

    local avail_gib
    avail_gib=$(awk "BEGIN {printf \"%.2f\", ${avail_bytes} / 1073741824}")

    if (( avail_bytes < MIN_SCRATCH_BYTES )); then
        log_error "Insufficient free disk space on ${SCRATCH_DIR}: ${avail_gib} GiB (${avail_bytes} bytes) available."
        log_error "At least 25.00 GiB (${MIN_SCRATCH_BYTES} bytes) of free local space is required (30+ GiB recommended for back-to-back dual-layer DVD-9 ripping)."
        return 1
    elif (( avail_bytes < REC_SCRATCH_BYTES )); then
        log_warn "Scratch directory ${SCRATCH_DIR} has ${avail_gib} GiB free (meets >= 25 GiB requirement; 30+ GiB is recommended for back-to-back dual-layer DVD-9 ripping)."
    else
        log_ok "Scratch directory ${SCRATCH_DIR} verified (${avail_gib} GiB free >= 25 GiB required)."
    fi
}

# ------------------------------------------------------------------------------
# 6. Host NFS Bind-Mount Verification, PUID/PGID Detection & Directory Init
# ------------------------------------------------------------------------------
is_mounted_dir() {
    local target="$1"
    if command -v mountpoint >/dev/null 2>&1; then
        mountpoint -q "$target" 2>/dev/null
        return $?
    fi
    awk -v dir="$target" '$2 == dir {found=1} END {exit !found}' /proc/mounts 2>/dev/null
}

verify_and_init_nfs() {
    log_info "Verifying host bind-mount directory (${HOST_NFS_DIR})..."

    if [[ ! -d "$HOST_NFS_DIR" ]]; then
        log_error "Host NFS directory '${HOST_NFS_DIR}' does not exist."
        log_error "Please mount your NAS media share at '${HOST_NFS_DIR}' on the host before running ./install.sh."
        return 1
    fi

    if is_mounted_dir "$HOST_NFS_DIR"; then
        log_ok "Host NFS directory ${HOST_NFS_DIR} is mounted."
    else
        log_warn "Host directory ${HOST_NFS_DIR} is not a distinct mountpoint (using existing directory for bind mount)."
    fi

    local share_uid share_gid
    share_uid=$(stat -c '%u' "$HOST_NFS_DIR" 2>/dev/null || echo "0")
    share_gid=$(stat -c '%g' "$HOST_NFS_DIR" 2>/dev/null || echo "0")

    if [[ "$EXPLICIT_PUID" -eq 0 || -z "${PUID:-}" ]]; then
        if [[ "$share_uid" != "0" && "$share_uid" != "65534" ]]; then
            PUID="$share_uid"
        else
            PUID="${SUDO_UID:-1000}"
        fi
    fi

    if [[ "$EXPLICIT_PGID" -eq 0 || -z "${PGID:-}" ]]; then
        if [[ "$share_gid" != "0" && "$share_gid" != "65534" ]]; then
            PGID="$share_gid"
        else
            PGID="${SUDO_GID:-1000}"
        fi
    fi

    log_info "Testing root_squash-compatible write access and initializing directories on ${HOST_NFS_DIR} as PUID=${PUID}, PGID=${PGID} (share owner is ${share_uid}:${share_gid})..."

    if ! setpriv --reuid="$PUID" --regid="$PGID" --clear-groups \
        bash -c "mkdir -p '$HOST_NFS_DIR/movies' '$HOST_NFS_DIR/tv' '$HOST_NFS_DIR/unidentified' && \
                 touch '$HOST_NFS_DIR/.hometheatre_write_test' && \
                 rm -f '$HOST_NFS_DIR/.hometheatre_write_test'" 2>/dev/null; then
        log_error "Write test failed (EACCES) on '${HOST_NFS_DIR}' as UID:GID ${PUID}:${PGID}."
        log_error "How to fix:"
        log_error "  - Ensure the NAS NFS export mounted at '${HOST_NFS_DIR}' grants Read/Write (rw) access to this host."
        log_error "  - Ensure filesystem permissions on '${HOST_NFS_DIR}' allow writes by UID:GID ${PUID}:${PGID} (detected share owner: ${share_uid}:${share_gid})."
        log_error "  - If using Squash All Users / Map to Guest on Synology/QNAP/TrueNAS, ensure the mapped guest user has write permissions, or set PUID=${share_uid} PGID=${share_gid} in ${ENV_FILE}."
        return 1
    fi

    log_ok "Verified write access on ${HOST_NFS_DIR} (PUID=${PUID}, PGID=${PGID}) and initialized movies/, tv/, and unidentified/ directories."
}

# ------------------------------------------------------------------------------
# 7. TMDB API Key Validation (v3 API Key & v4 Bearer Read Access Token)
# ------------------------------------------------------------------------------
validate_tmdb_api_key() {
    log_info "Validating TMDB_API_KEY against TMDB API (${TMDB_API_BASE_URL}/configuration)..."

    local http_code=""
    local auth_mode=""

    if [[ "$TMDB_API_KEY" == eyJ* || "${#TMDB_API_KEY}" -gt 64 ]]; then
        auth_mode="v4 Bearer Read Access Token"
        http_code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 15 \
            -H "Authorization: Bearer ${TMDB_API_KEY}" \
            -H "Accept: application/json" \
            "${TMDB_API_BASE_URL}/configuration" 2>/dev/null || echo "000")
    else
        auth_mode="v3 API Key"
        http_code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 15 \
            -H "Accept: application/json" \
            "${TMDB_API_BASE_URL}/configuration?api_key=${TMDB_API_KEY}" 2>/dev/null || echo "000")
    fi

    if [[ "$http_code" == "200" ]]; then
        log_ok "TMDB API credential validated (${auth_mode})."
        return 0
    elif [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
        log_error "TMDB API rejected TMDB_API_KEY (${auth_mode}, HTTP ${http_code})."
        log_error "Please verify your v3 API Key or v4 Read Access Token at https://www.themoviedb.org/settings/api."
        return 1
    else
        log_error "Failed to validate TMDB_API_KEY against ${TMDB_API_BASE_URL}/configuration (HTTP ${http_code})."
        log_error "Check internet connectivity and DNS resolution on this host."
        return 1
    fi
}

# ------------------------------------------------------------------------------
# 8. Write .env, Render deploy/devcontainer.json & Launch DevPod Daemon
# ------------------------------------------------------------------------------
write_env_file() {
    log_info "Writing validated configuration to ${ENV_FILE}..."
    local tmp_env="${ENV_FILE}.tmp.$$"

    cat > "$tmp_env" <<EOF
# ==============================================================================
# Home Theatre: Automated DVD ISO Ripper & Kodi NFS Pipeline (.env)
# Generated by install.sh
# ==============================================================================

# Optical Drive Passthrough (auto-detected by install.sh)
OPTICAL_SR_DEVICE=${OPTICAL_SR_DEVICE}
OPTICAL_SG_DEVICE=${OPTICAL_SG_DEVICE}
CDROM_GID=${CDROM_GID}

# Host NFS Bind Mount & File Ownership (auto-detected from HOST_NFS_DIR by install.sh)
HOST_NFS_DIR=${HOST_NFS_DIR}
PUID=${PUID}
PGID=${PGID}

# Local Scratch Storage (>= 25 GiB free space required)
SCRATCH_DIR=${SCRATCH_DIR}

# Metadata, Web UI & DevPod Workspace
TMDB_API_KEY=${TMDB_API_KEY}
WEB_PORT=${WEB_PORT}
WEBHOOK_URL=${WEBHOOK_URL:-}
DAEMON_WORKSPACE_ID=${DAEMON_WORKSPACE_ID}
EOF

    chmod 0600 "$tmp_env"
    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
        chown "${SUDO_USER}:${SUDO_GID:-$(id -g "$SUDO_USER")}" "$tmp_env" 2>/dev/null || true
    fi
    mv "$tmp_env" "$ENV_FILE"
    log_ok "Saved configuration to ${ENV_FILE}."
}

write_deploy_devcontainer() {
    log_info "Updating DevPod daemon configuration at ${DEPLOY_DEVCONTAINER_FILE}..."
    mkdir -p "$(dirname "$DEPLOY_DEVCONTAINER_FILE")"
    local tmp_dc="${DEPLOY_DEVCONTAINER_FILE}.tmp.$$"

    cat > "$tmp_dc" <<EOF
{
	"name": "Home Theatre Daemon",
	"build": {
		"dockerfile": "../Dockerfile",
		"context": ".."
	},
	"workspaceFolder": "/workspaces/hometheatre",
	"remoteUser": "root",
	"capAdd": [
		"SYS_RAWIO"
	],
	"runArgs": [
		"--security-opt",
		"label=disable",
		"--network=host",
		"--device=${OPTICAL_SR_DEVICE}:/dev/sr0",
		"--device=${OPTICAL_SG_DEVICE}:${OPTICAL_SG_DEVICE}",
		"--group-add=${CDROM_GID}",
		"--env-file",
		"\${localWorkspaceFolder}/.env"
	],
	"mounts": [
		"source=${SCRATCH_DIR},target=/scratch,type=bind",
		"source=${HOST_NFS_DIR},target=/mnt/nfs,type=bind"
	],
	"containerEnv": {
		"OPTICAL_DEVICE": "/dev/sr0",
		"CONTAINER_OPTICAL_DEVICE": "/dev/sr0",
		"CONTAINER_SCRATCH_DIR": "/scratch",
		"CONTAINER_NFS_ROOT": "/mnt/nfs",
		"DVDCSS_METHOD": "key",
		"DVDCSS_CACHE": "/scratch/.dvdcss",
		"DVDCSS_VERBOSE": "1"
	},
	"postStartCommand": "/usr/local/bin/start-daemon.sh"
}
EOF

    chmod 0644 "$tmp_dc"
    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
        chown "${SUDO_USER}:${SUDO_GID:-$(id -g "$SUDO_USER")}" "$tmp_dc" 2>/dev/null || true
    fi
    mv "$tmp_dc" "$DEPLOY_DEVCONTAINER_FILE"
    log_ok "Rendered ${DEPLOY_DEVCONTAINER_FILE} with bind mounts (${SCRATCH_DIR}:/scratch, ${HOST_NFS_DIR}:/mnt/nfs)."
}

deploy_devpod_daemon() {
    write_deploy_devcontainer

    local rel_devcontainer="deploy/devcontainer.json"
    if [[ "$DEPLOY_DEVCONTAINER_FILE" != "${SCRIPT_DIR}/deploy/devcontainer.json" ]]; then
        rel_devcontainer="$DEPLOY_DEVCONTAINER_FILE"
    fi

    log_info "Building and launching standalone DevPod daemon workspace '${DAEMON_WORKSPACE_ID}'..."
    run_devpod up "$SCRIPT_DIR" \
        --id "$DAEMON_WORKSPACE_ID" \
        --devcontainer-path "$rel_devcontainer" \
        --ide none \
        --recreate

    # Ensure background supervisor is started even if the provider skipped postStartCommand
    run_devpod ssh "$DAEMON_WORKSPACE_ID" --command "/usr/local/bin/start-daemon.sh" >/dev/null 2>&1 || true

    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" && -d "${SCRIPT_DIR}/.git" ]]; then
        chown -R "${SUDO_USER}:${SUDO_GID:-$(id -g "$SUDO_USER")}" "${SCRIPT_DIR}/.git" 2>/dev/null || true
    fi

    log_ok "Home Theatre DevPod daemon container '${DAEMON_WORKSPACE_ID}' is up and running."
}

# ------------------------------------------------------------------------------
# 9. Final Onboarding Summary & Kodi Instructions
# ------------------------------------------------------------------------------
print_summary() {
    local host_ip
    host_ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
    [[ -n "$host_ip" ]] || host_ip="localhost"

    cat <<EOF

================================================================================
Home Theatre Installation Complete (DevPod)!
================================================================================

1. Local Web UI & Unidentified Disc Tagging Queue:
   - URL:            http://${host_ip}:${WEB_PORT}
   - Local Fallback: http://localhost:${WEB_PORT}

2. DevPod Daemon Container & Host Storage:
   - DevPod Workspace: ${DAEMON_WORKSPACE_ID} (config: deploy/devcontainer.json)
   - Optical Drive:    ${OPTICAL_SR_DEVICE} -> /dev/sr0 (${OPTICAL_SG_DEVICE}, gid=${CDROM_GID})
   - Scratch Bind:     ${SCRATCH_DIR} -> /scratch
   - Host NFS Bind:    ${HOST_NFS_DIR} -> /mnt/nfs (PUID=${PUID}, PGID=${PGID})

3. Kodi Setup Instructions (Google TV & Tablet):
   In Kodi, go to Settings -> Media -> Library -> Videos -> Add videos...
   Add the following two NFS network sources from your NAS export:

   - Movies Source:   nfs://<NAS_IP>/<NAS_EXPORT_PATH>/movies
     (Set content: "Movies", Scraper: "Local information only" or "TMDB")

   - TV Shows Source: nfs://<NAS_IP>/<NAS_EXPORT_PATH>/tv
     (Set content: "TV shows", Scraper: "Local information only" or "TMDB TV Shows")

   IMPORTANT — NAS 'insecure' Export Requirement for Kodi on Google TV / Android:
   Standalone Kodi apps on Google TV and Android tablets use userspace libnfs,
   which connects from unprivileged source ports (> 1024). Ensure your NAS NFS
   export enables the 'insecure' option (e.g., 'insecure' in Linux /etc/exports,
   "Allow non-root mount" on TrueNAS, or default non-root port access on
   Synology/QNAP) so Kodi does not receive Permission Denied.
================================================================================
EOF
}

main() {
    parse_args "$@"
    check_root
    load_env_file
    install_prerequisites
    setup_sg_module
    configure_optical_drive
    prompt_and_validate_config
    verify_scratch_dir
    verify_and_init_nfs
    validate_tmdb_api_key
    write_env_file
    deploy_devpod_daemon
    print_summary
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
