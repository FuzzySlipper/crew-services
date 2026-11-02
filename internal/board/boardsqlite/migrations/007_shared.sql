-- crew-board shared boards: a board's network, a post's sync state, the
-- outbox of repo files still to push, each network's sync position, and
-- files a pull ignored and why.
ALTER TABLE boards ADD COLUMN network TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN sync TEXT NOT NULL DEFAULT '';

CREATE TABLE sync_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    network TEXT NOT NULL,
    path TEXT NOT NULL,
    content BLOB NOT NULL,
    post_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    commit_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX sync_outbox_pending ON sync_outbox(network, state, id);

CREATE TABLE sync_state (
    network TEXT PRIMARY KEY,
    last_commit TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL DEFAULT '',
    repo_bytes INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sync_flags (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    network TEXT NOT NULL,
    commit_id TEXT NOT NULL,
    path TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
);
