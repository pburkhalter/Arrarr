#!/usr/bin/env python3
"""
arr_import_cleaner.py

Resolves importPending entries in Sonarr/Radarr that arrarr+TorBox+rclone
cannot complete naturally. Two modes:

  --mode=import   trigger ManualImport (Sonarr/Radarr try to copy via the
                  rclone vfs cache; queue entry clears, library shows file)
  --mode=cleanup  delete queue entry and unmonitor the underlying
                  episode/movie so it isn't re-grabbed (no library entry)

Default is dry-run: prints what it would do and exits.

Env required:
  SONARR_URL, SONARR_API_KEY
  RADARR_URL, RADARR_API_KEY

Either pair may be omitted to skip that arr.

Usage:
  source ~/.config/arr-keys.env
  export SONARR_URL=http://localhost:$SONARR_PORT
  export RADARR_URL=http://localhost:$RADARR_PORT
  ./arr_import_cleaner.py                    # dry-run, both arrs
  ./arr_import_cleaner.py --mode=import      # trigger ManualImport
  ./arr_import_cleaner.py --mode=cleanup     # remove + unmonitor
  ./arr_import_cleaner.py --only=sonarr --mode=cleanup
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def api(method, base, key, path, body=None, params=None, timeout=30):
    url = base.rstrip("/") + "/api/v3/" + path.lstrip("/")
    if params:
        url += "?" + urllib.parse.urlencode(params)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        url,
        data=data,
        method=method,
        headers={"X-Api-Key": key, "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def list_pending(base, key):
    out, page = [], 1
    while True:
        d = api(
            "GET", base, key, "queue",
            params={
                "page": page,
                "pageSize": 200,
                "includeUnknownSeriesItems": "true",
                "includeUnknownMovieItems": "true",
            },
        )
        recs = d.get("records", []) or []
        out.extend(r for r in recs if r.get("trackedDownloadState") == "importPending")
        if page * 200 >= d.get("totalRecords", 0) or not recs:
            break
        page += 1
    return out


def trigger_manual_import(base, key, folder, kind):
    """Returns (cmd_id, file_count). file_count == 0 means nothing matched."""
    files = api(
        "GET", base, key, "manualimport",
        params={"folder": folder, "filterExistingFiles": "false"},
    )
    payload = []
    for f in files or []:
        path = f.get("path")
        if not path:
            continue
        if kind == "series":
            if not f.get("series") or not f.get("episodes"):
                continue
            payload.append({
                "path": path,
                "seriesId": f["series"]["id"],
                "episodeIds": [e["id"] for e in f["episodes"]],
                "quality": f["quality"],
                "languages": f.get("languages") or [],
                "releaseGroup": f.get("releaseGroup", ""),
            })
        else:
            if not f.get("movie"):
                continue
            payload.append({
                "path": path,
                "movieId": f["movie"]["id"],
                "quality": f["quality"],
                "languages": f.get("languages") or [],
                "releaseGroup": f.get("releaseGroup", ""),
            })
    if not payload:
        return None, 0
    cmd = api("POST", base, key, "command", body={
        "name": "ManualImport",
        "files": payload,
        "importMode": "Copy",
    })
    return cmd.get("id"), len(payload)


def unmonitor_and_delete(base, key, rec, kind):
    qid = rec["id"]
    if kind == "series" and rec.get("episodeId"):
        api("PUT", base, key, "episode/monitor", body={
            "episodeIds": [rec["episodeId"]],
            "monitored": False,
        })
    elif kind == "movies" and rec.get("movieId"):
        m = api("GET", base, key, f"movie/{rec['movieId']}")
        m["monitored"] = False
        api("PUT", base, key, f"movie/{m['id']}", body=m)
    api("DELETE", base, key, f"queue/{qid}", params={
        "removeFromClient": "false",
        "blocklist": "false",
        "skipRedownload": "true",
    })


def process(name, base, key, kind, mode, limit):
    print(f"\n=== {name} ===")
    pending = list_pending(base, key)
    print(f"  {len(pending)} importPending")
    if limit:
        pending = pending[:limit]
    for r in pending:
        title = (r.get("title") or "?")[:70]
        folder = r.get("outputPath")
        if mode == "dry":
            print(f"  - {title}  folder={folder}")
            continue
        try:
            if mode == "import":
                if not folder:
                    print(f"  - {title}: no outputPath, skip")
                    continue
                cmd_id, n = trigger_manual_import(base, key, folder, kind)
                if n == 0:
                    print(f"  - {title}: manualimport found 0 files; falling back to cleanup")
                    unmonitor_and_delete(base, key, r, kind)
                else:
                    print(f"  - {title}: ManualImport cmd id={cmd_id} files={n}")
            elif mode == "cleanup":
                unmonitor_and_delete(base, key, r, kind)
                print(f"  - {title}: removed + unmonitored")
        except urllib.error.HTTPError as e:
            print(f"  - {title}: HTTP {e.code} {e.read()[:200]!r}")
        except Exception as e:
            print(f"  - {title}: {e!r}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["dry", "import", "cleanup"], default="dry")
    ap.add_argument("--only", choices=["sonarr", "radarr"])
    ap.add_argument("--limit", type=int, default=0, help="max items per arr (0 = all)")
    args = ap.parse_args()

    targets = []
    if args.only != "radarr":
        if os.environ.get("SONARR_URL") and os.environ.get("SONARR_API_KEY"):
            targets.append(("Sonarr", os.environ["SONARR_URL"],
                            os.environ["SONARR_API_KEY"], "series"))
    if args.only != "sonarr":
        if os.environ.get("RADARR_URL") and os.environ.get("RADARR_API_KEY"):
            targets.append(("Radarr", os.environ["RADARR_URL"],
                            os.environ["RADARR_API_KEY"], "movies"))
    if not targets:
        print("no SONARR_URL+SONARR_API_KEY or RADARR_URL+RADARR_API_KEY in env",
              file=sys.stderr)
        sys.exit(2)

    print(f"mode={args.mode}")
    for name, base, key, kind in targets:
        process(name, base, key, kind, args.mode, args.limit)


if __name__ == "__main__":
    main()
