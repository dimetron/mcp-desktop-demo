#!/usr/bin/env bash
# Starts a headless desktop whose only network listener is desktop-gw: the
# HTML viewer, the screen WebSocket and the MCP bridge share one port.
set -euo pipefail

: "${GEOMETRY:=1280x800x24}"
export DISPLAY=:0 HOME="${HOME:-/data}"
logs=/data/desktop-logs
mkdir -p "$logs"

Xvfb :0 -screen 0 "$GEOMETRY" -nolisten tcp >"$logs/xvfb.log" 2>&1 &
for _ in $(seq 50); do [ -S /tmp/.X11-unix/X0 ] && break; sleep 0.1; done
openbox >"$logs/openbox.log" 2>&1 &

# In a kagent sandbox the egress gateway re-signs HTTPS with its own CA,
# mounted at $SSL_CERT_FILE. Chromium on Linux trusts only its root store plus
# the user's NSS database, so import the bundle there on every start (it can
# rotate). Outside a sandbox SSL_CERT_FILE is unset and this does nothing.
if [ -s "${SSL_CERT_FILE:-}" ] && command -v certutil >/dev/null; then
  nssdb="$HOME/.pki/nssdb"
  mkdir -p "$nssdb"
  [ -f "$nssdb/cert9.db" ] || certutil -N -d "sql:$nssdb" --empty-password
  certs=$(mktemp -d)
  awk -v dir="$certs" '/BEGIN CERTIFICATE/ { n++ } n { print > (dir "/" n ".pem") }' "$SSL_CERT_FILE"
  for f in "$certs"/*.pem; do
    name="egress-gateway-$(basename "$f" .pem)"
    certutil -D -d "sql:$nssdb" -n "$name" >/dev/null 2>&1 || true
    certutil -A -d "sql:$nssdb" -n "$name" -t "C,," -i "$f"
  done
  rm -rf "$certs"
fi

# Chrome 136+ only allows remote debugging with a non-default profile dir.
# DevTools binds to loopback so only in-sandbox MCP servers can drive it.
if command -v browser >/dev/null && [ -n "${BROWSER_URL-about:blank}" ]; then
  size=${GEOMETRY%x*}
  # Memory: a kagent sandbox is killed over 1 GiB (controller sandboxes.memory)
  # and /tmp lives in sandbox memory, so cap renderers and the JS heap, keep
  # the disk cache small and skip background services. QUIC (HTTP/3) is off:
  # the sandbox egress gateway carries TCP only, and Cloudflare and Google
  # sites fail with ERR_QUIC_PROTOCOL_ERROR instead of falling back. ECH is
  # off: it hides the real hostname behind a decoy (cloudflare-ech.com), and
  # the gateway allows TLS by hostname, so ECH sites (kagent.dev) are refused.
  # DNS HTTPS records are ignored too: their address hints bypass the
  # sandbox DNS relay, and the gateway only admits addresses it resolved.
  browser --no-sandbox --no-first-run --no-default-browser-check --password-store=basic \
    --disable-dev-shm-usage --user-data-dir=/data/chrome \
    --renderer-process-limit=3 --js-flags=--max-old-space-size=384 --disable-quic \
    --disk-cache-size=67108864 --disable-background-networking --disable-component-update \
    --disable-features=Translate,MediaRouter,OptimizationHints,AutofillServerCommunication,EncryptedClientHello,UseDnsHttpsSvcb,UseDnsHttpsSvcbAlpn \
    --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
    --window-position=0,0 --window-size="${size/x/,}" --force-renderer-accessibility \
    "${BROWSER_URL-about:blank}" >"$logs/browser.log" 2>&1 &
fi

if [ -z "${GW_TOKEN:-}${GW_TOKEN_FILE:-}" ]; then
  [ -s /data/gw-token ] || od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >/data/gw-token
  chmod 600 /data/gw-token
  export GW_TOKEN_FILE=/data/gw-token
fi

# Agent cursor: cua-driver picks the theme per MCP session, so desktop-gw
# sets GW_CURSOR_THEME (the image's small "+") on each new session. The theme
# store is baked into the image.
export CUA_DRIVER_CURSOR_THEME_DIR="${CUA_DRIVER_CURSOR_THEME_DIR:-/usr/share/cua-driver/cursor-themes}"
command -v cua-driver >/dev/null && export GW_CURSOR_THEME="${GW_CURSOR_THEME-${CURSOR_THEME:-dev.pibot.cursor.plus}}"

exec desktop-gw
