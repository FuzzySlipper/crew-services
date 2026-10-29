-- The session projection and its event log are removed (#10456). Nothing had
-- written them since 2026-09-04 and their only client, the retired dsh-crew
-- crew-messaging plugin, is gone. Session addresses are bindings and are kept.
-- Children are dropped first, so foreign keys stay satisfied without turning
-- them off; their indexes go with the tables.
DROP TABLE IF EXISTS session_event_operation_receipts;
DROP TABLE IF EXISTS session_events;
DROP TABLE IF EXISTS sessions;
