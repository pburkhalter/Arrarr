-- v3: add columns to support torrent jobs (in addition to existing usenet) and
-- local download tracking (file pulled from TorBox to NAS, not symlinked).

ALTER TABLE jobs ADD COLUMN source TEXT NOT NULL DEFAULT 'usenet';
-- 'usenet' or 'torrent'. Required at insert by v3 sab/qbit shims.

ALTER TABLE jobs ADD COLUMN magnet TEXT;
-- magnet URI when torrent was submitted as magnet (no .torrent blob).

ALTER TABLE jobs ADD COLUMN local_path TEXT;
-- absolute path to the directory under DOWNLOAD_DIR where files were pulled.
-- Reported to Sonarr/Radarr as the import-from path. Distinct from library_path
-- (v2 STRM/symlink writer output) so v2 and v3 can coexist during transition.

ALTER TABLE jobs ADD COLUMN bytes_downloaded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN bytes_total INTEGER NOT NULL DEFAULT 0;
-- Local download progress. Useful for the qbit shim's torrents/info response so
-- Sonarr/Radarr's progress bar reflects the puller's progress, not just TorBox's.
