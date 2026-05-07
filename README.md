# Arrarr

A SABnzbd-compatible shim that submits NZBs to [TorBox](https://torbox.app)'s
usenet API, writes a structured library tree for Sonarr/Radarr/Jellyfin to
read, and (optionally) listens for TorBox webhooks so completion is instant
instead of polled.

> _Why "Arrarr"? `torboxarr` was already taken, and the \*arr suite owns every
> clean variation. Doubling the arr was the path of least resistance. Naming
> things is hard._

**Files never hit local disk.** Arrarr writes only SQLite state, transient NZB
blobs (dropped on success), and tiny STRM text files (or symlinks). All
playable bytes live on TorBox.

## Two ways to look at it

The minimal v1 version of this — submit NZB → wait → tell Sonarr the path —
still exists, and is the right model if you only have a single Arrarr user
and a single arr stack:

```
Sonarr/Radarr → POSTs NZB → Arrarr (mimics SABnzbd) → TorBox API
                                ↓                          ↓
                            SQLite state            TorBox downloads in cloud
                                ↑                          ↓
                       background poller ← TorBox folder appears in /mnt/torbox
                                ↓
                Arrarr reports completed path → Sonarr imports
```

v2 keeps that pipeline and layers a **library writer** on top, so Arrarr is
also responsible for producing the tree Sonarr/Radarr/Jellyfin all read from.
That tree can be **STRM** (text files containing per-file TorBox CDN URLs,
refreshed before they expire) or **WebDAV symlinks** (relative links into the
existing rclone mount), or both. With v2 enabled the picture is:

```
                                  +─────────────+
Prowlarr → Sonarr/Radarr ─grab─→  │   Arrarr    │  ─submit─→  TorBox
                          ←completed path        │              │
                                  │  + tags      ├──────────────┤
                                  │              │  webhook     │
                                  │  ←─signed────┤  notifies    │
                                  │              │              │
                                  │  librarian   │              │
                                  │  /url-refresh│              │
                                  │  /tagger     │              │
                                  │  /mirror     │              │
                                  +──────┬───────+              │
                                         │                      │
                                  Pushover (matched & ours)     │
                                         │                      │
                              /library/series/...  ←──[STRM/symlink]──  TorBox storage
                              /library/movies/...                        (rclone WebDAV)
                                         │
                                      Jellyfin
```

You opt into each v2 piece independently — see [Configuration](#configuration).

## Why this exists

As of May 2026, no other tool bridges Sonarr/Radarr's NZB workflow to TorBox
without local downloads:

| Tool | TorBox NZB? | Why not |
|---|---|---|
| Decypharr | ❌ | NNTP-only; tracking [issue #7](https://github.com/sirrobot01/decypharr/issues/7) still open |
| DebriDav | ❌ | "TorBox usenet not yet supported" (per README) |
| TorBoxarr | ⚠️ | Submits to TorBox but downloads files locally (defeats zero-storage) |
| NzbDav, AltMount | ❌ | NNTP-only |

Arrarr fills that gap, and v2 also turns it into the file-organizer Sonarr
can't be when its source lives on a read-only WebDAV.

## Quickstart (v1: minimal)

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

Settings → Download Clients → Add → SABnzbd:

| Field | Value |
|---|---|
| Host | `arrarr` |
| Port | `8086` |
| URL Base | `/sabnzbd` |
| API Key | the value of `ARRARR_API_KEY` |
| Category | `sonarr` (or `radarr`) |

Click **Test** → green. Done.

## v2 add-ons

Each is independently optional. Out of the box (no v2 env vars set) the binary
behaves exactly like v1.

### 1. Library writer

Have Arrarr produce the structured tree directly, so Sonarr/Radarr's import
phase is a no-op (in-place register) and Jellyfin reads the same tree.

```yaml
environment:
  ARRARR_LIBRARY_BASE: /library
  ARRARR_LIBRARY_MODE: strm   # or webdav, both
volumes:
  - /opt/docker-data/library:/library
```

Sonarr/Radarr Root Folder is then `/library/series` and `/library/movies`
respectively (they're created lazily). Jellyfin scans the same paths.

Modes:
- **`strm`** (default v2 mode): writes `<file>.strm` text files containing the
  TorBox CDN URL. Jellyfin/Kodi follow STRM natively. Faster initial play
  than fuse mounts. Refreshed automatically before the URL TTL.
- **`webdav`**: writes relative symlinks targeting `<mountBase>/<release>/<file>`.
  Reads go through the rclone fuse mount.
- **`both`**: writes both side-by-side (rare, for A/B comparison).

Optional canonical naming via Sonarr/Radarr's `parse` API:

```yaml
environment:
  ARRARR_SONARR_URL: http://host.docker.internal:8989
  ARRARR_SONARR_API_KEY: ${SONARR_API_KEY}
  ARRARR_RADARR_URL: http://host.docker.internal:7878
  ARRARR_RADARR_API_KEY: ${RADARR_API_KEY}
```

When set, Arrarr asks the appropriate arr to identify each completed release,
producing clean `<show>/Season XX/<show> - SXXEXX - <ep title>.strm` paths.
Without these, the layout falls back to `<release-name>/<file-basename>.strm`
— Sonarr will still ingest correctly via its own scan, just less prettily.

### 2. TorBox webhook receiver

Skip the 30s poll latency for completion events.

```yaml
environment:
  ARRARR_TORBOX_WEBHOOK_SECRET: ${TORBOX_WEBHOOK_SECRET}
```

The receiver lives at `<URL_BASE>/webhook` (e.g. `/sabnzbd/webhook`). It
verifies [Standard Webhooks](https://www.standardwebhooks.com/)-format
signatures, walks a lookup chain to find the matching job (structured ID →
folder name → nzo_id-in-message), and wakes the dispatcher so the poller
picks up the new state instantly. Foreign downloads (other users on a shared
TorBox account) get a clean `204` and no other side effects.

Configure in TorBox: Settings → Webhooks → URL = your public Arrarr URL,
secret = whatever you put in `ARRARR_TORBOX_WEBHOOK_SECRET`.

### 3. Pushover notifications

Notify on `download.ready` (and optionally `download.failed`) but **only for
downloads Arrarr itself initiated**. Mirror-mode rows (other users on the
account) stay silent.

```yaml
environment:
  ARRARR_PUSHOVER_TOKEN: ${PUSHOVER_TOKEN}
  ARRARR_PUSHOVER_USER: ${PUSHOVER_USER_KEY}
  ARRARR_PUSHOVER_NOTIFY_ON: ready  # off | ready | failed | both
```

Notifications are race-safe: Arrarr atomically claims the
`pushover_sent_at` slot via SQL UPDATE before sending, so duplicate webhook
deliveries don't double-notify.

### 4. Mirror mode (shared TorBox account)

Show every download on the TorBox account in Jellyfin's library, not just
Arrarr-initiated ones — useful when multiple humans share one TorBox plan.
Pushover stays silent for mirrored rows.

```yaml
environment:
  ARRARR_MIRROR_MODE: on
  ARRARR_MIRROR_POLL_INTERVAL: 10m
```

A periodic mylist sweep synthesizes `origin='mirror'` jobs and feeds them
through the librarian. Removing a download on TorBox prunes the mirror row +
its library entry within one sweep. Empty mylist responses are treated as
transient (no orphan sweep on a possibly-blip API blip).

### 5. Tagging on the shared account

Arrarr stamps every download it creates with TorBox `tags`:

```
["arrarr", "host:<ARRARR_INSTANCE_NAME>", "job:<uuid>", "cat:<sonarr|radarr>"]
```

The webhook handler uses these (plus the structured ID) to identify ownership
on multi-user accounts. The submitter calls `EditUsenet` immediately after
create; if TorBox rejects it (item not yet "cached"), a background tagger
loop retries every 90 seconds until it sticks.

## Configuration

Everything is environment variables. Defaults are conservative — out of the
box only the four required v1 vars need to be set; v2 features stay off.

| Variable | Default | Notes |
|---|---|---|
| **Required** | | |
| `ARRARR_API_KEY` | _(required)_ | the key Sonarr/Radarr send |
| `TORBOX_API_KEY` | _(required)_ | upstream bearer token |
| **Server / paths (v1)** | | |
| `ARRARR_LISTEN` | `:8080` | HTTP bind |
| `ARRARR_URL_BASE` | `/sabnzbd` | URL prefix |
| `LOCAL_VERIFY_BASE` | `/mnt/torbox` | path where arrarr can `stat()` files |
| `SONARR_VISIBLE_BASE` | `/torbox` | path reported back to Sonarr/Radarr in v1 mode |
| `DB_PATH` | `/data/arrarr.db` | SQLite location (mount as a volume) |
| `MAX_NZB_BYTES` | `52428800` | 50 MB upload cap |
| **TorBox** | | |
| `TORBOX_BASE_URL` | `https://api.torbox.app/v1/api` | override for tests |
| `TORBOX_RATE_LIMIT_PER_MIN` | `200` | TorBox documents 300/min/key |
| **Worker cadence** | | |
| `WORKER_POLL_INTERVAL` | `30s` | TorBox `mylist` cadence |
| `WORKER_VERIFY_INTERVAL` | `60s` | WebDAV stat cadence |
| `DISPATCH_INTERVAL` | `10s` | submission scan cadence |
| `REAP_INTERVAL` | `5m` | reap cadence |
| `JOB_RETENTION_DAYS` | `7` | terminal jobs deleted after this |
| `WORKER_POOL_SIZE` | `4` | concurrent submitters |
| **v2: tagging** | | |
| `ARRARR_INSTANCE_NAME` | `arrarr` | becomes the `host:` tag segment on TorBox |
| **v2: library writer** | | |
| `ARRARR_LIBRARY_BASE` | `/library` | root of the structured tree |
| `ARRARR_LIBRARY_MODE` | `strm` | `strm` / `webdav` / `both` / `off` |
| `ARRARR_STREAMING_URL_REFRESH_AFTER` | `5h` | how long after writing a STRM the URL is presumed valid |
| **v2: webhook** | | |
| `ARRARR_TORBOX_WEBHOOK_SECRET` | _(empty)_ | enables `<URL_BASE>/webhook`; empty → 503 |
| `ARRARR_WEBHOOK_REPLAY_WINDOW` | `5m` | Standard Webhooks timestamp tolerance |
| **v2: Pushover** | | |
| `ARRARR_PUSHOVER_TOKEN` | _(empty)_ | Pushover application token |
| `ARRARR_PUSHOVER_USER` | _(empty)_ | Pushover user/group key |
| `ARRARR_PUSHOVER_NOTIFY_ON` | `ready` | `off` / `ready` / `failed` / `both` |
| **v2: mirror mode** | | |
| `ARRARR_MIRROR_MODE` | `off` | `off` / `on` |
| `ARRARR_MIRROR_POLL_INTERVAL` | `10m` | mylist sweep cadence |
| **v2: arr canonical naming** | | |
| `ARRARR_SONARR_URL` | _(empty)_ | enables Sonarr `parse` callback for naming |
| `ARRARR_SONARR_API_KEY` | _(empty)_ | |
| `ARRARR_RADARR_URL` | _(empty)_ | enables Radarr `parse` callback for naming |
| `ARRARR_RADARR_API_KEY` | _(empty)_ | |
| `ARRARR_ARR_CALLBACK_TIMEOUT` | `5s` | per-call timeout |
| **Logging** | | |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

### Path mapping cheat sheet (v1 mode)

If your Sonarr container sees TorBox at `/torbox` (because it bind-mounts
`/mnt/torbox:/torbox:ro` from the host), and arrarr's container also bind-mounts
`/mnt/torbox:/torbox:ro`, then both bases are `/torbox`. That's the simple case.

If they differ — say arrarr has it at `/mnt/torbox` but Sonarr has it at
`/torbox` — set `LOCAL_VERIFY_BASE=/mnt/torbox` and `SONARR_VISIBLE_BASE=/torbox`.

In v2 library mode this all becomes irrelevant: Arrarr reports the in-library
path it just wrote, and Sonarr's import is in-place.

## How it works

Arrarr is a small Go daemon with a handful of background loops:

| Loop | Cadence | Job |
|---|---|---|
| Submitter | dispatch interval + wake | claim NEW jobs, POST `createusenetdownload`, transition to SUBMITTED |
| Poller | poll interval | one `mylist` call per tick, update every in-flight job |
| Verifier | verify interval | `os.Stat` the WebDAV mount; advisory only when v2 librarian is on |
| Librarian (v2) | verify interval | claim COMPLETED_TORBOX rows, fetch files + URLs, write library entries, transition to READY |
| URL refresh (v2) | quarter of refresh-after | rewrite STRM files before TorBox CDN URLs expire |
| Tagger retry (v2) | 90s | retry `editusenetdownload` for rows TorBox hadn't cached at submit time |
| Mirror (v2, opt-in) | mirror poll interval | sweep `mylist`, synthesize `origin='mirror'` jobs, prune orphans |
| Reaper | reap interval | drop terminal jobs older than retention |

State machine (v2):

```
NEW → SUBMITTED → DOWNLOADING → COMPLETED_TORBOX ─[librarian writes]→ READY
 ↓        ↓           ↓                 ↓                                ↑
 ↳───── FAILED  ←─────┴─────────────────┴────────────────────────────────┘
```

In v1 mode (no librarian), the verifier transitions COMPLETED_TORBOX → READY
directly; the librarian step is skipped.

### Dual-id swap

TorBox returns a `queue_id` when an NZB is first accepted. Once it activates,
the `mylist` row gains an `id`. Arrarr stores both and matches by either —
deletions, lookups, and webhook id-resolution always prefer `active_id` once
it's seen.

### WebDAV cache lag

TorBox's WebDAV cache refreshes every 15 min. After TorBox's API says
"complete", the folder may not yet be visible on the mount. The verifier
keeps Sonarr's queue showing "Verifying" (99%) until the folder actually
appears, then transitions onward. With v2 STRM mode this is largely
sidestepped — STRMs point at TorBox's CDN, not the WebDAV mount, so playback
works the moment TorBox reports completion regardless of mount state.

### Webhook signature

Standard Webhooks — HMAC-SHA256 over `"<webhook-id>.<webhook-timestamp>.<body>"`,
verified with the shared secret you set in `ARRARR_TORBOX_WEBHOOK_SECRET`.
Replay-window enforcement (default ±5 min) drops events with skewed timestamps.

Lookup chain on a verified event, in priority order:

1. **Structured ID** in `data` — match against `torbox_active_id` or `torbox_queue_id`
2. **Folder name** in `data` — case-insensitive against `torbox_folder_name`
3. **nzo_id pattern** (`arrarr_[a-f0-9]{32}`) embedded in `data.message`

If none match, the handler returns `204` and does nothing — TorBox should not
retry. Foreign-account downloads disappear into this branch silently.

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

Integration tests in [`internal/worker/integration_test.go`](internal/worker/integration_test.go),
[`internal/worker/b3_test.go`](internal/worker/b3_test.go), and
[`internal/worker/mirror_test.go`](internal/worker/mirror_test.go) drive jobs
all the way from `NEW` through library write against fakes, in well under a
second each.

The full v2 design lives in [`PLAN.md`](PLAN.md).

## Limitations & out of scope

- **Web UI** — there's a tiny status dashboard at `/`, no rich UI.
- **Metrics** — only `/healthz`. No Prometheus.
- **Multi-debrid** — TorBox only.
- **Torrents** — TorBox already pairs with [Decypharr](https://github.com/sirrobot01/decypharr) for torrents; Arrarr is usenet-only by design.
- **BlackHole / NZBGet protocols** — only SABnzbd's API surface.
- **Automatic webhook URL registration** — TorBox webhook URL+secret must be
  pasted into TorBox's UI by hand; no API endpoint exists for that yet. The
  `arrarr verify-webhook` subcommand can fire a test notification once
  configuration is in place.

## License

MIT.
