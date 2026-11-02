-- A notice whose post was purged is marked so: its cached text is gone, and
-- no later update from the sender can write it back.
ALTER TABLE notices ADD COLUMN purged INTEGER NOT NULL DEFAULT 0;
