package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// SaveExtraction persists the model's structured output as its own auditable
// record, separate from the source text (docs/02-ERD.md). Re-running the
// classifier overwrites rather than accumulating rows.
func (s *Store) SaveExtraction(ctx context.Context, enquiryID uuid.UUID, ex model.Extraction, rawOutput string) error {
	b, err := json.Marshal(ex)
	if err != nil {
		return err
	}
	// A nil slice marshals to `null`, which is a scalar in jsonb and breaks
	// jsonb_array_length downstream. Always store an array.
	unverified := ex.UnverifiedFields
	if unverified == nil {
		unverified = []string{}
	}
	flags, err := json.Marshal(unverified)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO extraction_result (enquiry_id, model_used, extracted_json, raw_model_output,
		                               confidence_score, hallucination_flags)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (enquiry_id) DO UPDATE SET
			model_used=EXCLUDED.model_used, extracted_json=EXCLUDED.extracted_json,
			raw_model_output=EXCLUDED.raw_model_output, confidence_score=EXCLUDED.confidence_score,
			hallucination_flags=EXCLUDED.hallucination_flags, created_at=now()`,
		enquiryID, ex.ModelUsed, b, rawOutput, ex.Confidence, flags)
	return err
}

func (s *Store) Extraction(ctx context.Context, enquiryID uuid.UUID) (model.Extraction, error) {
	var b []byte
	err := s.pool.QueryRow(ctx,
		`SELECT extracted_json FROM extraction_result WHERE enquiry_id=$1`, enquiryID).Scan(&b)
	if err != nil {
		return model.Extraction{}, err
	}
	var ex model.Extraction
	return ex, json.Unmarshal(b, &ex)
}

// ---------- CRM entity matching ----------

// MatchContact finds the best existing contact for an extracted contact block.
// Exact identifier matches win outright; fuzzy name/company similarity is only a
// fallback and never scores high enough to auto-attach. No LLM involved — this
// has to be cheap, consistent, and explainable (docs/04-ARCHITECTURE.md §3).
func (s *Store) MatchContact(ctx context.Context, c model.ExtractedContact, trgmFloor float64) (*model.DuplicateMatch, error) {
	if c.Email != nil && strings.TrimSpace(*c.Email) != "" {
		var m model.DuplicateMatch
		err := s.pool.QueryRow(ctx,
			`SELECT id, COALESCE(name,'') FROM contact WHERE lower(email)=lower($1)`, *c.Email,
		).Scan(&m.ContactID, &m.ContactName)
		if err == nil {
			m.Score, m.Method = 1.0, "email_exact"
			return &m, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	if c.Phone != nil && normalizePhone(*c.Phone) != "" {
		var m model.DuplicateMatch
		err := s.pool.QueryRow(ctx, `
			SELECT id, COALESCE(name,'') FROM contact
			WHERE regexp_replace(COALESCE(phone,''), '[^0-9]', '', 'g') = $1
			  AND length($1) >= 7`, normalizePhone(*c.Phone),
		).Scan(&m.ContactID, &m.ContactName)
		if err == nil {
			m.Score, m.Method = 0.99, "phone_exact"
			return &m, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	// Fuzzy fallback: trigram similarity on the name, optionally boosted when the
	// company also matches. Capped below the auto-attach threshold on purpose, so
	// a fuzzy hit always lands in front of a human.
	if c.Name == nil || strings.TrimSpace(*c.Name) == "" {
		return nil, nil
	}
	company := ""
	if c.CompanyName != nil {
		company = *c.CompanyName
	}
	var m model.DuplicateMatch
	err := s.pool.QueryRow(ctx, `
		SELECT ct.id, COALESCE(ct.name,''),
		       LEAST(0.94, similarity(ct.name, $1)
		             + CASE WHEN $2 <> '' AND co.name IS NOT NULL
		                    THEN 0.15 * similarity(co.name, $2) ELSE 0 END) AS score
		FROM contact ct
		LEFT JOIN company co ON co.id = ct.company_id
		WHERE similarity(ct.name, $1) >= $3
		ORDER BY score DESC LIMIT 1`, *c.Name, company, trgmFloor,
	).Scan(&m.ContactID, &m.ContactName, &m.Score)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Method = "trgm_name"
	if company != "" {
		m.Method = "trgm_name_company"
	}
	return &m, nil
}

func normalizePhone(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RecordDuplicateCandidate logs every CRM dedupe check, matched or not, so
// duplicate handling is explainable rather than a black box (docs/02-ERD.md).
func (s *Store) RecordDuplicateCandidate(ctx context.Context, enquiryID uuid.UUID, m *model.DuplicateMatch, resolution string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO duplicate_match_candidate (enquiry_id, matched_contact_id, match_score, match_method, resolution)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (enquiry_id, matched_contact_id) DO UPDATE
			SET match_score=EXCLUDED.match_score, match_method=EXCLUDED.match_method
		RETURNING id`,
		enquiryID, m.ContactID, m.Score, m.Method, resolution).Scan(&id)
	return id, err
}

// ---------- CRM writes ----------

// UpsertContact resolves or creates the contact and its company. Only fields
// that passed the grounding check should reach here; the caller enforces that.
func (s *Store) UpsertContact(ctx context.Context, enquiryID uuid.UUID, c model.ExtractedContact, existing *uuid.UUID) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var companyID *uuid.UUID
	if c.CompanyName != nil && strings.TrimSpace(*c.CompanyName) != "" {
		domain := domainOf(c.Email)
		var id uuid.UUID
		// Match on domain when we have one (reliable), else on exact name.
		err := tx.QueryRow(ctx, `
			SELECT id FROM company
			WHERE ($1 <> '' AND lower(domain)=lower($1)) OR lower(name)=lower($2)
			ORDER BY (lower(domain)=lower($1)) DESC LIMIT 1`, domain, *c.CompanyName).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx,
				`INSERT INTO company (name, domain) VALUES ($1, NULLIF($2,'')) RETURNING id`,
				*c.CompanyName, domain).Scan(&id); err != nil {
				return uuid.Nil, err
			}
		} else if err != nil {
			return uuid.Nil, err
		}
		companyID = &id
	}

	if existing != nil {
		// Attach: fill blanks only. Never overwrite a human-known value with a
		// model-extracted one.
		if _, err := tx.Exec(ctx, `
			UPDATE contact SET
				name = COALESCE(NULLIF(name,''), $2),
				email = COALESCE(email, $3),
				phone = COALESCE(phone, $4),
				company_id = COALESCE(company_id, $5)
			WHERE id=$1`, *existing, deref(c.Name), c.Email, c.Phone, companyID); err != nil {
			return uuid.Nil, err
		}
		return *existing, tx.Commit(ctx)
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO contact (name, email, phone, company_id, source_enquiry_id)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		c.Name, c.Email, c.Phone, companyID, enquiryID).Scan(&id)
	if isUniqueViolation(err) {
		// Raced with another worker on the same email: adopt the winner.
		if err = tx.QueryRow(ctx, `SELECT id FROM contact WHERE lower(email)=lower($1)`, c.Email).Scan(&id); err != nil {
			return uuid.Nil, err
		}
	} else if err != nil {
		return uuid.Nil, err
	}
	return id, tx.Commit(ctx)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func domainOf(email *string) string {
	if email == nil {
		return ""
	}
	if i := strings.LastIndex(*email, "@"); i >= 0 && i < len(*email)-1 {
		return strings.ToLower((*email)[i+1:])
	}
	return ""
}

// UpsertCRMRecord creates the lead/ticket for an enquiry, or returns the
// existing one. The unique index on enquiry_id is what makes a retried worker
// unable to create a second record.
func (s *Store) UpsertCRMRecord(ctx context.Context, r model.CRMRecord, enquiryID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO crm_record (type, contact_id, owner_user_id, team, status, stage, enquiry_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (enquiry_id) WHERE enquiry_id IS NOT NULL DO UPDATE SET
			owner_user_id = COALESCE(crm_record.owner_user_id, EXCLUDED.owner_user_id),
			team = EXCLUDED.team, updated_at = now()
		RETURNING id`,
		r.Type, r.ContactID, r.OwnerUserID, r.Team, "open", r.Stage, enquiryID).Scan(&id)
	return id, err
}

// PickOwner selects the least-loaded active user on a team, so routing spreads
// work instead of piling it on one person. Returns nil if the team has no users.
func (s *Store) PickOwner(ctx context.Context, team string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT u.id FROM app_user u
		LEFT JOIN crm_record r ON r.owner_user_id=u.id AND r.status='open'
		WHERE u.team=$1
		GROUP BY u.id ORDER BY count(r.id), u.id LIMIT 1`, team).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ---------- routing rules ----------

func (s *Store) RoutingRules(ctx context.Context) ([]model.RoutingRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, condition_json, target_team, target_user_id, priority, active
		FROM routing_rule WHERE active ORDER BY priority, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RoutingRule
	for rows.Next() {
		var r model.RoutingRule
		var cond []byte
		if err := rows.Scan(&r.ID, &r.Name, &cond, &r.TargetTeam, &r.TargetUserID, &r.Priority, &r.Active); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(cond, &r.Condition); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
