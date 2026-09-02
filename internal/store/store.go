// Package store is the only package that talks to Postgres. Every mutation that
// matters also writes an audit row, in the same transaction where possible, so
// the audit trail cannot drift from the data it describes.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/beda/enquiry-pipeline/internal/model"
)

//go:embed schema.sql
var schemaFS embed.FS

// ErrDuplicate means a unique constraint rejected the write. Callers treat this
// as "someone already did this" rather than an error — that is what makes
// retried queue messages idempotent.
var ErrDuplicate = errors.New("duplicate")

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	// Retry the first ping: in docker-compose the API often starts before
	// Postgres finishes its own init.
	var pingErr error
	for i := 0; i < 30; i++ {
		if pingErr = pool.Ping(ctx); pingErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if pingErr != nil {
		return nil, fmt.Errorf("postgres unreachable: %w", pingErr)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Migrate applies schema.sql. It is idempotent (CREATE ... IF NOT EXISTS).
func (s *Store) Migrate(ctx context.Context) error {
	b, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, string(b)); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// ---------- enquiry ingest ----------

// InsertEnquiry stores a normalized enquiry. It returns ErrDuplicate if the
// idempotency key was already used, so a redelivered webhook is a no-op.
func (s *Store) InsertEnquiry(ctx context.Context, e *model.Enquiry) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO enquiry (source_channel, sender_identifier, subject, normalized_text,
		                     raw_payload, dedupe_hash, idempotency_key, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, received_at`,
		e.SourceChannel, e.SenderIdentifier, e.Subject, e.NormalizedText,
		e.RawPayload, e.DedupeHash, e.IdempotencyKey, model.StatusReceived,
	).Scan(&e.ID, &e.ReceivedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

// EnquiryByIdempotencyKey lets the webhook return the original enquiry id on a
// redelivery instead of an error.
func (s *Store) EnquiryByIdempotencyKey(ctx context.Context, key string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM enquiry WHERE idempotency_key=$1`, key).Scan(&id)
	return id, err
}

// FindRecentDuplicate looks for an earlier enquiry with the same content hash
// inside the dedupe window. Deterministic, runs before any LLM call.
func (s *Store) FindRecentDuplicate(ctx context.Context, hash string, exclude uuid.UUID, window time.Duration) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM enquiry
		WHERE dedupe_hash=$1 AND id<>$2 AND received_at > now() - $3::interval
		ORDER BY received_at ASC LIMIT 1`,
		hash, exclude, window.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// ---------- queue ----------

// ClaimEnquiry atomically takes the next due enquiry and locks it for this
// worker. SKIP LOCKED is what makes several workers safe against each other.
// ponytail: Postgres-as-queue. Swap for NATS JetStream when throughput needs
// fan-out across machines; the Claim/Release surface stays the same.
func (s *Store) ClaimEnquiry(ctx context.Context, lockTTL time.Duration) (*model.Enquiry, error) {
	var e model.Enquiry
	err := s.pool.QueryRow(ctx, `
		UPDATE enquiry SET status=$1, attempts=attempts+1, locked_until=now()+$2::interval
		WHERE id = (
			SELECT id FROM enquiry
			WHERE (status=$3 AND next_attempt_at<=now())
			   OR (status=$1 AND locked_until < now())
			ORDER BY received_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, source_channel, received_at, sender_identifier,
		          COALESCE(subject,''), normalized_text, dedupe_hash, attempts`,
		model.StatusProcessing, lockTTL.String(), model.StatusReceived,
	).Scan(&e.ID, &e.SourceChannel, &e.ReceivedAt, &e.SenderIdentifier,
		&e.Subject, &e.NormalizedText, &e.DedupeHash, &e.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// SetStatus moves an enquiry to a terminal or waiting state and clears its lock.
func (s *Store) SetStatus(ctx context.Context, id uuid.UUID, status, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE enquiry SET status=$2, status_reason=$3, locked_until=NULL WHERE id=$1`,
		id, status, reason)
	return err
}

// Requeue schedules a retry with backoff. Called when a stage fails but the
// retry budget is not yet spent.
func (s *Store) Requeue(ctx context.Context, id uuid.UUID, in time.Duration, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE enquiry SET status=$2, status_reason=$3, locked_until=NULL,
		       next_attempt_at = now() + $4::interval
		WHERE id=$1`, id, model.StatusReceived, reason, in.String())
	return err
}

// ---------- audit ----------

// Audit appends an immutable audit row. Never returns early on a marshal error:
// an unloggable payload still gets an entry, just without the snapshot.
func (s *Store) Audit(ctx context.Context, enquiryID *uuid.UUID, entityType string, entityID *uuid.UUID, action, actor string, payload any) error {
	var snap []byte
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			snap = b
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log_entry (enquiry_id, entity_type, entity_id, action, actor, payload_snapshot_ref)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		enquiryID, entityType, entityID, action, actor, snap)
	return err
}

// AuditTrail returns every entry for one enquiry, oldest first.
func (s *Store) AuditTrail(ctx context.Context, enquiryID uuid.UUID) ([]model.AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, entity_type, entity_id, action, actor,
		       COALESCE(payload_snapshot_ref,'null'::jsonb), created_at
		FROM audit_log_entry WHERE enquiry_id=$1 ORDER BY created_at, id`, enquiryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		a := model.AuditEntry{EnquiryID: &enquiryID}
		if err := rows.Scan(&a.ID, &a.EntityType, &a.EntityID, &a.Action, &a.Actor, &a.Payload, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
