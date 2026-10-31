# syntax=docker/dockerfile:1
# Desktop sandbox images for the MCP + WebSocket viewer demo.
#
#   x11  Xvfb + openbox + desktop-gw                 (viewer only, no browser)
#   base x11 + Chrome (amd64) / Chromium (arm64)       (viewer, no MCP)
#   cdm  base + Node + chrome-devtools-mcp            (browser-scoped MCP)
#   cua  base + cua-driver                            (desktop-wide MCP)
#
# Only desktop-gw listens on a non-loopback port (8080). Chrome's DevTools port
# and every MCP server stay on loopback or stdio inside the sandbox.

FROM golang:1.27-alpine AS gw
WORKDIR /src
COPY desktop-gw/go.mod desktop-gw/go.sum ./
RUN go mod download
COPY desktop-gw/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/desktop-gw .

FROM ubuntu:24.04 AS x11
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      xvfb openbox fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*
COPY --from=gw /out/desktop-gw /usr/local/bin/desktop-gw
COPY start-desktop.sh /usr/local/bin/start-desktop.sh
RUN chmod 755 /usr/local/bin/start-desktop.sh && mkdir -p /data && chown 65532:65532 /data
ENV HOME=/data DISPLAY=:0 GW_LISTEN=0.0.0.0:8080
USER 65532:65532
EXPOSE 8080
CMD ["start-desktop.sh"]

# Google Chrome ships Linux builds for amd64 only; arm64 gets Chromium from the
# xtradeb PPA (Ubuntu's chromium is a snap). Both signing keys are pinned.
FROM x11 AS base
USER root
ARG TARGETARCH
ARG GOOGLE_KEY_FPR=EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796
ARG XTRADEB_KEY_FPR=5301FA4FD93244FBC6F6149982BB6851C64F6880
RUN set -eux; \
    apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl gnupg fonts-liberation; \
    case "$TARGETARCH" in \
      amd64) url=https://dl.google.com/linux/linux_signing_key.pub; fpr=$GOOGLE_KEY_FPR; bin=/usr/bin/google-chrome-stable; \
             repo="deb [arch=amd64 signed-by=/usr/share/keyrings/browser.gpg] https://dl.google.com/linux/chrome/deb/ stable main"; pkg=google-chrome-stable ;; \
      arm64) url="https://keyserver.ubuntu.com/pks/lookup?op=get&options=mr&search=0x$XTRADEB_KEY_FPR"; fpr=$XTRADEB_KEY_FPR; bin=/usr/bin/chromium; \
             repo="deb [arch=arm64 signed-by=/usr/share/keyrings/browser.gpg] https://ppa.launchpadcontent.net/xtradeb/apps/ubuntu noble main"; pkg=chromium ;; \
    esac; \
    curl -fsSL "$url" | gpg --dearmor >/usr/share/keyrings/browser.gpg; \
    gpg --show-keys --with-colons /usr/share/keyrings/browser.gpg | grep -q "^fpr:::::::::$fpr:$"; \
    echo "$repo" >/etc/apt/sources.list.d/browser.list; \
    apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$pkg"; \
    ln -s "$bin" /usr/local/bin/browser; \
    rm -f /etc/apt/sources.list.d/google-chrome.list /etc/apt/sources.list.d/google-chrome.sources; \
    apt-get purge -y gnupg curl; apt-get autoremove -y; rm -rf /var/lib/apt/lists/*
USER 65532:65532

FROM node:22-bookworm-slim AS cdm-build
ARG CDM_VERSION=1.12.0
RUN npm install --omit=dev --no-audit --no-fund --prefix /opt/cdm "chrome-devtools-mcp@${CDM_VERSION}" \
    && npm cache clean --force

FROM base AS cdm
COPY --from=cdm-build /usr/local/bin/node /usr/local/bin/node
COPY --from=cdm-build /opt/cdm /opt/cdm
ENV CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 \
    CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS=1 \
    GW_MCP_CMD="exec node /opt/cdm/node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js --browser-url=http://127.0.0.1:9222 --no-usage-statistics --no-performance-crux"

FROM alpine:3.22 AS cua-fetch
ARG TARGETARCH
ARG CUA_VERSION=0.38.0
ARG CUA_SHA256_ARM64=80c9ae66bc7322052dd426c6133c96086b33894bf06c972bca8215384400b33d
ARG CUA_SHA256_AMD64=31ab9709dd91ce73677e49b9f3e75c6f0d3e4d9625c286c9e58262bfbea698dd
RUN set -eux; apk add --no-cache curl binutils; \
    case "$TARGETARCH" in arm64) a=linux-arm64; sum=$CUA_SHA256_ARM64 ;; amd64) a=linux-x86_64; sum=$CUA_SHA256_AMD64 ;; esac; \
    curl -fsSL -o /tmp/cua.tgz "https://github.com/trycua/cua/releases/download/cua-driver-rs-v${CUA_VERSION}/cua-driver-rs-${CUA_VERSION}-${a}-binary.tar.gz"; \
    echo "$sum  /tmp/cua.tgz" | sha256sum -c -; \
    mkdir /out; tar -xzf /tmp/cua.tgz -C /out cua-driver; strip /out/cua-driver

FROM base AS cua
USER root
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libx11-6 libxi6 libxkbcommon0 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=cua-fetch /out/cua-driver /usr/local/bin/cua-driver
USER 65532:65532
ENV CUA_DRIVER_RS_TELEMETRY_ENABLED=0 \
    CUA_TELEMETRY_ENABLED=0 \
    GW_MCP_CMD="exec cua-driver mcp"
