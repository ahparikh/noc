package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    host_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    native_session_id TEXT NOT NULL,
    live_state TEXT NOT NULL,
    display_state TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    snapshot_json BLOB NOT NULL,
    UNIQUE (host_id, provider, native_session_id)
);

CREATE INDEX agents_live_display_idx
    ON agents (live_state, display_state, id);

CREATE TABLE observations (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL,
    provider TEXT NOT NULL,
    host_id TEXT NOT NULL,
    native_session_id TEXT,
    event_type TEXT NOT NULL,
    source TEXT NOT NULL,
    occurred_at TEXT,
    received_at TEXT NOT NULL,
    processed_at TEXT,
    payload_json BLOB NOT NULL,
    error TEXT
);

CREATE INDEX observations_session_received_idx
    ON observations (provider, native_session_id, received_at);

CREATE TABLE hook_outbox (
    id TEXT PRIMARY KEY,
    received_at TEXT NOT NULL,
    envelope_json BLOB NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    processed_at TEXT,
    last_error TEXT
);

CREATE INDEX hook_outbox_pending_idx
    ON hook_outbox (processed_at, received_at);
`,
	},
	{
		version: 2,
		sql:     `CREATE INDEX observations_received_idx ON observations (received_at DESC, id DESC);`,
	},
	{
		version: 3,
		sql:     `ALTER TABLE agents ADD COLUMN user_name TEXT NOT NULL DEFAULT '';`,
	},
}

// migrate applies every migration newer than the recorded version, one
// transaction per migration. A database newer than this binary is accepted.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
		return fmt.Errorf("create schema migration table: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}
