#!/usr/bin/env bash
# ==============================================================================
# Home Theatre: Idempotent Debian/Ubuntu Host Installer (install.sh)
# ==============================================================================
# Responsibilities (per INTENT.md 4.1, DRIVE.md 3, METADATA.md 3.4, DOCKER.md 3):
# 1. Check and install host prerequisites (docker.io / docker-ce,
#    docker-compose-plugin, nfs-common, dnsutils, util-linux, lsscsi, udev, curl).
# 2. Load and persist the generic SCSI kernel module (modprobe sg +
#    /etc/modules-load.d/hometheatre-sg.conf).
# 3. Auto-detect physical optical drives via sysfs (/sys/class/block/sr* and
#    /device/scsi_generic/sg*), handling 0, 1, or multiple drives cleanly.
# 4. Prompt for or validate .env configuration (NFS_SERVER, NFS_PATH,
#    TMDB_API_KEY, SCRATCH_DIR, WEB_PORT, optional WEBHOOK_URL).
# 5. Verify SCRATCH_DIR has at least 25 GiB (26,843,545,600 bytes) of free space.
# 6. Resolve NFS_SERVER -> NFS_SERVER_IP via getent ahostsv4, auto-probe
#    nfsvers=4 (fallback nfsvers=3,nolock), auto-detect share owner PUID/PGID,
#    and test root_squash-compatible write access via setpriv to initialize
#    movies/, tv/, and unidentified/.
# 7. Validate TMDB_API_KEY against the TMDB API (v3 ?api_key= & v4 Bearer eyJ...).
# 8. Recreate the hometheatre_nfs_media Docker volume wrapper if NFS options
#    changed, build & launch the Docker Compose stack, and print the Web UI URL
#    + Kodi NFS setup instructions (including the 'insecure' export requirement).
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-${SCRIPT_DIR}/.env}"
COMPOSE_FILE="${COMPOSE_FILE:-${SCRIPT_DIR}/docker-compose.yml}"
SYSFS_BLOCK_DIR="${SYSFS_BLOCK_DIR:-/sys/class/block}"
DEV_DIR="${DEV_DIR:-/dev}"
MODULES_LOAD_CONF="${MODULES_LOAD_CONF:-/etc/modules-load.d/hometheatre-sg.conf}"
TMDB_API_BASE_URL="${TMDB_API_BASE_URL:-https://api.themoviedb.org/3}"

MIN_SCRATCH_BYTES=26843545600 # 25 GiB (25 * 1024^3)
REC_SCRATCH_BYTES=32212254720 # 30 GiB (30 * 1024^3)

NFS_OPTS_V4="rw,nfsvers=4,hard,timeo=600,retrans=2,rsize=1048576,wsize=1048576,noatime"
NFS_OPTS_V3="rw,nfsvers=3,nolock,hard,timeo=600,retrans=2,rsize=1048576,wsize=1048576,noatime"

RECONFIGURE=0
NON_INTERACTIVE=0
ENV_FILE_EXISTED=0
EXPLICIT_OPTICAL_SR=0
EXPLICIT_OPTICAL_SG=0
EXPLICIT_PUID=0
EXPLICIT_PGID=0
TMP_MNT=""

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

cleanup_tmp_mount() {
    if [[ -n "${TMP_MNT:-}" ]]; then
        umount -lf "$TMP_MNT" 2>/dev/null || true
        rmdir "$TMP_MNT" 2>/dev/null || true
        TMP_MNT=""
    fi
}

trap cleanup_tmp_mount EXIT

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
# 1. Root & Prerequisite Package Checks
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

check_os_release() {
    if [[ -f /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        if [[ "${ID:-}" != "debian" && "${ID:-}" != "ubuntu" && "${ID_LIKE:-}" != *"debian"* ]]; then
            log_warn "Detected OS '${PRETTY_NAME:-${ID:-unknown}}'. This script targets Debian/Ubuntu hosts."
        fi
    fi
}

pkg_installed() {
    dpkg -s "$1" >/dev/null 2>&1
}

pkg_candidate_exists() {
    local candidate
    candidate=$(apt-cache policy "$1" 2>/dev/null | awk '/Candidate:/ {print $2}')
    [[ -n "$candidate" && "$candidate" != "(none)" ]]
}

install_prerequisites() {
    log_info "Checking Debian/Ubuntu host prerequisites..."

    local base_pkgs=(
        nfs-common
        dnsutils
        util-linux
        lsscsi
        udev
        curl
    )
    local missing_pkgs=()
    local need_docker=0
    local need_compose=0

    for pkg in "${base_pkgs[@]}"; do
        if ! pkg_installed "$pkg"; then
            missing_pkgs+=("$pkg")
        fi
    done

    if ! command -v docker >/dev/null 2>&1; then
        need_docker=1
    fi

    if ! docker compose version >/dev/null 2>&1; then
        need_compose=1
    fi

    if [[ "${#missing_pkgs[@]}" -gt 0 || "$need_docker" -eq 1 || "$need_compose" -eq 1 ]]; then
        log_info "Updating apt package index..."
        apt-get update -qq

        if [[ "$need_docker" -eq 1 ]]; then
            if pkg_candidate_exists docker-ce; then
                missing_pkgs+=("docker-ce")
            else
                missing_pkgs+=("docker.io")
            fi
        fi

        if [[ "$need_compose" -eq 1 ]]; then
            if pkg_candidate_exists docker-compose-plugin; then
                missing_pkgs+=("docker-compose-plugin")
            elif pkg_candidate_exists docker-compose-v2; then
                missing_pkgs+=("docker-compose-v2")
            else
                missing_pkgs+=("docker-compose-plugin")
            fi
        fi

        log_info "Installing missing host packages: ${missing_pkgs[*]}"
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing_pkgs[@]}"
    fi

    if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
        systemctl enable --now docker >/dev/null 2>&1 || true
    fi

    if ! command -v docker >/dev/null 2>&1; then
        log_error "Docker CLI is not available after package installation."
        return 1
    fi

    if ! docker compose version >/dev/null 2>&1; then
        log_error "'docker compose' plugin is not available. Please install docker-compose-plugin."
        return 1
    fi

    log_ok "All required host packages are installed (docker, docker compose, nfs-common, dnsutils, util-linux, lsscsi, udev, curl)."
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
        # Check if .env already explicitly selected one of the discovered drives
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
        # 0 drives detected via sysfs
        if [[ "$EXPLICIT_OPTICAL_SR" -eq 1 && "$EXPLICIT_OPTICAL_SG" -eq 1 && -n "${OPTICAL_SR_DEVICE:-}" && -n "${OPTICAL_SG_DEVICE:-}" ]]; then
            log_warn "No physical optical drive detected in ${SYSFS_BLOCK_DIR}/sr*, using explicit override from .env (${OPTICAL_SR_DEVICE} + ${OPTICAL_SG_DEVICE})."
        else
            log_error "No physical optical drive detected in ${SYSFS_BLOCK_DIR}/sr* (with matching /device/scsi_generic/sg*)."
            log_error "Please connect a DVD drive to this host (or explicitly set OPTICAL_SR_DEVICE and OPTICAL_SG_DEVICE in ${ENV_FILE}) and re-run ./install.sh."
            return 1
        fi
    fi

    # Detect CDROM_GID and verify device node permissions
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
    # Track explicit environment overrides passed on invocation
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
        # Strip leading/trailing whitespace
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [[ -z "$line" || "$line" == \#* ]] && continue

        if [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
            key="${BASH_REMATCH[1]}"
            val="${BASH_REMATCH[2]}"

            # Strip matching surrounding double or single quotes
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
                CDROM_GID|NFS_SERVER|NFS_SERVER_IP|NFS_PATH|NFS_OPTS|SCRATCH_DIR|TMDB_API_KEY|WEB_PORT|WEBHOOK_URL)
                    if [[ -z "${!key:-}" ]]; then
                        printf -v "$key" '%s' "$val"
                    fi
                    ;;
            esac
        fi
    done < "$ENV_FILE"

    # If .env still has the placeholder TMDB_API_KEY from .env.example, treat it as an
    # unconfigured template copy so hardware & PUID/PGID are auto-detected and prompts run.
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

    # Treat placeholder TMDB key from .env.example as unset
    if [[ "${TMDB_API_KEY:-}" == "your_tmdb_api_key_here" ]]; then
        TMDB_API_KEY=""
    fi

    prompt_value "NFS_SERVER" "Enter NAS NFS server hostname or IP (e.g., nas.local or 192.168.1.50)" "" 1
    prompt_value "NFS_PATH" "Enter exported NAS media path (e.g., /volume1/media)" "" 1

    # Normalize NFS_PATH (must start with /, strip trailing / unless root /)
    if [[ "${NFS_PATH}" != /* ]]; then
        log_error "NFS_PATH ('${NFS_PATH}') must be an absolute path starting with '/'."
        return 1
    fi
    if [[ "${#NFS_PATH}" -gt 1 ]]; then
        NFS_PATH="${NFS_PATH%/}"
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
# 6. NFS Server Resolution, Version Probing, PUID/PGID & Write Verification
# ------------------------------------------------------------------------------
verify_and_init_nfs() {
    log_info "Resolving NFS_SERVER '${NFS_SERVER}' to IPv4 address via getent ahostsv4..."

    NFS_SERVER_IP=$(getent ahostsv4 "$NFS_SERVER" 2>/dev/null | awk 'NR==1 {print $1}')
    if [[ -z "$NFS_SERVER_IP" ]]; then
        log_error "Failed to resolve IPv4 address for NFS_SERVER '${NFS_SERVER}' via 'getent ahostsv4'."
        log_error "Verify DNS/hosts configuration or specify the NAS IPv4 address directly in NFS_SERVER."
        return 1
    fi
    log_ok "Resolved ${NFS_SERVER} -> ${NFS_SERVER_IP}"

    TMP_MNT=$(mktemp -d /tmp/hometheatre-nfs-test.XXXXXX)

    log_info "Probing NFS mount to ${NFS_SERVER_IP}:${NFS_PATH} (trying nfsvers=4 first)..."
    if mount -t nfs -o "addr=${NFS_SERVER_IP},${NFS_OPTS_V4}" "${NFS_SERVER_IP}:${NFS_PATH}" "$TMP_MNT" 2>/dev/null; then
        NFS_OPTS="$NFS_OPTS_V4"
        log_ok "Mounted ${NFS_SERVER_IP}:${NFS_PATH} using NFSv4 (${NFS_OPTS})."
    else
        log_warn "NFSv4 mount probe failed; falling back to NFSv3 (nfsvers=3,nolock)..."
        if mount -t nfs -o "addr=${NFS_SERVER_IP},${NFS_OPTS_V3}" "${NFS_SERVER_IP}:${NFS_PATH}" "$TMP_MNT" 2>/dev/null; then
            NFS_OPTS="$NFS_OPTS_V3"
            log_ok "Mounted ${NFS_SERVER_IP}:${NFS_PATH} using NFSv3 fallback (${NFS_OPTS})."
        else
            cleanup_tmp_mount
            log_error "Failed to mount NFS share '${NFS_SERVER_IP}:${NFS_PATH}' with both nfsvers=4 and nfsvers=3,nolock."
            log_error "Please verify:"
            log_error "  1. The export path '${NFS_PATH}' exists and is exported on ${NFS_SERVER} (${NFS_SERVER_IP})."
            log_error "  2. This Docker host's IP/subnet is permitted in the NAS NFS export ACL."
            log_error "  3. Firewall rules allow TCP/UDP 2049 (and port 111/rpcbind for NFSv3)."
            return 1
        fi
    fi

    # Inspect share root UID/GID ownership
    local share_uid share_gid
    share_uid=$(stat -c '%u' "$TMP_MNT" 2>/dev/null || echo "0")
    share_gid=$(stat -c '%g' "$TMP_MNT" 2>/dev/null || echo "0")

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

    log_info "Testing root_squash-compatible write access and initializing directories as PUID=${PUID}, PGID=${PGID} (share owner is ${share_uid}:${share_gid})..."

    if ! setpriv --reuid="$PUID" --regid="$PGID" --clear-groups \
        bash -c "mkdir -p '$TMP_MNT/movies' '$TMP_MNT/tv' '$TMP_MNT/unidentified' && \
                 touch '$TMP_MNT/.hometheatre_write_test' && \
                 rm -f '$TMP_MNT/.hometheatre_write_test'" 2>/dev/null; then
        cleanup_tmp_mount
        log_error "Write test failed (EACCES) on '${NFS_SERVER}:${NFS_PATH}' as UID:GID ${PUID}:${PGID}."
        log_error "How to fix on your NAS:"
        log_error "  - Ensure the NFS export '${NFS_PATH}' grants Read/Write (rw) access to this host."
        log_error "  - Ensure filesystem permissions on '${NFS_PATH}' allow writes by UID:GID ${PUID}:${PGID} (detected share owner: ${share_uid}:${share_gid})."
        log_error "  - If using Squash All Users / Map to Guest on Synology/QNAP/TrueNAS, ensure the mapped guest user has write permissions, or set PUID=${share_uid} PGID=${share_gid} in ${ENV_FILE}."
        return 1
    fi

    cleanup_tmp_mount
    log_ok "Verified NFS write access (PUID=${PUID}, PGID=${PGID}) and initialized movies/, tv/, and unidentified/ directories."
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
# 8. Write .env & Deploy Docker Compose Stack (Recreating NFS Volume on Change)
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

# NAS NFS Configuration
NFS_SERVER=${NFS_SERVER}
NFS_SERVER_IP=${NFS_SERVER_IP}
NFS_PATH=${NFS_PATH}
NFS_OPTS=${NFS_OPTS}

# NFS File Ownership (auto-detected from NFS share by install.sh)
PUID=${PUID}
PGID=${PGID}

# Local Scratch Storage (>= 25 GiB free space required)
SCRATCH_DIR=${SCRATCH_DIR}

# Metadata & Web UI
TMDB_API_KEY=${TMDB_API_KEY}
WEB_PORT=${WEB_PORT}
WEBHOOK_URL=${WEBHOOK_URL:-}
EOF

    chmod 0600 "$tmp_env"
    mv "$tmp_env" "$ENV_FILE"
    log_ok "Saved configuration to ${ENV_FILE}."
}

deploy_compose_stack() {
    log_info "Checking Docker named volume 'hometheatre_nfs_media'..."

    local expected_device=":${NFS_PATH}"
    local expected_opts="addr=${NFS_SERVER_IP},${NFS_OPTS}"

    if docker volume inspect hometheatre_nfs_media >/dev/null 2>&1; then
        local current_device current_opts
        current_device=$(docker volume inspect --format '{{index .Options "device"}}' hometheatre_nfs_media 2>/dev/null || true)
        current_opts=$(docker volume inspect --format '{{index .Options "o"}}' hometheatre_nfs_media 2>/dev/null || true)

        if [[ "$current_device" != "$expected_device" || "$current_opts" != "$expected_opts" ]]; then
            log_info "NFS volume configuration changed (device: '${current_device}' -> '${expected_device}', opts: '${current_opts}' -> '${expected_opts}')."
            log_info "Stopping container and recreating local 'hometheatre_nfs_media' volume wrapper..."
            docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" down
            docker volume rm -f hometheatre_nfs_media >/dev/null
            log_ok "Removed stale 'hometheatre_nfs_media' volume metadata."
        else
            log_ok "Existing 'hometheatre_nfs_media' volume matches current NFS configuration."
        fi
    fi

    log_info "Building and starting the Home Theatre Docker Compose stack..."
    docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up -d --build
    log_ok "Home Theatre container is up and running."
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
Home Theatre Installation Complete!
================================================================================

1. Local Web UI & Unidentified Disc Tagging Queue:
   - URL:            http://${host_ip}:${WEB_PORT}
   - Local Fallback: http://localhost:${WEB_PORT}

2. Hardware & Storage Configuration:
   - Optical Drive:  ${OPTICAL_SR_DEVICE} -> /dev/sr0 (${OPTICAL_SG_DEVICE}, gid=${CDROM_GID})
   - Scratch Disk:   ${SCRATCH_DIR}
   - NAS NFS Share:  ${NFS_SERVER} (${NFS_SERVER_IP}):${NFS_PATH} (PUID=${PUID}, PGID=${PGID})

3. Kodi Setup Instructions (Google TV & Tablet):
   In Kodi, go to Settings -> Media -> Library -> Videos -> Add videos...
   Add the following two NFS network sources directly:

   - Movies Source:   nfs://${NFS_SERVER}${NFS_PATH}/movies
     (Set content: "Movies", Scraper: "Local information only" or "TMDB")

   - TV Shows Source: nfs://${NFS_SERVER}${NFS_PATH}/tv
     (Set content: "TV shows", Scraper: "Local information only" or "TMDB TV Shows")

   IMPORTANT — NAS 'insecure' Export Requirement for Kodi on Google TV / Android:
   Standalone Kodi apps on Google TV and Android tablets use userspace libnfs,
   which connects from unprivileged source ports (> 1024). Ensure your NAS NFS
   export for '${NFS_PATH}' enables the 'insecure' option (e.g., 'insecure' in
   Linux /etc/exports, "Allow non-root mount" on TrueNAS, or default non-root
   port access on Synology/QNAP) so Kodi does not receive Permission Denied.
================================================================================
EOF
}

main() {
    parse_args "$@"
    check_root
    check_os_release
    load_env_file
    install_prerequisites
    setup_sg_module
    configure_optical_drive
    prompt_and_validate_config
    verify_scratch_dir
    verify_and_init_nfs
    validate_tmdb_api_key
    write_env_file
    deploy_compose_stack
    print_summary
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
