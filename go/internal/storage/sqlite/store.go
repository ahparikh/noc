// Package sqlite is the durable SQLite-backed registry, hook outbox, and
// observation journal.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers driver "sqlite"

	"noc.local/noc/internal/config"
	"noc.local/noc/internal/identity"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

var (
	_ registry.Store            = (*Store)(nil)
	_ registry.Reader           = (*Store)(nil)
	_ registry.Namer            = (*Store)(nil)
	_ registry.Forgetter        = (*Store)(nil)
	_ registry.Pruner           = (*Store)(nil)
	_ registry.EventReader      = (*Store)(nil)
	_ registry.ObservationStore = (*Store)(nil)
)

// Store is the SQLite registry. All in-process access serializes on one connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path, applies pragmas and
// migrations, and restricts the file to its owner.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("SQLite path is required")
	}
	if err := config.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to SQLite database: %w", err)
	}
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure SQLite database: %w", err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure SQLite database %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close SQLite database: %w", err)
	}
	return nil
}

// HostID returns the stable identity of this database, creating it on first use.
func (s *Store) HostID(ctx context.Context) (string, error) {
	const query = `SELECT value FROM metadata WHERE key = 'host_id'`
	var id string
	err := s.db.QueryRowContext(ctx, query).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read host identity: %w", err)
	}
	generated, err := identity.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate host identity: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO metadata (key, value, updated_at) VALUES ('host_id', ?, ?) ON CONFLICT(key) DO NOTHING`,
		generated, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", fmt.Errorf("persist host identity: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, query).Scan(&id); err != nil {
		return "", fmt.Errorf("confirm host identity: %w", err)
	}
	return id, nil
}

// List returns agents ordered by display state then id.
func (s *Store) List(ctx context.Context, opts registry.ListOptions) ([]apiv1.Agent, error) {
	query := `SELECT snapshot_json, user_name FROM agents`
	if !opts.IncludeNotLive {
		query += ` WHERE live_state <> 'not_live'`
	}
	query += ` ORDER BY display_state, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	agents := []apiv1.Agent{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agents: %w", err)
	}
	return agents, nil
}

// Get returns one agent by id.
func (s *Store) Get(ctx context.Context, id string) (apiv1.Agent, error) {
	return s.getWhere(ctx, `id = ?`, id)
}

// GetByNativeIdentity returns the agent for a provider-native session.
func (s *Store) GetByNativeIdentity(ctx context.Context, hostID string, provider apiv1.Provider, nativeSessionID string) (apiv1.Agent, error) {
	return s.getWhere(ctx, `host_id = ? AND provider = ? AND native_session_id = ?`, hostID, string(provider), nativeSessionID)
}

func (s *Store) getWhere(ctx context.Context, where string, args ...any) (apiv1.Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT snapshot_json, user_name FROM agents WHERE `+where, args...)
	agent, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return apiv1.Agent{}, registry.ErrNotFound
	}
	return agent, err
}

// Upsert stores the agent snapshot keyed by id. The incoming user name is
// ignored; the stored label survives unless the provider renamed the session.
func (s *Store) Upsert(ctx context.Context, agent apiv1.Agent) error {
	if agent.ID == "" || agent.HostID == "" || agent.Provider == "" || agent.NativeSessionID == "" {
		return errors.New("agent ID, host ID, provider, and native session ID are required")
	}
	agent.SchemaVersion = apiv1.SchemaVersion
	agent.DisplayState = apiv1.DeriveDisplayState(agent)
	agent.Bucket = apiv1.DeriveBucket(agent)
	agent.UserName = ""
	snapshot, err := json.Marshal(agent)
	if err != nil {
		return fmt.Errorf("encode agent: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agents (
    id, host_id, provider, native_session_id, live_state,
    display_state, last_seen_at, snapshot_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    host_id = excluded.host_id,
    provider = excluded.provider,
    native_session_id = excluded.native_session_id,
    live_state = excluded.live_state,
    display_state = excluded.display_state,
    last_seen_at = excluded.last_seen_at,
    user_name = CASE
        WHEN ? <> '' AND ? <> COALESCE(json_extract(agents.snapshot_json, '$.name'), '')
        THEN ''
        ELSE agents.user_name
    END,
    snapshot_json = excluded.snapshot_json`,
		agent.ID, agent.HostID, string(agent.Provider), agent.NativeSessionID,
		string(agent.LiveState), string(agent.DisplayState),
		agent.LastSeenAt.UTC().Format(time.RFC3339Nano), snapshot,
		agent.Name, agent.Name)
	if err != nil {
		return fmt.Errorf("upsert agent: %w", err)
	}
	return nil
}

// PruneRetired deletes ended, unlabeled rows that were never engaged or were
// last seen before endedBefore, returning how many were deleted.
func (s *Store) PruneRetired(ctx context.Context, endedBefore time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM agents
WHERE live_state = ?
  AND (user_name IS NULL OR TRIM(user_name) = '')
  AND (
    last_seen_at < ?
    OR (
      TRIM(COALESCE(json_extract(snapshot_json, '$.name'), '')) = ''
      AND TRIM(COALESCE(json_extract(snapshot_json, '$.initial_request.text'), '')) = ''
    )
  )`, string(apiv1.LiveStateNotLive), endedBefore.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("prune retired agents: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune retired agents: %w", err)
	}
	return int(n), nil
}

// Forget deletes one registry row; the observation journal is untouched.
func (s *Store) Forget(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("forget agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm forgotten agent: %w", err)
	}
	if n == 0 {
		return registry.ErrNotFound
	}
	return nil
}

// SetUserName stores the user label verbatim; an empty name clears it.
func (s *Store) SetUserName(ctx context.Context, id, name string) (apiv1.Agent, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET user_name = ? WHERE id = ?`, name, id)
	if err != nil {
		return apiv1.Agent{}, fmt.Errorf("set agent user name: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return apiv1.Agent{}, fmt.Errorf("confirm agent user name: %w", err)
	}
	if n == 0 {
		return apiv1.Agent{}, registry.ErrNotFound
	}
	return s.Get(ctx, id)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgent(row rowScanner) (apiv1.Agent, error) {
	var (
		snapshot []byte
		userName string
	)
	if err := row.Scan(&snapshot, &userName); err != nil {
		return apiv1.Agent{}, err
	}
	var agent apiv1.Agent
	if err := json.Unmarshal(snapshot, &agent); err != nil {
		return apiv1.Agent{}, fmt.Errorf("decode stored agent: %w", err)
	}
	agent.UserName = userName
	agent.Bucket = apiv1.DeriveBucket(agent)
	return agent, nil
}
