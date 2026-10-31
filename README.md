# MCP + web viewer desktop sandbox (demo)

A desktop sandbox with one listener, `desktop-gw` on port 8080:

- **Agents** drive the desktop through MCP at `/mcp`. The MCP server is
  [cua-driver](https://github.com/trycua/cua) or
  [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp).
- **People** watch the screen in a browser over WebSocket at `/`. They can
  take control when their ticket allows it.
- **MCP hosts** can embed the same viewer as an MCP App (`desktop_view` tool,
  `ui://desktop-gw/viewer` resource).

No VNC or RustDesk port is exposed. See [ADR.md](ADR.md) for the decision,
option comparison and measurements.

## Requirements

- Docker (tested with OrbStack on arm64; amd64 uses Google Chrome instead of
  Chromium).
- `curl`, `openssl` and `python3` on the host for `e2e.sh`.
- Go 1.25+ only to run the unit tests outside Docker.
- `kind` and `kubectl` for `make kind-e2e`; `kubectl` for `make deploy`.
- Optional for `make lint`: `shellcheck`, `actionlint`, `kubeconform`.

Run `make help` for every target. The common ones:

| Target | What it does |
| --- | --- |
| `make test` | `go vet` and race tests for `desktop-gw`. |
| `make lint` | shellcheck, actionlint, render and kubeconform `deploy/`. |
| `make e2e` | Docker end-to-end (`RUNTIME=runc` or `gvisor`, or `make e2e-gvisor`). |
| `make kind-e2e` | kind cluster with kagent's CRDs: admit `deploy/`, roll out, test MCP through the Service. |
| `make push-all` | Build and push multi-arch images for every variant. |
| `make deploy` | Render and apply `deploy/` to the current cluster. |
| `make clean` | Remove containers, the kind cluster and `out/`. |

## Run the end-to-end test

```sh
make e2e                      # runc: build, start cdm/cua/mtls, test, stats
make e2e-gvisor               # same, with runsc inside a Docker-in-Docker host
make info                     # print the viewer URLs and token
make down                     # remove all demo containers
```

`make` wraps `e2e.sh`; `./e2e.sh all` and `RUNTIME=gvisor ./e2e.sh all` work
too. Other commands: `build`, `up`, `test`, `stats`, `logs [cdm|cua|mtls]`.

`all` starts three sandboxes:

| Container | Image | URL | What it tests |
| --- | --- | --- | --- |
| `mcpd-cdm` | `mcp-desktop:cdm` | http://127.0.0.1:18081 | chrome-devtools-mcp over `/mcp` |
| `mcpd-cua` | `mcp-desktop:cua` | http://127.0.0.1:18082 | cua-driver over `/mcp` |
| `mcpd-mtls` | `mcp-desktop:x11` | https://127.0.0.1:18443 | TLS 1.3 with client certificates and the `proxy` leg |

The token is generated once into `out/gw-token`. You can set `GW_TOKEN` to
choose one. Demo certificates are written to `out/certs/`. Each run saves one
streamed frame per variant to `out/frame-<runtime>-<variant>.jpg`.

Expected output ends with 29 `PASS` lines, followed by image sizes and memory.

## Use it by hand

After `./e2e.sh up` (or `all`):

```sh
T=$(cat out/gw-token)
open "http://127.0.0.1:18082/#token=$T"            # viewer; click "Take control"
open "http://127.0.0.1:18081/host-demo#token=$T"   # MCP Apps host + agent console
```

The token stays in the URL fragment, which the browser never sends. The page
exchanges it for a single-use ticket (`POST /ticket`), then opens
`/ws/screen?ticket=…`.

Call MCP with curl:

```sh
U=http://127.0.0.1:18082/mcp
S=$(curl -sD - -o /dev/null -H "Authorization: Bearer $T" -H 'Content-Type: application/json' $U \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"me","version":"0"}}}' \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}')
mcp() { curl -s -H "Authorization: Bearer $T" -H "Mcp-Session-Id: $S" -H 'Content-Type: application/json' $U -d "$1"; echo; }
mcp '{"jsonrpc":"2.0","method":"notifications/initialized"}'
mcp '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_windows","arguments":{}}}'
```

To use it from an MCP client such as Claude Desktop, VS Code or Copilot, add a
Streamable HTTP server with URL `http://127.0.0.1:18082/mcp` and header
`Authorization: Bearer <token>`. Hosts that advertise the
`io.modelcontextprotocol/ui` extension also get the `desktop_view` tool, which
renders the live viewer inline.

Run a single sandbox without the harness:

```sh
docker build --target cua -t mcp-desktop:cua .
docker run --rm -p 127.0.0.1:8080:8080 --shm-size=1g -e GW_TOKEN=change-me mcp-desktop:cua
```

## Deploy to kagent

`deploy/` is a kustomize base for namespace `kagent` with two ways to run the
desktop:

| Resource | Use |
| --- | --- |
| `Deployment` + `Service` `mcp-desktop` | A shared, long-running `cua` desktop serving MCP and the viewer on port 8080. |
| `RemoteMCPServer` `mcp-desktop` | Registers that `/mcp` endpoint with kagent. The egress gateway adds the `Authorization` header from Secret `mcp-desktop-auth`. |
| `SandboxTemplate` `mcp-desktop` | A per-session desktop in a Substrate gVisor sandbox (worker pool `kagent-default`). |

Publish the images, then deploy with the image pinned by digest:

```sh
make push-all                     # or let the publish workflow push to GHCR
make deploy                       # renders out/deploy.yaml, creates the Secret, applies
make deploy NAMESPACE=agents WORKER_POOL=my-pool \
  SNAPSHOT_LOCATION=gs://my-bucket/mcp-desktop/ IMAGE=ghcr.io/me/mcp-desktop
make port-forward                 # prints the viewer URL with the token
```

`make manifests` only renders `out/deploy.yaml`. `DIGEST` is read from the
registry for `$(IMAGE):$(IMAGE_TAG)` (default `cua`); set it to deploy a
specific build. `SandboxTemplate` only accepts digest references.

`make deploy-secret` creates `mcp-desktop-auth` once, from `GW_TOKEN` or a
random value. It has two keys: `token`, mounted into the pod as
`GW_TOKEN_FILE`, and `authorization`, the full `Bearer <token>` header value
that kagent injects. To rotate it, delete the Secret, run `make deploy-secret`
and `kubectl -n kagent rollout restart deploy/mcp-desktop`.

Give an agent the desktop tools through its `AgentTemplate`:

```yaml
spec:
  tools:
    - mcp:
        server:
          kind: RemoteMCPServer
          name: mcp-desktop
        tools: [get_desktop_state, list_windows, click, type_text, press_key, scroll]
```

Omit `tools` to expose every cua-driver tool.

### As a sandbox environment

kagent replaces the image entrypoint with its sandbox guest and doesn't forward
ports (see [ADR.md](ADR.md)), so the template binds `desktop-gw` to
`127.0.0.1:8080` and the desktop is started through the process API. Without
`GW_TOKEN`, `start-desktop.sh` writes a token to `/data/gw-token`.

```sh
kagent sandbox create mcp-desktop --request-id demo
kagent sandbox exec <id> --wait=false -- start-desktop.sh
kagent sandbox exec <id> -- sh -c \
  'desktop-gw probe -token "$(cat /data/gw-token)" -out /data/workspace/frame.jpg'
kagent sandbox download <id> /data/workspace/frame.jpg frame.jpg
```

### Kubernetes smoke test

`make kind-e2e` (`k8s-e2e.sh`) needs no kagent controller. It:

1. Creates kind cluster `mcp-desktop-e2e` with kubeconfig `out/kubeconfig`.
2. Installs kagent's `RemoteMCPServer` and `SandboxTemplate` CRDs from
   `KAGENT_REF` (default `main`).
3. Loads the local `cua` image and applies the rendered manifests. This checks
   both CRs against the CRD schemas and CEL rules.
4. Calls MCP through the Service with the Secret's token.

It reuses an existing cluster. `make kind-down` deletes it.

## CI and images

| Workflow | Trigger | Jobs |
| --- | --- | --- |
| `.github/workflows/ci.yaml` | push to `main`, pull requests | `unit`, `lint`, `e2e` (runc and gVisor), `kind` |
| `.github/workflows/publish.yaml` | push to `main`, `v*` tags, manual | Multi-arch `x11`/`cdm`/`cua` images to `ghcr.io/<owner>/mcp-desktop`, then `deploy.yaml` pinned by digest |

Image tags are `<variant>` (default branch), `<variant>-<sha7>` and
`<variant>-<version>` for `v*` tags. The rendered `deploy.yaml` is uploaded as
an artifact and attached to the GitHub release for tags.

## Images

| Target | Adds | MCP server |
| --- | --- | --- |
| `x11` | Ubuntu 24.04, Xvfb, openbox, `desktop-gw` | none (viewer only) |
| `base` | Chrome (amd64) or Chromium (arm64) | none |
| `cdm` | Node 22, chrome-devtools-mcp 1.12.0 | `GW_MCP_CMD` runs chrome-devtools-mcp against `127.0.0.1:9222` |
| `cua` | cua-driver 0.38.0 (checksum-verified, stripped) | `GW_MCP_CMD="exec cua-driver mcp"` |

## Configuration

`start-desktop.sh` settings:

| Variable | Default | Meaning |
| --- | --- | --- |
| `GEOMETRY` | `1280x800x24` | Xvfb screen size. |
| `BROWSER_URL` | `about:blank` | First page. An empty string disables the browser. |

`desktop-gw` settings:

| Variable | Default | Meaning |
| --- | --- | --- |
| `GW_LISTEN` | `127.0.0.1:8080` (images: `0.0.0.0:8080`) | Listen address. |
| `GW_TOKEN` / `GW_TOKEN_FILE` | generated `/data/gw-token` | Bearer token. Required for non-loopback binds. |
| `GW_MCP_CMD` | per image | Shell command for the stdio MCP server; empty disables `/mcp`. |
| `GW_FPS` | `8` | Capture rate, 1–30. |
| `GW_JPEG_QUALITY` | `70` | JPEG quality, 10–95. |
| `GW_VIEW_ONLY` | unset | `1` disables all input injection. |
| `GW_TLS_CERT`, `GW_TLS_KEY` | unset | Serve HTTPS/WSS. |
| `GW_TLS_CLIENT_CA` | unset | Require client certificates (TLS 1.3). Also enables trust in `X-Forwarded-*`. |

`desktop-gw` subcommands, used by `e2e.sh`:

- `desktop-gw probe -url ws://…/ws/screen [-token …] [-cacert -cert -key] [-out f.jpg]`
  reads the hello and one frame.
- `desktop-gw proxy -listen 127.0.0.1:9443 -upstream https://… -cacert -cert -key`
  is a loopback reverse proxy that presents a client certificate. It stands in
  for the kagent gateway leg.

## Develop

```sh
make test     # cd desktop-gw && go vet ./... && go test -race ./...
```

The tests use a fake MCP child process and cover these areas:

- The MCP Apps flow.
- Protocol errors.
- Session limits.
- Tickets.
- Key mapping.
- Input translation.
- Frame broadcasting.
- Configuration validation.

## Files

| File | Purpose |
| --- | --- |
| `Dockerfile` | `x11`, `base`, `cdm`, `cua` targets. |
| `Dockerfile.gvisor` | Docker-in-Docker host with Substrate's pinned runsc. |
| `start-desktop.sh` | Starts Xvfb, openbox, the browser and `desktop-gw`. |
| `e2e.sh` | Local end-to-end harness (runc and gVisor). |
| `k8s-e2e.sh` | kind smoke test for `deploy/` with kagent's CRDs. |
| `Makefile` | Develop, end-to-end, publish and deploy targets (`make help`). |
| `deploy/` | kustomize base: Deployment, Service, `RemoteMCPServer`, `SandboxTemplate`. |
| `.github/workflows/` | `ci.yaml` (tests, lint, e2e, kind) and `publish.yaml` (GHCR images, manifests). |
| `desktop-gw/` | Gateway source, embedded viewer (`html-client.html`) and MCP Apps host demo (`host-demo.html`). |
| `ADR.md` | Decision record with option comparison and measurements. |
| `DESKFORGE.md` | DeskForge dataset and grounding models, and how they map onto this sandbox. |

## Troubleshooting

- **Port already in use** on 18081, 18082 or 18443: run `./e2e.sh down`, or
  stop the other process.
- **Black or empty viewer**: run `./e2e.sh logs cua` and check
  `/data/desktop-logs/*.log` in the container.
- **gVisor DNS**: Docker's embedded DNS doesn't work under runsc, so the
  harness bind-mounts the host's resolver as `/etc/resolv.conf`.
- **`docker cp` fails under gVisor**: the rootfs overlay is in sandbox memory.
  Use `docker exec … cat` instead (as `e2e.sh` does).
- **Chrome shows "unsupported command-line flag: --no-sandbox"**: expected.
  gVisor is the isolation boundary.
