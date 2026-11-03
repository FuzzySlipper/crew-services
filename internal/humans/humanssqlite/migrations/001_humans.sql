CREATE TABLE notices (
    id TEXT PRIMARY KEY,
    person TEXT NOT NULL,
    source TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    link TEXT NOT NULL,
    reason TEXT NOT NULL,
    grp TEXT NOT NULL,
    urgent INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    read_at TEXT,
    UNIQUE (source, idempotency_key)
);

CREATE INDEX notices_by_person ON notices (person, id);

CREATE TABLE deliveries (
    notice_id TEXT NOT NULL REFERENCES notices (id),
    route INTEGER NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL,
    last_error TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    give_up_at TEXT NOT NULL,
    sent_at TEXT,
    PRIMARY KEY (notice_id, route)
);

CREATE INDEX deliveries_due ON deliveries (state, next_attempt_at);
