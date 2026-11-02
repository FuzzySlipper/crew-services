-- The model a session's driver reported for each revision: display only.
ALTER TABLE revisions ADD COLUMN model TEXT NOT NULL DEFAULT '';
