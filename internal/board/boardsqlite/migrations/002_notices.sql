-- crew-board notices and reading: the notice outbox, subscriptions, and each
-- participant's read cursor per thread.
ALTER TABLE posts ADD COLUMN urgent INTEGER NOT NULL DEFAULT 0;

CREATE TABLE notices (
    id TEXT PRIMARY KEY,
    post_id TEXT NOT NULL REFERENCES posts(id),
    thread_id TEXT NOT NULL REFERENCES threads(id),
    recipient_kind TEXT NOT NULL,
    recipient_address TEXT NOT NULL,
    recipient_site TEXT NOT NULL,
    reason TEXT NOT NULL,
    policy TEXT NOT NULL,
    urgent INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    message_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    give_up_at TEXT NOT NULL,
    sent_at TEXT,
    UNIQUE (post_id, recipient_kind, recipient_address, recipient_site)
);
CREATE INDEX notices_pending ON notices(state, next_attempt_at);
CREATE INDEX notices_by_recipient ON notices(recipient_kind, recipient_address, recipient_site, thread_id, post_id);

CREATE TABLE subscriptions (
    participant_kind TEXT NOT NULL,
    participant_address TEXT NOT NULL,
    participant_site TEXT NOT NULL,
    target TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (participant_kind, participant_address, participant_site, target_kind, target)
);
CREATE INDEX subscriptions_by_target ON subscriptions(target_kind, target);

CREATE TABLE read_cursors (
    participant_kind TEXT NOT NULL,
    participant_address TEXT NOT NULL,
    participant_site TEXT NOT NULL,
    thread_id TEXT NOT NULL REFERENCES threads(id),
    post_id TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (participant_kind, participant_address, participant_site, thread_id)
);
