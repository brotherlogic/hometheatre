# ==============================================================================
# Stage 1: Build Go Daemon & Embedded Web UI
# ==============================================================================
FROM golang:1.22-bookworm AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hometheatre ./cmd/hometheatre

# ==============================================================================
# Stage 2: Build libdvdcss2 .deb Non-Interactively via Debian contrib libdvd-pkg
# ==============================================================================
FROM debian:bookworm-slim AS dvdcss-builder
ENV DEBIAN_FRONTEND=noninteractive
RUN sed -i 's/Components: main/Components: main contrib/g' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        libdvd-pkg && \
    dpkg-reconfigure -f noninteractive libdvd-pkg && \
    for deb in /usr/src/libdvd-pkg/libdvdcss2_*.deb; do \
        dpkg-deb -R "$deb" /tmp/libdvdcss2-repack && \
        sed -i -E 's/(^Depends:.*)libdvd-pkg, */\1/; s/(^Depends:.*), *libdvd-pkg/\1/; s/^Depends: *libdvd-pkg$/Depends:/' /tmp/libdvdcss2-repack/DEBIAN/control && \
        dpkg-deb -b /tmp/libdvdcss2-repack "$deb" && \
        rm -rf /tmp/libdvdcss2-repack && \
        ! dpkg-deb -f "$deb" Depends | grep -q libdvd-pkg; \
    done && \
    ls -la /usr/src/libdvd-pkg/libdvdcss2_*.deb

# ==============================================================================
# Stage 3: Minimal Runtime Image
# ==============================================================================
FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive

# Install runtime CLI dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        dvdbackup \
        eject \
        gddrescue \
        genisoimage \
        libdvdread8 \
        lsdvd \
        procps \
        util-linux && \
    rm -rf /var/lib/apt/lists/*

# Install pre-built libdvdcss2 package from Stage 2
COPY --from=dvdcss-builder /usr/src/libdvd-pkg/libdvdcss2_*.deb /tmp/
RUN dpkg -i /tmp/libdvdcss2_*.deb && rm -f /tmp/libdvdcss2_*.deb

# Copy static Go binary from Stage 1
COPY --from=go-builder /out/hometheatre /usr/local/bin/hometheatre

# Create idempotent DevPod background daemon launcher with auto-restart loop
RUN cat <<'EOF' > /usr/local/bin/start-daemon.sh && chmod 0755 /usr/local/bin/start-daemon.sh
#!/usr/bin/env bash
set -euo pipefail

if pgrep -x hometheatre >/dev/null 2>&1; then
    exit 0
fi

export OPTICAL_DEVICE="${CONTAINER_OPTICAL_DEVICE:-/dev/sr0}"
export SCRATCH_DIR="${CONTAINER_SCRATCH_DIR:-/scratch}"
export NFS_ROOT="${CONTAINER_NFS_ROOT:-/mnt/nfs}"
export DVDCSS_METHOD="${DVDCSS_METHOD:-key}"
export DVDCSS_CACHE="${DVDCSS_CACHE:-/scratch/.dvdcss}"
export DVDCSS_VERBOSE="${DVDCSS_VERBOSE:-1}"

nohup bash -c '
    while true; do
        /usr/local/bin/hometheatre >> /var/log/hometheatre.log 2>&1 || true
        sleep 2
    done
' >/dev/null 2>&1 &
EOF

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/hometheatre"]
