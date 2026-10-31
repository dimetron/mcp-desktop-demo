#!/usr/bin/env bash
# Kubernetes smoke test for deploy/: a kind cluster with kagent's CRDs, the
# rendered manifests, and MCP calls through the Service. No kagent controller
# runs, so the RemoteMCPServer and SandboxTemplate are only admission-checked
# (schema + CEL); the Deployment is exercised end to end.
set -euo pipefail

cd "$(dirname "$0")"
CLUSTER=${CLUSTER:-mcp-desktop-e2e}
NAMESPACE=${NAMESPACE:-kagent}
KAGENT_REF=${KAGENT_REF:-main}
CRDS_URL=https://raw.githubusercontent.com/kagent-dev/kagent/$KAGENT_REF/helm/kagent-crds/templates
BASE_IMAGE=ghcr.io/dimetron/mcp-desktop
LOCAL_PORT=${LOCAL_PORT:-18090}
TOKEN=kind-e2e-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
OUT=$(pwd)/out
export KUBECONFIG=$OUT/kubeconfig

log()  { printf '==> %s\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }

up() {
  mkdir -p "$OUT"
  if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    log "creating kind cluster $CLUSTER"
    kind create cluster --name "$CLUSTER" --wait 120s --quiet
  fi
  docker image inspect mcp-desktop:cua >/dev/null 2>&1 || {
    log "building mcp-desktop:cua"; docker build -q --target cua -t mcp-desktop:cua . >/dev/null; }
  docker tag mcp-desktop:cua "$BASE_IMAGE:cua"
  log "loading $BASE_IMAGE:cua into kind"
  kind load docker-image --name "$CLUSTER" "$BASE_IMAGE:cua" >/dev/null

  log "installing kagent CRDs ($KAGENT_REF)"
  kubectl apply --server-side -f "$CRDS_URL/api.kagent.dev_remotemcpservers.yaml" \
    -f "$CRDS_URL/api.kagent.dev_sandboxtemplates.yaml" >/dev/null
  kubectl wait --for=condition=Established --timeout=60s \
    crd/remotemcpservers.api.kagent.dev crd/sandboxtemplates.api.kagent.dev >/dev/null

  kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  GW_TOKEN=$TOKEN make --no-print-directory deploy-secret NAMESPACE="$NAMESPACE" >/dev/null

  # The local image has no registry digest; pin the SandboxTemplate to its ID
  # (admission only) and run the Deployment from the loaded tag.
  make --no-print-directory manifests NAMESPACE="$NAMESPACE" IMAGE="$BASE_IMAGE" \
    DIGEST="$(docker image inspect mcp-desktop:cua --format '{{.Id}}')" >/dev/null
  sed "/^kind: Deployment/,/^---/ s#image: .*#image: $BASE_IMAGE:cua#" "$OUT/deploy.yaml" \
    | kubectl apply -f - >/dev/null
  pass "manifests admitted (Deployment, Service, RemoteMCPServer, SandboxTemplate)"
  kubectl -n "$NAMESPACE" rollout status deploy/mcp-desktop --timeout=5m >/dev/null || {
    kubectl -n "$NAMESPACE" describe pod -l app.kubernetes.io/name=mcp-desktop | tail -30; fail "rollout"; }
  pass "deploy/mcp-desktop is ready"
}

test_mcp() {
  local u=http://127.0.0.1:$LOCAL_PORT/mcp pf s token
  token=$(kubectl -n "$NAMESPACE" get secret mcp-desktop-auth -o jsonpath='{.data.token}' | base64 -d)
  kubectl -n "$NAMESPACE" port-forward svc/mcp-desktop "$LOCAL_PORT:8080" >/dev/null 2>&1 &
  pf=$!
  trap 'kill "$pf" 2>/dev/null || true' RETURN
  for _ in $(seq 30); do curl -s -o /dev/null "http://127.0.0.1:$LOCAL_PORT/healthz" && break; sleep 1; done

  mcp() {
    curl -sS -H "Authorization: Bearer $token" ${s:+-H "Mcp-Session-Id: $s"} -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' "$u" -d "$1"
  }
  check() { python3 -c "import json,sys; r=json.load(sys.stdin); sys.exit(0 if ($1) else 1)"; }

  [ "$(curl -s -o /dev/null -w '%{http_code}' "$u" -d '{}')" = 401 ] || fail "unauthenticated /mcp"
  pass "unauthenticated /mcp -> 401"
  s=$(curl -sS -D - -o /dev/null -H "Authorization: Bearer $token" -H 'Content-Type: application/json' "$u" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]}}},"clientInfo":{"name":"k8s-e2e","version":"0"}}}' \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}')
  [ -n "$s" ] || fail "initialize"
  pass "initialize with Secret token"
  mcp '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
  mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
    | check '{"desktop_view","list_windows"} <= {t["name"] for t in r["result"]["tools"]}' || fail "tools/list"
  pass "tools/list has cua-driver tools and desktop_view"
  mcp '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_windows","arguments":{}}}' \
    | check '"window_id=" in r["result"]["content"][0]["text"]' || fail "list_windows"
  pass "list_windows sees the browser"
  mcp '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_desktop_state","arguments":{}}}' \
    | check 'any(c["type"]=="image" for c in r["result"]["content"])' || fail "get_desktop_state"
  pass "get_desktop_state returns a screenshot"
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $token" -H "Mcp-Session-Id: $s" "$u")" = 204 ] || fail "DELETE /mcp"
  pass "DELETE /mcp ends the session"
}

down() { log "deleting kind cluster $CLUSTER"; kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }

case "${1:-}" in
  up) up ;;
  test) log "testing MCP through svc/mcp-desktop"; test_mcp ;;
  all) up; log "testing MCP through svc/mcp-desktop"; test_mcp ;;
  down) down ;;
  *) echo "usage: $0 {all|up|test|down}" >&2; exit 2 ;;
esac
