package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// MaxHookAttempts is the number of failed attempts after which an outbox row
// is quarantined.
const MaxHookAttempts = 5

const (
	timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
	maxErrorBytes   = 1024
)

func formatTimestamp(t time.Time) string { return t.UTC().Format(timestampLayout) }

func nullableTimestamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTimestamp(*t)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EnqueueObservation spools an observation durably; false means a duplicate.
func (s *Store) EnqueueObservation(ctx context.Context, obs apiv1.Observation) (bool, error) {
	if obs.EventID == "" || obs.HostID == "" || obs.Provider == "" {
		return false, errors.New("event ID, host ID, and provider are required")
	}
	obs.SchemaVersion = apiv1.SchemaVersion
	envelope, err := json.Marshal(obs)
	if err != nil {
		return false, fmt.Errorf("encode hook observation: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO hook_outbox (id, received_at, envelope_json) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		obs.EventID, formatTimestamp(obs.ReceivedAt), envelope)
	if err != nil {
		return false, fmt.Errorf("enqueue hook observation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read hook enqueue result: %w", err)
	}
	return n > 0, nil
}

// PendingObservations returns unprocessed, non-quarantined observations oldest first.
func (s *Store) PendingObservations(ctx context.Context, limit int) ([]apiv1.Observation, error) {
	if limit <= 0 {
		return []apiv1.Observation{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT envelope_json FROM hook_outbox WHERE processed_at IS NULL AND attempts < ? ORDER BY received_at, id LIMIT ?`,
		MaxHookAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending hook observations: %w", err)
	}
	defer rows.Close()
	out := []apiv1.Observation{}
	for rows.Next() {
		var envelope []byte
		if err := rows.Scan(&envelope); err != nil {
			return nil, fmt.Errorf("scan pending hook observation: %w", err)
		}
		var obs apiv1.Observation
		if err := json.Unmarshal(envelope, &obs); err != nil {
			return nil, fmt.Errorf("decode pending hook observation: %w", err)
		}
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending hook observations: %w", err)
	}
	return out, nil
}

// MarkObservationProcessed completes an outbox row. It returns sql.ErrNoRows
// when no unprocessed row has that id.
func (s *Store) MarkObservationProcessed(ctx context.Context, id string, processedAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE hook_outbox SET attempts = attempts + 1, processed_at = ?, last_error = NULL WHERE id = ? AND processed_at IS NULL`,
		formatTimestamp(processedAt), id)
	if err != nil {
		return fmt.Errorf("mark hook observation processed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read hook processed result: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkObservationFailed records a failed attempt with a bounded error message.
func (s *Store) MarkObservationFailed(ctx context.Context, id string, cause error) error {
	message := "unknown processing error"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE hook_outbox SET attempts = attempts + 1, last_error = ? WHERE id = ? AND processed_at IS NULL`,
		message, id)
	if err != nil {
		return fmt.Errorf("mark hook observation failed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read hook failure result: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordObservation appends to the observation journal, idempotent by event id.
func (s *Store) RecordObservation(ctx context.Context, obs apiv1.Observation, processedAt time.Time) error {
	payload, err := json.Marshal(obs.Data)
	if err != nil {
		return fmt.Errorf("encode observation data: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO observations (
    id, schema_version, provider, host_id, native_session_id, event_type, source,
    occurred_at, received_at, processed_at, payload_json, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`,
		obs.EventID, apiv1.SchemaVersion, string(obs.Provider), obs.HostID,
		nullableString(obs.NativeSessionID), obs.EventType, string(obs.Source),
		nullableTimestamp(obs.OccurredAt), formatTimestamp(obs.ReceivedAt),
		formatTimestamp(processedAt), payload, nullableString(obs.Error))
	if err != nil {
		return fmt.Errorf("record observation: %w", err)
	}
	return nil
}

// ListEvents returns the newest journal events in chronological order,
// projected without any preview text.
func (s *Store) ListEvents(ctx context.Context, opts registry.EventListOptions) ([]apiv1.AgentEvent, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = registry.DefaultEventLimit
	}
	if limit > registry.MaxEventLimit {
		limit = registry.MaxEventLimit
	}
	query := `
SELECT o.id, COALESCE(a.id, ''), o.provider, o.event_type, o.source,
       o.occurred_at, o.received_at, o.processed_at, o.payload_json, o.error
FROM observations o
LEFT JOIN agents a
  ON a.host_id = o.host_id AND a.provider = o.provider AND a.native_session_id = o.native_session_id
WHERE 1 = 1`
	var args []any
	if opts.AgentID != "" {
		query += ` AND a.id = ?`
		args = append(args, opts.AgentID)
	}
	if opts.Provider != "" {
		query += ` AND o.provider = ?`
		args = append(args, string(opts.Provider))
	}
	query += ` ORDER BY o.received_at DESC, o.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list agent events: %w", err)
	}
	defer rows.Close()
	events := []apiv1.AgentEvent{}
	for rows.Next() {
		var (
			ev                           apiv1.AgentEvent
			provider, source             string
			occurred, processed, errText sql.NullString
			received                     string
			payload                      []byte
		)
		if err := rows.Scan(&ev.ID, &ev.AgentID, &provider, &ev.EventType, &source,
			&occurred, &received, &processed, &payload, &errText); err != nil {
			return nil, fmt.Errorf("scan agent event: %w", err)
		}
		var data apiv1.ObservationData
		if err := json.Unmarshal(payload, &data); err != nil {
			return nil, fmt.Errorf("decode agent event data: %w", err)
		}
		ev.SchemaVersion = apiv1.SchemaVersion
		ev.Provider = apiv1.Provider(provider)
		ev.Source = apiv1.ObservationSource(source)
		ev.NativeEvent = data.NativeEvent
		ev.PreviousLiveState = data.PreviousLiveState
		ev.LiveState = data.LiveState
		ev.Reason = data.Reason
		ev.Error = errText.String
		if ev.ReceivedAt, err = parseTimestamp(received); err != nil {
			return nil, err
		}
		if occurred.Valid {
			t, err := parseTimestamp(occurred.String)
			if err != nil {
				return nil, err
			}
			ev.OccurredAt = &t
		}
		if processed.Valid {
			t, err := parseTimestamp(processed.String)
			if err != nil {
				return nil, err
			}
			ev.ProcessedAt = &t
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent events: %w", err)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}

func parseTimestamp(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp %q: %w", value, err)
	}
	return t.UTC(), nil
}
