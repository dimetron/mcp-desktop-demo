#!/usr/bin/env bash
# End-to-end harness for the MCP + WebSocket desktop demo.
#
# Each variant is one sandbox whose only listener is desktop-gw:
#   cdm   chrome-devtools-mcp behind /mcp     -> http://127.0.0.1:18081
#   cua   cua-driver behind /mcp              -> http://127.0.0.1:18082
#   mtls  viewer only, TLS 1.3 + client certs -> https://127.0.0.1:18443
#
# RUNTIME=runc   (default) uses the local Docker engine.
# RUNTIME=gvisor runs the sandboxes with runsc inside a privileged
#                Docker-in-Docker host (mcpd-gvisor), like a Substrate worker.
#
# expect() evals single-quoted snippets, so variables expand late on purpose.
# shellcheck disable=SC2016,SC2034
set -euo pipefail

cd "$(dirname "$0")"
RUNTIME=${RUNTIME:-runc}
: "${GW_TOKEN:=demo-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')}"
: "${BROWSER_URL:=https://github.com/agent-substrate/substrate/blob/main/README.md}"
: "${GEOMETRY:=1280x800x24}"
GVISOR_IMAGE=mcp-desktop-gvisor-host:demo
GVISOR_DOCKER_PORT=${GVISOR_DOCKER_PORT:-23751}
OUT=$(pwd)/out
CERTS=$OUT/certs
TOKEN_FILE=$OUT/gw-token

port()  { case $1 in cdm) echo 18081 ;; cua) echo 18082 ;; mtls) echo 18443 ;; esac; }
image() { case $1 in mtls) echo mcp-desktop:x11 ;; *) echo "mcp-desktop:$1" ;; esac; }

case "$RUNTIME" in
  runc)   D=(docker); RUN_ARGS=(); BIND=127.0.0.1: ;;
  gvisor)
    D=(docker -H "tcp://127.0.0.1:$GVISOR_DOCKER_PORT")
    # gVisor's netstack bypasses Docker's embedded DNS (127.0.0.11).
    RUN_ARGS=(--runtime=runsc -v /etc/sandbox-resolv.conf:/etc/resolv.conf:ro)
    BIND= ;;
  *) echo "RUNTIME must be runc or gvisor" >&2; exit 2 ;;
esac

log()  { printf '==> %s\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
# expect DESC CMD...: runs CMD (a shell snippet) and reports PASS or exits on FAIL.
expect() { local d=$1; shift; if eval "$*"; then pass "$d"; else fail "$d"; fi; }
token() {
  if [ ! -s "$TOKEN_FILE" ]; then mkdir -p "$OUT"; printf %s "$GW_TOKEN" >"$TOKEN_FILE"; fi
  cat "$TOKEN_FILE"
}

build() {
  log "building images"
  for t in x11 base cdm cua; do docker build -q --target "$t" -t "mcp-desktop:$t" . >/dev/null; done
  [ "$RUNTIME" = gvisor ] && gvisor_host
  return 0
}

gvisor_host() {
  log "starting gVisor host mcpd-gvisor"
  docker build -q -f Dockerfile.gvisor -t "$GVISOR_IMAGE" . >/dev/null
  docker rm -f mcpd-gvisor >/dev/null 2>&1 || true
  mkdir -p "$OUT"
  # Privileged only for this local stand-in for a Substrate worker.
  docker run -d --privileged --name mcpd-gvisor -e DOCKER_TLS_CERTDIR= \
    -p "127.0.0.1:$GVISOR_DOCKER_PORT:2375" \
    -p 127.0.0.1:18081:18081 -p 127.0.0.1:18082:18082 -p 127.0.0.1:18443:18443 \
    -v "$OUT:$OUT" "$GVISOR_IMAGE" >/dev/null
  for _ in $(seq 60); do "${D[@]}" info >/dev/null 2>&1 && break; sleep 1; done
  "${D[@]}" info --format '{{json .Runtimes}}' | grep -q runsc || fail "runsc not registered"
  docker exec mcpd-gvisor sh -c 'grep ^nameserver /etc/resolv.conf >/etc/sandbox-resolv.conf'
  log "loading images into mcpd-gvisor"
  docker save mcp-desktop:x11 mcp-desktop:cdm mcp-desktop:cua | "${D[@]}" load -q >/dev/null
}

certs() {
  [ -s "$CERTS/client.pem" ] && return 0
  log "generating demo CA, server and client certificates in $CERTS"
  mkdir -p "$CERTS"; cd "$CERTS"
  openssl req -x509 -newkey rsa:2048 -nodes -days 7 -subj /CN=desktop-demo-ca \
    -keyout ca.key -out ca.pem 2>/dev/null
  printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' >server.ext
  printf 'extendedKeyUsage=clientAuth\n' >client.ext
  for n in server client; do
    openssl req -newkey rsa:2048 -nodes -subj "/CN=desktop-demo-$n" -keyout "$n.key" -out "$n.csr" 2>/dev/null
    openssl x509 -req -in "$n.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -days 7 \
      -extfile "$n.ext" -out "$n.pem" 2>/dev/null
  done
  # The sandbox user (65532) reads the mounted key; demo-only permissions.
  chmod 644 server.key client.key
  rm -f ./*.csr ./*.ext ca.srl
  cd - >/dev/null
}

start() {
  local v=$1 name="mcpd-$1" args=()
  "${D[@]}" rm -f "$name" >/dev/null 2>&1 || true
  args=(-e GW_TOKEN="$(token)" -e GEOMETRY="$GEOMETRY" -e BROWSER_URL="$BROWSER_URL" --shm-size=1g)
  if [ "$v" = mtls ]; then
    certs
    args+=(-v "$CERTS:/certs:ro" -e GW_LISTEN=0.0.0.0:8443 -e GW_TLS_CERT=/certs/server.pem
           -e GW_TLS_KEY=/certs/server.key -e GW_TLS_CLIENT_CA=/certs/ca.pem
           -p "${BIND}$(port "$v"):8443")
  else
    args+=(-p "${BIND}$(port "$v"):8080")
  fi
  log "starting $name ($(image "$v"), runtime $RUNTIME)"
  "${D[@]}" run -d --name "$name" ${RUN_ARGS[@]+"${RUN_ARGS[@]}"} "${args[@]}" "$(image "$v")" >/dev/null
  local scheme=http curlargs=()
  [ "$v" = mtls ] && { scheme=https; curlargs=(--cacert "$CERTS/ca.pem" --cert "$CERTS/client.pem" --key "$CERTS/client.key"); }
  for _ in $(seq 60); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' ${curlargs[@]+"${curlargs[@]}"} "$scheme://127.0.0.1:$(port "$v")/healthz")" = 204 ] && break
    sleep 1
  done
  if [ "$RUNTIME" = gvisor ]; then
    "${D[@]}" exec "$name" dmesg 2>/dev/null | grep -q 'Starting gVisor' || fail "$name is not under gVisor"
  fi
}

# mcp PORT SESSION JSON -> response body; prints the session id for initialize.
mcp() {
  curl -sS -H "Authorization: Bearer $(token)" ${2:+-H "Mcp-Session-Id: $2"} \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    "http://127.0.0.1:$1/mcp" -d "$3"
}
mcp_init() {
  curl -sS -D - -o /dev/null -H "Authorization: Bearer $(token)" -H 'Content-Type: application/json' \
    "http://127.0.0.1:$1/mcp" -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",
      "capabilities":{"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]}}},
      "clientInfo":{"name":"e2e","version":"0"}}}' | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}'
}
# jq-free assertions: py EXPR evaluates with r = parsed JSON from stdin.
py() { python3 -c "import json,sys; r=json.load(sys.stdin); v=($1); print(v) if v not in (True,None) else None; sys.exit(0 if v else 1)"; }

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

test_variant() {
  local v=$1 p s body tk
  p=$(port "$v")
  log "testing $v on :$p"
  expect "unauthenticated /mcp -> 401" '[ "$(code "http://127.0.0.1:$p/mcp" -d "{}")" = 401 ]'
  expect "unauthenticated /ws/screen -> 401" '[ "$(code "http://127.0.0.1:$p/ws/screen")" = 401 ]'

  s=$(mcp_init "$p"); [ -n "$s" ] || fail "initialize returned no session"
  mcp "$p" "$s" '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
  expect "tools/list includes desktop_view (MCP App)" \
    'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}" | py "\"desktop_view\" in [t[\"name\"] for t in r[\"result\"][\"tools\"]]"'
  expect "resources/read ui://desktop-gw/viewer is an MCP App" \
    'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"resources/read\",\"params\":{\"uri\":\"ui://desktop-gw/viewer\"}}" | py "r[\"result\"][\"contents\"][0][\"mimeType\"]==\"text/html;profile=mcp-app\""'
  body=$(mcp "$p" "$s" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"desktop_view","arguments":{"control":false}}}')
  tk=$(printf %s "$body" | py 'r["result"]["_meta"]["dev.kagent/desktop"]["ticket"]') || fail "desktop_view: $body"
  pass "desktop_view returned a viewer ticket in _meta"
  expect "ticket opens a view-only stream" \
    '"${D[@]}" exec "mcpd-$v" desktop-gw probe -token= -url "ws://127.0.0.1:8080/ws/screen?ticket=$tk" | grep -q control=false'
  expect "ticket is single-use" \
    '! "${D[@]}" exec "mcpd-$v" desktop-gw probe -token= -url "ws://127.0.0.1:8080/ws/screen?ticket=$tk" >/dev/null 2>&1'

  case "$v" in
    cdm)
      expect "navigate_page (chrome-devtools-mcp)" \
        'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"navigate_page\",\"arguments\":{\"pageId\":1,\"type\":\"url\",\"url\":\"data:text/html,<h1 style=font-size:96px>kagent MCP e2e</h1>\"}}}" | py "not r[\"result\"].get(\"isError\")"'
      expect "take_screenshot returns an image" \
        'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":6,\"method\":\"tools/call\",\"params\":{\"name\":\"take_screenshot\",\"arguments\":{\"pageId\":1,\"format\":\"jpeg\",\"quality\":60}}}" | py "any(c[\"type\"]==\"image\" for c in r[\"result\"][\"content\"])"' ;;
    cua)
      expect "list_windows (cua-driver)" \
        'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"list_windows\",\"arguments\":{}}}" | py "\"window_id=\" in r[\"result\"][\"content\"][0][\"text\"]"'
      expect "get_desktop_state returns a screenshot" \
        'mcp "$p" "$s" "{\"jsonrpc\":\"2.0\",\"id\":6,\"method\":\"tools/call\",\"params\":{\"name\":\"get_desktop_state\",\"arguments\":{}}}" | py "any(c[\"type\"]==\"image\" for c in r[\"result\"][\"content\"])"' ;;
  esac

  expect "bearer-token stream with input control" \
    '"${D[@]}" exec "mcpd-$v" desktop-gw probe -url ws://127.0.0.1:8080/ws/screen -out /tmp/frame.jpg | grep -q control=true'
  # docker cp cannot read gVisor's in-memory rootfs overlay; stream it instead.
  expect "frame saved to out/frame-$RUNTIME-$v.jpg" \
    '"${D[@]}" exec "mcpd-$v" cat /tmp/frame.jpg >"$OUT/frame-$RUNTIME-$v.jpg" && [ -s "$OUT/frame-$RUNTIME-$v.jpg" ]'
  expect "DELETE /mcp ends the session" \
    '[ "$(code -X DELETE -H "Authorization: Bearer $(token)" -H "Mcp-Session-Id: $s" "http://127.0.0.1:$p/mcp")" = 204 ]'
}

test_mtls() {
  local u tls
  u=https://127.0.0.1:$(port mtls)
  tls=(--cacert "$CERTS/ca.pem" --cert "$CERTS/client.pem" --key "$CERTS/client.key")
  log "testing mtls on ${u#https://}"
  expect "no client certificate -> rejected" '! curl -s -o /dev/null --cacert "$CERTS/ca.pem" "$u/healthz"'
  expect "client certificate -> /healthz 204" '[ "$(code "${tls[@]}" "$u/healthz")" = 204 ]'
  expect "client certificate alone -> /ticket 401 (token still required)" '[ "$(code "${tls[@]}" -X POST "$u/ticket")" = 401 ]'
  expect "wss:// probe with mTLS" \
    '"${D[@]}" exec mcpd-mtls desktop-gw probe -url wss://127.0.0.1:8443/ws/screen -cacert /certs/ca.pem -cert /certs/client.pem -key /certs/client.key | grep -q ^ok'
  "${D[@]}" exec -d mcpd-mtls desktop-gw proxy -listen 127.0.0.1:9443 -upstream https://127.0.0.1:8443 \
    -cacert /certs/ca.pem -cert /certs/client.pem -key /certs/client.key
  sleep 1
  expect "gateway-leg proxy (ws -> mTLS wss)" \
    '"${D[@]}" exec mcpd-mtls desktop-gw probe -url ws://127.0.0.1:9443/ws/screen | grep -q ^ok'
}

stats() {
  log "image sizes"
  for t in x11 base cdm cua; do
    printf '  %-5s %6s MB\n' "$t" $(( $(docker image inspect "mcp-desktop:$t" --format '{{.Size}}') / 1000000 ))
  done
  log "memory (runtime $RUNTIME, after the tests above)"
  for v in cdm cua mtls; do
    "${D[@]}" inspect "mcpd-$v" >/dev/null 2>&1 || continue
    printf '  %s: ' "mcpd-$v"
    "${D[@]}" stats --no-stream --format '{{.MemUsage}}' "mcpd-$v" 2>/dev/null || echo "n/a"
    "${D[@]}" exec "mcpd-$v" ps -eo rss=,comm= | awk '{s[$2]+=$1} END{for (k in s) if (k!="ps") printf "    %-16s %7.1f MB\n", k, s[k]/1024}' | sort -k2 -nr
  done
}

info() {
  cat <<EOF

Token: $(token)
  Viewer (cdm) : http://127.0.0.1:$(port cdm)/#token=$(token)
  Viewer (cua) : http://127.0.0.1:$(port cua)/#token=$(token)
  MCP App host : http://127.0.0.1:$(port cdm)/host-demo#token=$(token)
  MCP endpoint : http://127.0.0.1:$(port cdm)/mcp  (Authorization: Bearer <token>)
EOF
}

down() {
  log "removing demo containers"
  for engine in "docker" "docker -H tcp://127.0.0.1:$GVISOR_DOCKER_PORT"; do
    $engine info >/dev/null 2>&1 || continue
    $engine rm -f mcpd-cdm mcpd-cua mcpd-mtls >/dev/null 2>&1 || true
  done
  docker rm -f mcpd-gvisor >/dev/null 2>&1 || true
}

up() { for v in cdm cua mtls; do start "$v"; done; }
tests() { test_variant cdm; test_variant cua; test_mtls; }

case "${1:-}" in
  build) build ;;
  up) up; info ;;
  test) tests ;;
  stats) stats ;;
  all) down; build; up; tests; stats; info ;;
  info) info ;;
  logs) "${D[@]}" logs --tail 30 "mcpd-${2:-cdm}" ;;
  down) down ;;
  *) echo "usage: [RUNTIME=runc|gvisor] $0 {all|build|up|test|stats|info|logs [cdm|cua|mtls]|down}" >&2; exit 2 ;;
esac
