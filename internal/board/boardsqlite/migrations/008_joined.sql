-- When this site started reading a network: posts written before then are
-- history and never notify anyone here.
ALTER TABLE sync_state ADD COLUMN joined_at TEXT NOT NULL DEFAULT '';
