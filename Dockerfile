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
        util-linux && \
    rm -rf /var/lib/apt/lists/*

# Install pre-built libdvdcss2 package from Stage 2
COPY --from=dvdcss-builder /usr/src/libdvd-pkg/libdvdcss2_*.deb /tmp/
RUN dpkg -i /tmp/libdvdcss2_*.deb && rm -f /tmp/libdvdcss2_*.deb

# Copy static Go binary from Stage 1
COPY --from=go-builder /out/hometheatre /usr/local/bin/hometheatre

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/hometheatre"]
