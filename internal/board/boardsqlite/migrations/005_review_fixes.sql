-- A purged write's receipt keeps its operation identity, so a retry cannot
-- write again, but no longer its result. A notice keeps the exact text it
-- was first submitted with, so a retry submits the same request.
ALTER TABLE operations ADD COLUMN purged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notices ADD COLUMN body TEXT NOT NULL DEFAULT '';
