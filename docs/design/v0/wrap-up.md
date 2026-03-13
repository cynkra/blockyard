# v0 Wrap-Up

Two items remain before v0.0.1: simplifying volume/persistence
configuration, and fixing the broken WebSocket proxy.

---

## 1. Volume & Persistence Strategy

### Problem

The server manages persistent state that must survive container restarts and,
in some cases, be shared with sibling containers (build/worker containers
spawned via the Docker socket). The current approach uses host bind mounts
with manual path translation (`BLOCKYARD_STORAGE_BUNDLE_HOST_PATH`), which
is fragile and requires operators to know the host-side path.

### Persistent State Inventory

| Data | Location | Survives restart? | Shared with siblings? |
|---|---|---|---|
| Bundle archives | `{data}/bundles/{app}/{bundle}.tar.gz` | Only if volume-mounted | No |
| Unpacked bundles | `{data}/bundles/{app}/{bundle}/` | Only if volume-mounted | Yes (ro, build + worker) |
| R libraries | `{data}/bundles/{app}/{bundle}_lib/` | Only if volume-mounted | Yes (rw build, ro worker) |
| rv binary cache | `{data}/bundles/.rv-cache/` | Only if volume-mounted | Yes (ro, build) |
| SQLite database | `{data}/db/blockyard.db` | Only if volume-mounted | No |

### Design: Single Volume + Auto-Detection

#### Data layout

A single data directory (`/data` by default) holds everything:

```
/data/
  bundles/                          # BundleServerPath
    .rv-cache/                      # downloaded rv binaries
    {app-id}/
      {bundle-id}.tar.gz           # uploaded archive
      {bundle-id}/                  # unpacked app code
      {bundle-id}_lib/             # R package library
  db/
    blockyard.db                    # SQLite database
```

One volume, one mount, one path to configure. The database and bundle data
coexist on the same volume. Sibling containers only receive scoped subpath
mounts (e.g. `bundles/app1/bundle-abc` → `/app`), so they cannot access
the database or other apps' data.

#### Auto-detection of mount type

The server already detects its own container ID at startup. If running in a
container, it can inspect itself via `ContainerInspect` to discover how
`/data` is mounted:

| Mount type | What inspect returns | Server behavior |
|---|---|---|
| Named volume | type=`volume`, name=`blockyard-data` | Uses volume name + subpath when mounting into siblings |
| Bind mount | type=`bind`, source=`/host/path/data` | Derives host-side paths for bind mounts into siblings |
| No mount | Path not covered by any mount | Error at startup: "data path is not on a persistent mount" |
| Native mode | No container ID detected | Server path = host path. Bind mounts just work. |

This eliminates both `bundle_host_path` and any explicit `volume_name`
config. The server figures out what it needs from its own container's mount
table.

**Detection algorithm:**

1. If not running in a container (no server ID) → native mode, no
   translation needed.
2. Inspect own container → get list of mounts.
3. Find the mount whose `Destination` is a prefix of `BundleServerPath`.
4. If type=volume → store volume name + compute subpaths relative to the
   mount destination.
5. If type=bind → store host source path + compute host-side paths by
   replacing the destination prefix with the source.
6. If no mount found → warn (data won't survive restart and can't be
   shared). In container mode this is an error; the server refuses to start.

#### Config changes

**Removed:**
- `storage.bundle_host_path` — auto-detected
- `BLOCKYARD_STORAGE_BUNDLE_HOST_PATH` — no longer needed

**Defaults added:**
- `storage.bundle_server_path` defaults to `/data/bundles`
- `database.path` defaults to `/data/db/blockyard.db`

With defaults, a minimal config is just:

```toml
[server]
token = "my-secret-token"

[docker]
image = "ghcr.io/rocker-org/r-ver:4.4.3"
```

And the operator mounts a single volume at `/data`.

#### Sibling container mounts

**Volume mode** — uses `mount.Mount` with `Type: mount.TypeVolume` and
`VolumeOptions.Subpath`:

```go
mount.Mount{
    Type:     mount.TypeVolume,
    Source:   "blockyard-data",     // detected volume name
    Target:   "/app",
    ReadOnly: true,
    VolumeOptions: &mount.VolumeOptions{
        Subpath: "bundles/app1/bundle-abc",
    },
}
```

Requires Docker Engine 26.0+ (API 1.45+, released April 2024).

**Bind mode** — uses `HostConfig.Binds` with the translated host path:

```go
// Mount destination: /data, mount source: /host/path/data
// Server path: /data/bundles/app1/bundle-abc
// Host path:   /host/path/data/bundles/app1/bundle-abc
Binds: []string{
    "/host/path/data/bundles/app1/bundle-abc:/app:ro",
}
```

Same as today, but the host path is derived from container inspection
instead of manual config.

#### Mounts per container type

**Worker container** (runs the Shiny app):
- `{bundle}/` → `/app` (ro)
- `{bundle}_lib/` → `/blockyard-lib` (ro)

**Build container** (runs `rv sync`):
- `{bundle}/` → `/app` (ro)
- `{bundle}_lib/` → `/rv-library` (rw)
- `.rv-cache/{version}/rv` → `/usr/local/bin/rv` (ro)

#### Docker Compose example

```yaml
services:
  blockyard:
    image: ghcr.io/cynkra/blockyard:latest
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - blockyard-data:/data
    environment:
      BLOCKYARD_SERVER_TOKEN: my-secret-token
      BLOCKYARD_DOCKER_IMAGE: ghcr.io/rocker-org/r-ver:4.4.3
    cap_add:
      - NET_ADMIN

volumes:
  blockyard-data:
```

No host path translation. No volume name config. The server inspects itself,
sees `blockyard-data` mounted at `/data`, and uses it.

#### Security: subpath isolation

Volume subpath mounts and bind mounts are kernel-enforced — a container
cannot traverse above its mount point. A symlink inside a bundle pointing to
`../../db/blockyard.db` resolves to a non-existent path inside the container.

If stronger isolation is needed in the future (e.g., regulatory
requirements), the mitigation is straightforward: split into two volumes —
one for bundles (shared with siblings), one for the DB (server-only). The
auto-detection logic handles multiple volumes naturally since it matches
mounts by path prefix.

### Implementation Plan

1. **Mount auto-detection** — Add a `detectMountMode()` function that
   inspects the server's own container and returns a `MountConfig` describing
   how to mount paths into siblings. Called during `DockerBackend.New()`.

2. **Simplify config** — Remove `bundle_host_path` from `StorageConfig`.
   Add defaults for `bundle_server_path` and `database.path`. Remove
   `RestoreParams.HostBasePath` and `toHostPath()` — the Docker backend
   handles path translation internally.

3. **Update mount construction** — Replace hardcoded `Binds` in
   `createWorkerContainer()` and `Build()` with the `MountConfig` helper
   that produces either `Binds` or `Mounts` depending on detected mode.

4. **Startup validation** — At startup, if running in container mode and the
   data path is not on a persistent mount, refuse to start with a clear
   error message.

5. **Update examples and docs** — Update docker-compose.yml to the
   single-volume model. Remove references to
   `BLOCKYARD_STORAGE_BUNDLE_HOST_PATH`.

---

## 2. WebSocket Proxy

### Problem

Shiny apps load HTML and static assets correctly, but the WebSocket
connection fails. The browser reports: *"WebSocket connection to
'ws://localhost:8080/app/hello-shiny/websocket/' failed: The network
connection was lost."* The app UI appears (slider, title) but the plot
never renders and the disconnected overlay appears.

### Architecture: terminate-and-re-dial vs transparent proxy

The current `shuttleWS()` in `internal/proxy/ws.go` uses a
**terminate-and-re-dial** architecture: it *accepts* the client WebSocket
(via `websocket.Accept`), then creates a *new, independent* WebSocket to
the backend (via `websocket.Dial`). Two separate handshakes, two separate
`websocket.Conn` objects, with messages copied between them by `copyWS()`.

This differs fundamentally from how ShinyProxy and standard reverse proxies
(nginx, Apache) handle WebSocket: they **transparently tunnel** the upgrade
request. The browser's HTTP upgrade is forwarded as-is to the backend, the
backend responds with 101, and the proxy copies bytes bidirectionally. One
logical WebSocket connection, end-to-end.

The terminate-and-re-dial approach introduces four bugs:

### Bug 1: 32KB message read limit

`shuttleWS()` creates both client and backend `websocket.Conn` with default
options (`nil`). The `coder/websocket` library (v1.8.14) defaults to a
**32,768 byte read limit** per message. Shiny's `renderPlot` output is a
base64-encoded PNG that easily exceeds 32KB. When the backend sends the
plot response:

1. `src.Read(ctx)` in `copyWS()` exceeds the limit
2. The library closes the connection with `StatusMessageTooBig`
3. `copyWS()` silently returns (error is not logged)
4. `cancel()` fires, killing both directions
5. Client sees the disconnect

**Why the integration test passes:** `TestProxyWebSocketEcho` sends a
5-byte `"hello"` message. No large messages are exchanged, so the limit is
never hit.

**Fix:** Call `SetReadLimit(-1)` on both `clientConn` and `backendConn`
after creation. A proxy should not impose its own message size limits — it
should forward whatever the endpoints exchange. Additionally, `copyWS()`
should log errors at Debug level instead of swallowing them silently.

### Bug 2: `websocket.Accept(w, r, nil)` — origin checking

With `nil` `AcceptOptions`, the `coder/websocket` v1.8.14 library rejects
cross-origin requests by default. Same-origin requests pass because it
compares `Origin` host with `r.Host` via `strings.EqualFold`. However:

- The integration test (`TestProxyWebSocketEcho`) doesn't catch origin
  issues because Go's `websocket.Dial` never sends an `Origin` header —
  and the library allows requests without Origin. Browsers *always* send
  Origin on WebSocket upgrades.
- If the server is accessed via IP, a different hostname, or through an
  intermediate proxy that rewrites the `Host` header, the check silently
  rejects with 403.

**Fix:** Pass `&websocket.AcceptOptions{InsecureSkipVerify: true}` or
configure explicit `OriginPatterns`. A reverse proxy should not enforce
origin policy — that is the backend's responsibility.

### Bug 3: `websocket.Dial(r.Context(), ...)` — fragile context lifecycle

At `ws.go:46`, the backend Dial and the copy-loop context (`ws.go:58`) are
both derived from `r.Context()`. After `websocket.Accept` hijacks the HTTP
connection, `r.Context()` remains valid only because `shuttleWS` blocks
inside `ServeHTTP`. The moment `shuttleWS` returns, Go's `net/http` calls
`w.cancelCtx()`, cancelling the request context.

From the `coder/websocket` docs: *"When the context passed to Dial is
cancelled, the connection is closed."* This means any backend connection
cached in `WsCache` (client-disconnect path, `ws.go:90`) is **immediately
killed** when `shuttleWS` returns, because the cached `Conn`'s internal
context is a child of the now-cancelled `r.Context()`. The entire WsCache
reconnect mechanism is broken.

**Fix:** Use `context.Background()` for the backend Dial and the copy-loop
context, so the backend connection's lifetime is decoupled from the HTTP
request lifecycle.

### Bug 4: Backend Dial loses client headers

`websocket.Dial(ctx, url, nil)` sends a bare-bones upgrade request — no
`Origin`, no cookies, no `Sec-WebSocket-Protocol`, no
`Sec-WebSocket-Extensions`. If the backend expects or inspects any of these
(e.g. httpuv checking Origin, or future Shiny versions negotiating a
subprotocol), the Dial would succeed at the TCP level but the backend could
immediately close the connection.

### Design decision: fix `shuttleWS` or switch to transparent proxy

**Option A — Fix `shuttleWS`:** Apply fixes for bugs 1–4 (read limit,
origin, context, headers). Keeps the WsCache reconnect feature (caching
the backend connection for 60 s when the client disconnects).

**Option B — Transparent proxy via `httputil.ReverseProxy`:** Go's
`httputil.ReverseProxy` (since Go 1.12, refined in 1.20+) handles
WebSocket upgrades transparently:

1. `upgradeType()` detects `Connection: Upgrade` before hop-by-hop header
   removal.
2. After removing hop-by-hop headers, it re-adds `Upgrade` and
   `Connection`.
3. On a 101 response, `handleUpgradeResponse()` hijacks both sides and
   copies bytes bidirectionally.

The existing `forwardHTTP()` already uses `ReverseProxy`. Removing the
`isWebSocketUpgrade` branch in `proxy.go:89` and letting all traffic flow
through `forwardHTTP` would eliminate all four bugs, since raw bytes are
copied without any WebSocket framing interpretation.

**Trade-off:** WsCache cannot work with transparent proxying — it requires
the proxy to understand WebSocket frames. If this feature is needed,
`shuttleWS` should be kept and fixed (option A). If not, transparent
proxying is simpler and more correct (option B).
