-- Attachments a purge removed, by hash: replaying the repo (a reindex) never
-- brings their bytes back, though the repo still holds them.
CREATE TABLE purged_blobs (
    hash TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
