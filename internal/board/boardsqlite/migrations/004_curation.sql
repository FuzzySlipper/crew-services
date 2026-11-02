-- crew-board settling and curation: post kinds, a thread's settled links,
-- old titles and redirect, and acknowledgements of decisions.
ALTER TABLE posts ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN links TEXT NOT NULL DEFAULT '[]';
ALTER TABLE threads ADD COLUMN aliases TEXT NOT NULL DEFAULT '[]';
ALTER TABLE threads ADD COLUMN redirect_to TEXT NOT NULL DEFAULT '';
CREATE INDEX posts_by_kind ON posts(kind, id);

CREATE TABLE acks (
    post_id TEXT NOT NULL REFERENCES posts(id),
    participant_kind TEXT NOT NULL,
    participant_address TEXT NOT NULL,
    participant_site TEXT NOT NULL,
    acked_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (post_id, participant_kind, participant_address, participant_site)
);
