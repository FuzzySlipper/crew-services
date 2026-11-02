-- crew-board attachments: images and short moving images. ref is what posts
-- name; original_hash finds an upload seen before.
CREATE TABLE attachments (
    ref TEXT PRIMARY KEY,
    original_hash TEXT NOT NULL,
    output_hash TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT '',
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    bytes INTEGER NOT NULL DEFAULT 0,
    checks TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL
);
CREATE INDEX attachments_by_original ON attachments(original_hash);
