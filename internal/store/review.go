package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// UpsertDraft stores a drafted message as pending_approval. The unique index on
// (enquiry_id, kind) means a retried worker updates its own draft instead of
// queueing a second one for the same enquiry.
// A draft that a human already acted on is never overwritten.
func (s *Store) UpsertDraft(ctx context.Context, m model.Message) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO message (crm_record_id, enquiry_id, direction, kind, body, status, drafted_by)
		VALUES ($1,$2,'outbound',$3,$4,$5,$6)
		ON CONFLICT (enquiry_id, kind) WHERE direction='outbound' DO UPDATE SET
			body = CASE WHEN message.status IN ('draft','pending_approval') THEN EXCLUDED.body ELSE message.body END,
			crm_record_id = COALESCE(EXCLUDED.crm_record_id, message.crm_record_id),
			drafted_by = CASE WHEN message.status IN ('draft','pending_approval') THEN EXCLUDED.drafted_by ELSE message.drafted_by END
		RETURNING id`,
		m.CRMRecordID, m.EnquiryID, m.Kind, m.Body, model.MsgPendingApproval, m.DraftedBy).Scan(&id)
	return id, err
}

// ErrNotPending means the message was already approved, sent, or rejected. It is
// what stops a double-click on Approve from sending twice.
var ErrNotPending = errors.New("message is not pending approval")

// Approve records the human decision and, on approval, hands the message to the
// send step. Decision and send are one transaction: a message can never be sent
// without its ApprovalAction, and never approved twice.
func (s *Store) Approve(ctx context.Context, messageID, actorID uuid.UUID, decision, editedBody, notes string) error {
	switch decision {
	case "approved", "approved_with_edit", "rejected":
	default:
		return fmt.Errorf("invalid decision %q", decision)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Lock the row so two reviewers cannot both approve the same draft.
	var status, kind string
	var enquiryID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT status, kind, enquiry_id FROM message WHERE id=$1 FOR UPDATE`, messageID,
	).Scan(&status, &kind, &enquiryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("message %s not found", messageID)
	}
	if err != nil {
		return err
	}
	if status != model.MsgPendingApproval && status != model.MsgDraft {
		return fmt.Errorf("%w: status=%s", ErrNotPending, status)
	}

	newStatus, enquiryStatus := model.MsgSent, model.StatusSent
	if decision == "rejected" {
		newStatus, enquiryStatus = model.MsgRejected, model.StatusNeedsHumanReview
	}

	// ponytail: "sending" is this status flip plus the audit row. Wire a real
	// SMTP/messaging client into this transaction boundary when there is one.
	if _, err := tx.Exec(ctx, `
		UPDATE message SET
			body = CASE WHEN $3 <> '' THEN $3 ELSE body END,
			status = $2,
			drafted_by = CASE WHEN $3 <> '' THEN 'user:' || $4::text ELSE drafted_by END,
			sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE NULL END
		WHERE id=$1`, messageID, newStatus, editedBody, actorID); err != nil {
		return err
	}

	var approvalID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO approval_action (message_id, actor_user_id, decision, notes)
		VALUES ($1,$2,$3,NULLIF($4,'')) RETURNING id`,
		messageID, actorID, decision, notes).Scan(&approvalID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE enquiry SET status=$2, status_reason=$3 WHERE id=$1`,
		enquiryID, enquiryStatus, "human_"+decision); err != nil {
		return err
	}

	// Audit inside the same transaction: the trail cannot survive a rollback of
	// the action it describes, and vice versa.
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log_entry (enquiry_id, entity_type, entity_id, action, actor, payload_snapshot_ref)
		VALUES ($1,'message',$2,$3,$4,jsonb_build_object('decision',$5::text,'edited',$6::bool,'notes',$7::text,'approval_id',$8::text))`,
		enquiryID, messageID, "message_"+newStatus, "user:"+actorID.String(),
		decision, editedBody != "", notes, approvalID.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResolveDuplicate records a human merge/separate decision. Merging is not
// implemented on purpose (docs/04-ARCHITECTURE.md §7): a human either attaches
// the enquiry to the existing contact or declares it a separate person.
func (s *Store) ResolveDuplicate(ctx context.Context, candidateID, actorID uuid.UUID, resolution string) (uuid.UUID, uuid.UUID, error) {
	switch resolution {
	case "attached", "separate":
	default:
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid resolution %q", resolution)
	}
	var enquiryID, contactID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE duplicate_match_candidate
		SET resolution=$2, resolved_by=$3
		WHERE id=$1 AND resolution='pending'
		RETURNING enquiry_id, matched_contact_id`, candidateID, resolution, actorID).Scan(&enquiryID, &contactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, fmt.Errorf("candidate %s not found or already resolved", candidateID)
	}
	return enquiryID, contactID, err
}

// ---------- dashboard reads ----------

// QueueItem is one row of the review dashboard.
type QueueItem struct {
	EnquiryID     uuid.UUID  `json:"enquiry_id"`
	Subject       string     `json:"subject"`
	Sender        string     `json:"sender_identifier"`
	Channel       string     `json:"source_channel"`
	Status        string     `json:"status"`
	StatusReason  string     `json:"status_reason"`
	ReceivedAt    string     `json:"received_at"`
	EnquiryType   *string    `json:"enquiry_type"`
	Urgency       *string    `json:"urgency"`
	Confidence    *float64   `json:"confidence"`
	IntentSummary *string    `json:"intent_summary"`
	Team          *string    `json:"team"`
	OwnerName     *string    `json:"owner_name"`
	MessageID     *uuid.UUID `json:"message_id"`
	MessageKind   *string    `json:"message_kind"`
	MessageStatus *string    `json:"message_status"`
	Unverified    int        `json:"unverified_count"`
}

// Queue lists enquiries for the dashboard. `statuses` empty means all.
// `team` scopes the list to one team's records; "" means unscoped. Enquiries with
// no team yet (unroutable, or awaiting a clarifying question) are only visible
// unscoped, i.e. to Ops — docs/01-PRD.md §4 makes monitoring the review queue an
// Ops job, and a rep seeing every other team's unrouted work is just noise.
func (s *Store) Queue(ctx context.Context, statuses []string, team string, limit int) ([]QueueItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, COALESCE(e.subject,''), e.sender_identifier, e.source_channel,
		       e.status, COALESCE(e.status_reason,''), e.received_at::text,
		       x.extracted_json->>'enquiry_type', x.extracted_json->>'urgency',
		       x.confidence_score, x.extracted_json->>'intent_summary',
		       r.team, u.name, m.id, m.kind, m.status,
		       CASE WHEN jsonb_typeof(x.hallucination_flags)='array'
		            THEN jsonb_array_length(x.hallucination_flags) ELSE 0 END
		FROM enquiry e
		LEFT JOIN extraction_result x ON x.enquiry_id = e.id
		LEFT JOIN crm_record r ON r.enquiry_id = e.id
		LEFT JOIN app_user u ON u.id = r.owner_user_id
		LEFT JOIN message m ON m.enquiry_id = e.id AND m.direction='outbound'
		WHERE (cardinality($1::text[]) = 0 OR e.status = ANY($1))
		  AND ($2 = '' OR r.team = $2)
		ORDER BY e.received_at DESC
		LIMIT $3`, statuses, team, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueItem{}
	for rows.Next() {
		var q QueueItem
		if err := rows.Scan(&q.EnquiryID, &q.Subject, &q.Sender, &q.Channel, &q.Status,
			&q.StatusReason, &q.ReceivedAt, &q.EnquiryType, &q.Urgency, &q.Confidence,
			&q.IntentSummary, &q.Team, &q.OwnerName, &q.MessageID, &q.MessageKind,
			&q.MessageStatus, &q.Unverified); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MessageTeam returns the team that owns the CRM record behind a message, or ""
// when the enquiry has no record yet. Used to stop one team from approving
// another team's outbound message.
func (s *Store) MessageTeam(ctx context.Context, messageID uuid.UUID) (string, error) {
	var team string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(r.team,'')
		FROM message m
		LEFT JOIN crm_record r ON r.id = m.crm_record_id
		WHERE m.id=$1`, messageID).Scan(&team)
	return team, err
}

// EnquiryDetail is everything a reviewer needs on one screen: the customer's own
// words, what the model said, what code decided, and the full audit trail.
type EnquiryDetail struct {
	Enquiry    model.Enquiry           `json:"enquiry"`
	Extraction *model.Extraction       `json:"extraction"`
	RawOutput  string                  `json:"raw_model_output"`
	CRMRecord  *model.CRMRecord        `json:"crm_record"`
	OwnerName  *string                 `json:"owner_name"`
	Contact    *model.ExtractedContact `json:"contact"`
	Messages   []model.Message         `json:"messages"`
	Duplicates []model.DuplicateMatch  `json:"duplicate_candidates"`
	Audit      []model.AuditEntry      `json:"audit"`
}

func (s *Store) EnquiryDetail(ctx context.Context, id uuid.UUID) (*EnquiryDetail, error) {
	var d EnquiryDetail
	err := s.pool.QueryRow(ctx, `
		SELECT id, source_channel, received_at, sender_identifier, COALESCE(subject,''),
		       normalized_text, dedupe_hash, idempotency_key, status,
		       COALESCE(status_reason,''), attempts
		FROM enquiry WHERE id=$1`, id).Scan(
		&d.Enquiry.ID, &d.Enquiry.SourceChannel, &d.Enquiry.ReceivedAt, &d.Enquiry.SenderIdentifier,
		&d.Enquiry.Subject, &d.Enquiry.NormalizedText, &d.Enquiry.DedupeHash,
		&d.Enquiry.IdempotencyKey, &d.Enquiry.Status, &d.Enquiry.StatusReason, &d.Enquiry.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	if ex, err := s.Extraction(ctx, id); err == nil {
		d.Extraction = &ex
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(raw_model_output,'') FROM extraction_result WHERE enquiry_id=$1`, id).Scan(&d.RawOutput)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var rec model.CRMRecord
	var contact model.ExtractedContact
	err = s.pool.QueryRow(ctx, `
		SELECT r.id, r.type, r.contact_id, r.owner_user_id, COALESCE(r.team,''), r.status,
		       COALESCE(r.stage,''), r.created_at, u.name, c.name, c.email, c.phone, co.name
		FROM crm_record r
		JOIN contact c ON c.id = r.contact_id
		LEFT JOIN company co ON co.id = c.company_id
		LEFT JOIN app_user u ON u.id = r.owner_user_id
		WHERE r.enquiry_id=$1`, id).Scan(
		&rec.ID, &rec.Type, &rec.ContactID, &rec.OwnerUserID, &rec.Team, &rec.Status,
		&rec.Stage, &rec.CreatedAt, &d.OwnerName, &contact.Name, &contact.Email, &contact.Phone, &contact.CompanyName)
	if err == nil {
		d.CRMRecord, d.Contact = &rec, &contact
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	msgRows, err := s.pool.Query(ctx, `
		SELECT id, crm_record_id, direction, kind, body, status, drafted_by, created_at, sent_at
		FROM message WHERE enquiry_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer msgRows.Close()
	d.Messages = []model.Message{}
	for msgRows.Next() {
		m := model.Message{EnquiryID: id}
		if err := msgRows.Scan(&m.ID, &m.CRMRecordID, &m.Direction, &m.Kind, &m.Body,
			&m.Status, &m.DraftedBy, &m.CreatedAt, &m.SentAt); err != nil {
			return nil, err
		}
		d.Messages = append(d.Messages, m)
	}
	if err := msgRows.Err(); err != nil {
		return nil, err
	}

	dupRows, err := s.pool.Query(ctx, `
		SELECT d.id, d.matched_contact_id, COALESCE(c.name,''), d.match_score, d.match_method, d.resolution
		FROM duplicate_match_candidate d
		JOIN contact c ON c.id = d.matched_contact_id
		WHERE d.enquiry_id=$1 ORDER BY d.match_score DESC`, id)
	if err != nil {
		return nil, err
	}
	defer dupRows.Close()
	d.Duplicates = []model.DuplicateMatch{}
	for dupRows.Next() {
		var m model.DuplicateMatch
		if err := dupRows.Scan(&m.ID, &m.ContactID, &m.ContactName, &m.Score, &m.Method, &m.Resolution); err != nil {
			return nil, err
		}
		d.Duplicates = append(d.Duplicates, m)
	}
	if err := dupRows.Err(); err != nil {
		return nil, err
	}

	if d.Audit, err = s.AuditTrail(ctx, id); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) Users(ctx context.Context) ([]model.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, role, team FROM app_user ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.User{}
	for rows.Next() {
		var u model.User
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.Team); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) User(ctx context.Context, id uuid.UUID) (model.User, error) {
	var u model.User
	err := s.pool.QueryRow(ctx, `SELECT id, name, role, team FROM app_user WHERE id=$1`, id).
		Scan(&u.ID, &u.Name, &u.Role, &u.Team)
	return u, err
}

// Stats backs the dashboard's metrics strip (docs/01-PRD.md §7).
func (s *Store) Stats(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM enquiry GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byStatus := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		byStatus[st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out["by_status"] = byStatus

	var total, escalated, unverified int
	var medianSeconds *float64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE (extracted_json->>'escalated')::bool),
		       count(*) FILTER (WHERE jsonb_typeof(hallucination_flags)='array'
		                          AND jsonb_array_length(hallucination_flags) > 0)
		FROM extraction_result`).Scan(&total, &escalated, &unverified); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (m.created_at - e.received_at)))
		FROM message m JOIN enquiry e ON e.id = m.enquiry_id WHERE m.direction='outbound'`).Scan(&medianSeconds); err != nil {
		return nil, err
	}
	out["extractions"] = total
	out["escalated_to_tier2"] = escalated
	out["with_unverified_fields"] = unverified
	out["median_seconds_to_draft"] = medianSeconds
	return out, nil
}
