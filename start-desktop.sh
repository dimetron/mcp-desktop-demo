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

# Chrome 136+ only allows remote debugging with a non-default profile dir.
# DevTools binds to loopback so only in-sandbox MCP servers can drive it.
if command -v browser >/dev/null && [ -n "${BROWSER_URL-about:blank}" ]; then
  size=${GEOMETRY%x*}
  browser --no-sandbox --no-first-run --no-default-browser-check --password-store=basic \
    --disable-dev-shm-usage --user-data-dir=/data/chrome \
    --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
    --window-position=0,0 --window-size="${size/x/,}" --force-renderer-accessibility \
    "${BROWSER_URL-about:blank}" >"$logs/browser.log" 2>&1 &
fi

if [ -z "${GW_TOKEN:-}${GW_TOKEN_FILE:-}" ]; then
  [ -s /data/gw-token ] || od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >/data/gw-token
  chmod 600 /data/gw-token
  export GW_TOKEN_FILE=/data/gw-token
fi

exec desktop-gw
