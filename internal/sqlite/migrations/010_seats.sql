-- Seats: addresses a session holds for a while and hands over (docs/seats.md).
-- A seat's generation advances only when it is retired and created again; a
-- change of holder never advances it, so queued seat mail carries over.

CREATE TABLE seats (
    address TEXT PRIMARY KEY,
    generation INTEGER NOT NULL,
    retired INTEGER NOT NULL DEFAULT 0,
    current_holding_id INTEGER,
    last_holding_id INTEGER NOT NULL DEFAULT 0,
    charter_version INTEGER NOT NULL DEFAULT 0,
    handoff_version INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (retired IN (0, 1)),
    CHECK (retired = 0 OR current_holding_id IS NULL)
);

CREATE TABLE seat_holdings (
    seat_address TEXT NOT NULL REFERENCES seats(address),
    holding_id INTEGER NOT NULL,
    seat_generation INTEGER NOT NULL,
    holder_address TEXT NOT NULL,
    holder_adapter_id TEXT NOT NULL,
    holder_generation INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    start_note TEXT NOT NULL DEFAULT '',
    ended_at TEXT,
    end_reason TEXT NOT NULL DEFAULT '',
    end_note TEXT NOT NULL DEFAULT '',
    relieved_by INTEGER,
    PRIMARY KEY (seat_address, holding_id),
    CHECK ((ended_at IS NULL AND end_reason = '') OR
           (ended_at IS NOT NULL AND end_reason IN ('left', 'left_without_handoff', 'relieved', 'retired')))
);

CREATE INDEX seat_holdings_holder_idx ON seat_holdings(holder_address, ended_at);

CREATE TABLE seat_texts (
    seat_address TEXT NOT NULL REFERENCES seats(address),
    kind TEXT NOT NULL,
    version INTEGER NOT NULL,
    body TEXT NOT NULL,
    set_at TEXT NOT NULL,
    set_by_address TEXT NOT NULL,
    set_by_adapter_id TEXT NOT NULL,
    set_by_seats_json TEXT NOT NULL,
    holding_id INTEGER,
    PRIMARY KEY (seat_address, kind, version),
    CHECK (kind IN ('charter', 'handoff'))
);

CREATE TABLE seat_operation_receipts (
    adapter_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (adapter_id, operation_id)
);

-- A message sent as a seat has the seat as its sender address; these record
-- the real session behind it. They are empty for every other message.
ALTER TABLE messages ADD COLUMN sender_session_address TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN sender_holding_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN sender_charter_version INTEGER NOT NULL DEFAULT 0;

-- The holding a seat-mail claim was made under, for diagnosis. 0 otherwise.
ALTER TABLE deliveries ADD COLUMN claim_holding_id INTEGER NOT NULL DEFAULT 0;
