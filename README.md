# Arrarr

A SABnzbd + qBittorrent shim that submits NZBs and torrents to
[TorBox](https://torbox.app), waits for completion in the cloud, then pulls
the finished files to a local directory so Sonarr/Radarr can import via a
normal local path.

> _Why "Arrarr"? `torboxarr` was already taken, and the \*arr suite owns every
> clean variation. Doubling the arr was the path of least resistance._

## How it works

```
Prowlarr → Sonarr/Radarr ─grab─→  Arrarr  ─submit─→  TorBox cloud
                          ←completed local path        │
                                  │                    │
                                  │ ←─webhook (opt)────┤
                                  │                    │
                                  │  poller            │
                                  │  puller    ────────┘ (HTTPS CDN)
                                  ↓
                              /downloads/sonarr/…  →  Sonarr imports
                              /downloads/radarr/…
```

Sonarr/Radarr see arrarr as either:

- a SABnzbd download client (`POST /api?mode=addfile`) for usenet, or
- a qBittorrent v2 client (`POST /api/v2/torrents/add`) for torrents.

Arrarr forwards the NZB/torrent/magnet to TorBox, polls until TorBox finishes
downloading in the cloud, then pulls the bytes to `ARRARR_DOWNLOAD_DIR` and
reports that local path back. The arrs do a normal local import.

## Quickstart

### Prerequisites

- TorBox API key (usenet, torrent, or both enabled).
- A writable local directory for completed downloads (e.g. `/mnt/downloads`).
  Sonarr/Radarr need to see the same directory at the same path so their
  import scan finds the files.

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
      ARRARR_DOWNLOAD_DIR: /downloads
      ARRARR_QBIT_USERNAME: admin
      ARRARR_QBIT_PASSWORD: ${ARRARR_QBIT_PASSWORD}
    volumes:
      - /opt/docker-data/arrarr:/data
      - /mnt/downloads:/downloads
```

### Wire Sonarr/Radarr

**SABnzbd** (usenet): Settings → Download Clients → Add → SABnzbd

| Field | Value |
|---|---|
| Host | `arrarr` |
| Port | `8086` |
| URL Base | `/sabnzbd` |
| API Key | `ARRARR_API_KEY` |
| Category | `sonarr` (or `radarr`) |

**qBittorrent** (torrents): Settings → Download Clients → Add → qBittorrent

| Field | Value |
|---|---|
| Host | `arrarr` |
| Port | `8086` |
| Username | `ARRARR_QBIT_USERNAME` |
| Password | `ARRARR_QBIT_PASSWORD` |
| Category | `sonarr` (or `radarr`) |

## Configuration

| Variable | Default | Description |
|---|---|---|
| `ARRARR_API_KEY` | — *(required)* | SAB API key Sonarr/Radarr must present |
| `TORBOX_API_KEY` | — *(required)* | TorBox API key |
| `ARRARR_LISTEN` | `:8080` | listen address |
| `ARRARR_URL_BASE` | `/sabnzbd` | SAB URL prefix |
| `ARRARR_DOWNLOAD_DIR` | `/downloads` | local target for the puller |
| `ARRARR_DOWNLOAD_CONCURRENCY` | `4` | parallel file pulls per job |
| `ARRARR_QBIT_USERNAME` | `admin` | enables qBit shim when password also set |
| `ARRARR_QBIT_PASSWORD` | _(empty)_ | enables qBit shim |
| `ARRARR_QBIT_URL_BASE` | _(empty)_ | qBit URL prefix; conventionally rootless |
| `ARRARR_MAX_TORRENT_BYTES` | `4194304` | max .torrent upload size |
| `MAX_NZB_BYTES` | `52428800` | max NZB upload size |
| `DB_PATH` | `/data/arrarr.db` | SQLite path |
| `WORKER_POLL_INTERVAL` | `30s` | TorBox mylist poll cadence |
| `DISPATCH_INTERVAL` | `10s` | submitter tick |
| `WORKER_POOL_SIZE` | `4` | parallel submitter workers |
| `REAP_INTERVAL` | `5m` | retention sweep cadence |
| `JOB_RETENTION_DAYS` | `7` | drop terminal rows older than this |
| `TORBOX_RATE_LIMIT_PER_MIN` | `200` | client-side rate ceiling |

**Webhook receiver (optional):**

| Variable | Default | Description |
|---|---|---|
| `ARRARR_TORBOX_WEBHOOK_SECRET` | _(empty)_ | enables receiver; empty → 503 |
| `ARRARR_TORBOX_WEBHOOK_REQUIRE_SIGNATURE` | `true` | strict HMAC. Set to `false` for TorBox (they don't sign). |
| `ARRARR_WEBHOOK_REPLAY_WINDOW` | `5m` | replay protection window |

**Pushover notifications (optional):**

| Variable | Default | Description |
|---|---|---|
| `ARRARR_PUSHOVER_TOKEN` | _(empty)_ | |
| `ARRARR_PUSHOVER_USER` | _(empty)_ | |
| `ARRARR_PUSHOVER_NOTIFY_ON` | `off` | `off` / `ready` / `failed` / `both` |

## Worker loops

| Loop | Cadence | Purpose |
|---|---|---|
| Submitter | dispatch interval | Claim NEW jobs, call TorBox create-{usenet,torrent}, advance to SUBMITTED |
| Poller | poll interval | Reconcile inflight jobs against `/usenet/mylist` + `/torrents/mylist` |
| Puller | 15s | Download completed TorBox items to `ARRARR_DOWNLOAD_DIR`, advance to READY |
| Reaper | reap interval | Drop terminal jobs past retention |

## State machine

```
NEW → SUBMITTED → DOWNLOADING → COMPLETED_TORBOX → READY
                                                ↘  FAILED / CANCELED
```

## Endpoints

- `/sabnzbd/api` — SABnzbd-compatible (`mode=addfile`, `queue`, `history`, etc.)
- `/api/v2/*` — qBittorrent v2 (when credentials configured)
- `/webhook` — TorBox webhook receiver (when secret configured)
- `/healthz` — liveness probe
- `/` — status dashboard

## CLI

```
arrarr                  Run the daemon (default).
arrarr healthcheck      Probe /healthz on the local listener.
arrarr verify-webhook   Trigger TorBox /notifications/test.
arrarr version          Print build version.
```

## Development

```
go build ./...
go test ./...
docker buildx build -f deploy/Dockerfile .
```

SQLite schema lives in `internal/store/migrations/`. Migrations run
automatically on startup. Schema requires SQLite ≥ 3.35 (for
`UPDATE … RETURNING` + `ALTER TABLE DROP COLUMN`).

## License

MIT — see [LICENSE](LICENSE).
