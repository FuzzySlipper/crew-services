CREATE TABLE IF NOT EXISTS adapter_leases (
    adapter_id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL,
    lease_token TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS address_bindings (
    address TEXT PRIMARY KEY,
    adapter_id TEXT,
    target_ref TEXT,
    capabilities_json TEXT NOT NULL,
    revision INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((adapter_id IS NULL AND target_ref IS NULL) OR
           (adapter_id IS NOT NULL AND target_ref IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS address_bindings_order_idx ON address_bindings(address);
