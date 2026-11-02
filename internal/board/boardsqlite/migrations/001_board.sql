-- crew-board core: boards, threads, posts and their revisions, events, and
-- operation receipts. Rows are added, and changed only where a post's state
-- or a thread's activity moves on; a purge scrubs text in place.
CREATE TABLE boards (
    name TEXT PRIMARY KEY,
    site TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL
);

CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    board TEXT NOT NULL REFERENCES boards(name),
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    opening_post_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    last_activity_at TEXT NOT NULL,
    purged INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX threads_by_activity ON threads(board, last_activity_at DESC, id DESC);

CREATE TABLE posts (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(id),
    parent_id TEXT,
    author TEXT NOT NULL,
    site TEXT NOT NULL,
    state TEXT NOT NULL,
    revision INTEGER NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    edited_at TEXT
);
CREATE INDEX posts_by_thread ON posts(thread_id, id);

CREATE TABLE revisions (
    post_id TEXT NOT NULL REFERENCES posts(id),
    revision INTEGER NOT NULL,
    body TEXT NOT NULL,
    mentions TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    PRIMARY KEY (post_id, revision)
);

CREATE TABLE events (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(id),
    post_id TEXT,
    type TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL,
    data TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX events_by_thread ON events(thread_id, id);

CREATE TABLE operations (
    operation_id TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    result TEXT NOT NULL,
    created_at TEXT NOT NULL
);
