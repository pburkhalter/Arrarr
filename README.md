# Arrarr

A SABnzbd-compatible shim that forwards NZB requests from Sonarr/Radarr to
[TorBox](https://torbox.app)'s usenet API and reports the resulting cloud-side
folder back to the *arrs via the existing rclone WebDAV mount.

> _Why "Arrarr"? `torboxarr` was already taken, and the \*arr suite owns every
> clean variation. Doubling the arr was the path of least resistance. Naming
> things is hard._

**Files never hit local disk.** Arrarr only stores SQLite state + a transient
NZB blob (dropped on success). Everything media-shaped lives on TorBox.

```
Sonarr/Radarr → POSTs NZB → Arrarr (mimics SABnzbd) → TorBox API
                                ↓                          ↓
                            SQLite state            TorBox downloads in cloud
                                ↑                          ↓
                       background poller ← TorBox folder appears in /mnt/torbox
                                ↓
                Arrarr reports completed path → Sonarr imports
```

## Why this exists

As of May 2026, no other tool bridges Sonarr/Radarr's NZB workflow to TorBox
without local downloads:

| Tool | TorBox NZB? | Why not |
|---|---|---|
| Decypharr | ❌ | NNTP-only; tracking [issue #7](https://github.com/sirrobot01/decypharr/issues/7) still open |
| DebriDav | ❌ | "TorBox usenet not yet supported" (per README) |
| TorBoxarr | ⚠️ | Submits to TorBox but downloads files locally (defeats zero-storage) |
| NzbDav, AltMount | ❌ | NNTP-only |

Arrarr fills that gap.

## Quickstart

### Prerequisites

- TorBox API key with usenet enabled
- A working rclone WebDAV mount of TorBox at some path (e.g. `/mnt/torbox`),
  bind-mounted into Sonarr/Radarr at the same path the *arrs see in their
  root folder configuration (e.g. `/torbox`)

### Run

```yaml
# docker-compose.yaml — see deploy/docker-compose.example.yaml
services:
  arrarr:
    image: ghcr.io/pburkhalter/arrarr:latest
    restart: unless-stopped
    ports: ["8086:8080"]
    environment:
      ARRARR_API_KEY: a-secret-key-sonarr-will-send
      TORBOX_API_KEY: ${TORBOX_API_KEY}
      LOCAL_VERIFY_BASE: /torbox
      SONARR_VISIBLE_BASE: /torbox
    volumes:
      - /opt/docker-data/arrarr:/data
      - /mnt/torbox:/torbox:ro
```

### Wire Sonarr/Radarr

In Settings → Download Clients → Add → SABnzbd:

| Field | Value |
|---|---|
| Host | `arrarr` (or your container IP / hostname) |
| Port | `8086` |
| URL Base | `/sabnzbd` |
| API Key | the value of `ARRARR_API_KEY` |
| Category | `sonarr` (or `radarr` / whatever) |

Click **Test** → green. Done.

## Configuration

All configuration is via environment variables.

| Variable | Default | Notes |
|---|---|---|
| `ARRARR_API_KEY` | _(required)_ | the key Sonarr/Radarr send |
| `TORBOX_API_KEY` | _(required)_ | upstream bearer token |
| `ARRARR_LISTEN` | `:8080` | HTTP bind |
| `ARRARR_URL_BASE` | `/sabnzbd` | URL prefix |
| `TORBOX_BASE_URL` | `https://api.torbox.app/v1/api` | override for tests |
| `TORBOX_RATE_LIMIT_PER_MIN` | `200` | TorBox documents 300/min/key — leave headroom |
| `LOCAL_VERIFY_BASE` | `/mnt/torbox` | path where arrarr can `stat()` files |
| `SONARR_VISIBLE_BASE` | `/torbox` | path reported back to Sonarr/Radarr |
| `DB_PATH` | `/data/arrarr.db` | SQLite location (mount as a volume) |
| `WORKER_POLL_INTERVAL` | `30s` | TorBox `mylist` cadence |
| `WORKER_VERIFY_INTERVAL` | `60s` | WebDAV stat cadence |
| `DISPATCH_INTERVAL` | `10s` | submission scan cadence |
| `REAP_INTERVAL` | `5m` | reap cadence |
| `JOB_RETENTION_DAYS` | `7` | terminal jobs deleted after this |
| `WORKER_POOL_SIZE` | `4` | concurrent submitters |
| `MAX_NZB_BYTES` | `52428800` | 50 MB upload cap |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

### Path mapping cheat sheet

If your Sonarr container sees TorBox at `/torbox` (because it bind-mounts
`/mnt/torbox:/torbox:ro` from the host), and arrarr's container also bind-mounts
`/mnt/torbox:/torbox:ro`, then both bases are `/torbox`. That's the simple case.

If they differ — say arrarr has it at `/mnt/torbox` but Sonarr has it at
`/torbox` — set `LOCAL_VERIFY_BASE=/mnt/torbox` and `SONARR_VISIBLE_BASE=/torbox`.

## How it works

Arrarr is a small Go daemon with four background loops:

- **Submitter** — claims jobs in `NEW`, POSTs them to `POST /usenet/createusenetdownload`,
  transitions to `SUBMITTED`. Retries on failure with exponential backoff (max 5 attempts).
- **Poller** — every 30s, makes a single `GET /usenet/mylist` call, updates
  every in-flight job. Handles TorBox's queue_id → active_id swap and tracks
  the `folder_name` field (which is the WebDAV directory name).
- **Verifier** — every 60s, `os.Stat()` the WebDAV mount for each
  `COMPLETED_TORBOX` job. Folder visible → `READY`, which is what Sonarr's
  history endpoint reports as "Completed" with the storage path.
- **Reaper** — every 5min, deletes terminal jobs older than `JOB_RETENTION_DAYS`.

State machine:

```
NEW → SUBMITTED → DOWNLOADING → COMPLETED_TORBOX → READY
 ↓        ↓           ↓               ↓
 ↳───── FAILED  ←─────┴───────────────┘
```

### Dual-id swap

TorBox returns a `queue_id` when an NZB is first accepted. Once it activates,
the `mylist` row gains an `id`. Arrarr stores both and matches by either —
deletions and lookups always prefer the `active_id` once it's seen.

### WebDAV cache lag

TorBox's WebDAV cache refreshes every 15 min. After TorBox's API says "complete",
the folder may not yet be visible on the mount. Arrarr keeps Sonarr's queue
showing "Verifying" (99%) until the folder actually appears, then transitions
to `READY` and Sonarr imports.

### MIME type

TorBox rejects `Content-Type: application/x-nzb;charset=utf-8`. The multipart
file part is sent with plain `application/x-nzb` — Go's stdlib
`mime/multipart.Writer.CreateFormFile` would attach a charset parameter, so
arrarr drives the part header by hand.

## Development

```bash
go test -race ./...
go build ./cmd/arrarr
```

The integration test in [`internal/worker/integration_test.go`](internal/worker/integration_test.go)
drives a job from `NEW` to `READY` against a fake TorBox + fake mount in under
a second. It exercises the dual-id swap and webdav-lag handling.

## Limitations & out of scope

- Web UI — none. Logs + Sonarr/Radarr's queue/history are the only surface.
- Metrics — only `/healthz`. No Prometheus.
- Multi-debrid — TorBox only.
- Torrents — TorBox already pairs with [Decypharr](https://github.com/sirrobot01/decypharr) for that.
- BlackHole / NZBGet protocols — only SABnzbd's API surface.

## License

MIT.
