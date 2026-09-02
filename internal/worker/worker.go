// Package worker runs the pipeline stages. One enquiry is processed by one
// goroutine start to finish; concurrency comes from running several workers
// against the Postgres-backed queue (docs/04-ARCHITECTURE.md §1).
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/beda/enquiry-pipeline/internal/config"
	"github.com/beda/enquiry-pipeline/internal/ingest"
	"github.com/beda/enquiry-pipeline/internal/llm"
	"github.com/beda/enquiry-pipeline/internal/model"
	"github.com/beda/enquiry-pipeline/internal/router"
	"github.com/beda/enquiry-pipeline/internal/store"
)

type Pool struct {
	st  *store.Store
	llm *llm.Client
	cfg config.Config
	log *slog.Logger

	wake chan struct{} // the gateway nudges this so a new enquiry is picked up at once
}

func NewPool(st *store.Store, l *llm.Client, cfg config.Config, log *slog.Logger) *Pool {
	return &Pool{st: st, llm: l, cfg: cfg, log: log, wake: make(chan struct{}, 1)}
}

// Wake signals that work is available. Non-blocking: a full channel already means
// "there is work", so dropping the extra signal loses nothing.
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run starts WorkerCount goroutines and blocks until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.WorkerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.loop(ctx, id)
		}(i)
	}
	wg.Wait()
}

func (p *Pool) loop(ctx context.Context, id int) {
	log := p.log.With("worker", id)
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		// Drain the queue before going back to sleep: one wake signal may cover
		// several enquiries.
		for {
			e, err := p.st.ClaimEnquiry(ctx, p.cfg.LockTTL)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("claim failed", "err", err)
				break
			}
			if e == nil {
				break
			}
			p.process(ctx, log, e)
		}

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// process runs one enquiry through the stages in docs/03-FLOW.md. Any error
// short-circuits to retry-or-dead-letter; nothing is ever silently dropped.
func (p *Pool) process(ctx context.Context, log *slog.Logger, e *model.Enquiry) {
	log = log.With("enquiry", e.ID)
	if err := p.run(ctx, log, e); err != nil {
		p.fail(ctx, log, e, err)
	}
}

func (p *Pool) run(ctx context.Context, log *slog.Logger, e *model.Enquiry) error {
	// Stage 1 — exact duplicate. Free, deterministic, before any LLM call.
	dupID, found, err := p.st.FindRecentDuplicate(ctx, e.DedupeHash, e.ID, p.cfg.DedupeWindow)
	if err != nil {
		return err
	}
	if found {
		_ = p.st.Audit(ctx, &e.ID, "enquiry", &e.ID, "discarded_duplicate", "system:preprocessor",
			map[string]any{"original_enquiry_id": dupID, "dedupe_hash": e.DedupeHash})
		return p.st.SetStatus(ctx, e.ID, model.StatusDiscardedDuplicate, "duplicate_of:"+dupID.String())
	}

	// Stage 2 — spam heuristics. These only mark an enquiry suspicious; the LLM
	// still decides, because a real enquiry lost to a keyword is the expensive
	// failure (docs/01-PRD.md §7).
	spam := ingest.Suspicious(e)
	if spam.Suspicious {
		_ = p.st.Audit(ctx, &e.ID, "enquiry", &e.ID, "spam_heuristic_hit", "system:preprocessor", spam)
	}

	// Stage 3 — classify and extract, then verify the model's claims in code.
	ex, raw, err := p.llm.Extract(ctx, e.NormalizedText)
	if err != nil {
		return err
	}
	if err := p.st.SaveExtraction(ctx, e.ID, ex, raw); err != nil {
		return err
	}
	_ = p.st.Audit(ctx, &e.ID, "extraction_result", &e.ID, "classified", "system:classifier",
		map[string]any{
			"model_used": ex.ModelUsed, "enquiry_type": ex.EnquiryType, "confidence": ex.Confidence,
			"urgency": ex.Urgency, "escalated": ex.Escalated,
			"unverified_fields": ex.UnverifiedFields, "missing_fields": ex.MissingFields,
		})

	// Stage 4 — CRM entity matching. Only grounded fields are used, so a
	// hallucinated email cannot match or create a contact.
	trusted := llm.Trusted(ex)
	var match *model.DuplicateMatch
	if ex.EnquiryType != model.TypeJunk {
		if match, err = p.st.MatchContact(ctx, trusted, p.cfg.TrgmMatchFloor); err != nil {
			return err
		}
		if match != nil {
			resolution := "pending"
			if match.Score >= p.cfg.AutoAttachMatchThreshold {
				resolution = "auto_attached"
			}
			if match.ID, err = p.st.RecordDuplicateCandidate(ctx, e.ID, match, resolution); err != nil {
				return err
			}
			_ = p.st.Audit(ctx, &e.ID, "duplicate_match_candidate", &match.ID, "crm_match_found", "system:crm_sync", match)
		}
	}

	// Stage 5 — the deterministic routing decision. No LLM here.
	rules, err := p.st.RoutingRules(ctx)
	if err != nil {
		return err
	}
	d := router.Route(ex, match, rules, router.Thresholds{
		Confidence:      p.cfg.ConfidenceThreshold,
		AutoAttachMatch: p.cfg.AutoAttachMatchThreshold,
	})
	_ = p.st.Audit(ctx, &e.ID, "enquiry", &e.ID, "routed", "system:router", d)

	return p.act(ctx, log, e, ex, trusted, match, d)
}

// act carries out the router's decision. Every branch ends in a status a human
// can find; nothing customer-facing is sent here.
func (p *Pool) act(ctx context.Context, log *slog.Logger, e *model.Enquiry, ex model.Extraction,
	trusted model.ExtractedContact, match *model.DuplicateMatch, d model.Decision) error {

	switch d.Action {
	case model.ActionNeedsHumanReview:
		return p.st.SetStatus(ctx, e.ID, model.StatusNeedsHumanReview, d.Reason)

	case model.ActionArchiveJunk:
		return p.st.SetStatus(ctx, e.ID, model.StatusArchivedJunk, d.Reason)

	case model.ActionFlagForHumanMergeReview:
		// A merge corrupts the CRM quietly when it is wrong, so this stops here
		// and waits for a person (docs/04-ARCHITECTURE.md §7).
		return p.st.SetStatus(ctx, e.ID, model.StatusNeedsHumanReview, d.Reason)

	case model.ActionDraftClarifyingQuestion:
		body, drafter, err := p.llm.DraftClarifyingQuestion(ctx, ex, e.NormalizedText)
		if err != nil {
			return err
		}
		msgID, err := p.st.UpsertDraft(ctx, model.Message{
			EnquiryID: e.ID, Kind: model.KindClarifyingQuestion, Body: body, DraftedBy: drafter,
		})
		if err != nil {
			return err
		}
		_ = p.st.Audit(ctx, &e.ID, "message", &msgID, "clarifying_question_drafted", "system:draft_composer",
			map[string]any{"drafted_by": drafter, "missing_fields": ex.MissingFields})
		return p.st.SetStatus(ctx, e.ID, model.StatusPendingApproval, d.Reason)

	case model.ActionAttachToExistingContact, model.ActionCreateOrUpdateCRM:
		var existing *uuid.UUID
		if d.Action == model.ActionAttachToExistingContact && match != nil {
			existing = &match.ContactID
		}
		contactID, err := p.st.UpsertContact(ctx, e.ID, trusted, existing)
		if err != nil {
			return err
		}

		team, owner := d.Team, d.OwnerUserID
		if team == "" && match != nil {
			// Attach-path enquiries skipped the rules table, so resolve a team
			// from the rules the enquiry itself matches.
			if rules, err := p.st.RoutingRules(ctx); err == nil {
				for _, r := range rules {
					if r.Active && r.Matches(ex) {
						team, owner = r.TargetTeam, r.TargetUserID
						break
					}
				}
			}
		}
		if owner == nil && team != "" {
			if owner, err = p.st.PickOwner(ctx, team); err != nil {
				return err
			}
		}

		recID, err := p.st.UpsertCRMRecord(ctx, model.CRMRecord{
			Type: router.CRMRecordType(ex.EnquiryType), ContactID: contactID,
			OwnerUserID: owner, Team: team, Stage: "new",
		}, e.ID)
		if err != nil {
			return err
		}
		_ = p.st.Audit(ctx, &e.ID, "crm_record", &recID, "crm_record_upserted", "system:crm_sync",
			map[string]any{"type": router.CRMRecordType(ex.EnquiryType), "contact_id": contactID,
				"team": team, "owner_user_id": owner, "rule": d.RuleName, "attached_to_existing": existing != nil})

		body, drafter, err := p.llm.DraftReply(ctx, ex, e.NormalizedText)
		if err != nil {
			return err
		}
		msgID, err := p.st.UpsertDraft(ctx, model.Message{
			EnquiryID: e.ID, CRMRecordID: &recID, Kind: model.KindReply, Body: body, DraftedBy: drafter,
		})
		if err != nil {
			return err
		}
		_ = p.st.Audit(ctx, &e.ID, "message", &msgID, "reply_drafted", "system:draft_composer",
			map[string]any{"drafted_by": drafter, "requires_approval": true})

		// ponytail: owner notification is this audit row. Wire Slack/email here.
		_ = p.st.Audit(ctx, &e.ID, "crm_record", &recID, "owner_notified", "system:notifier",
			map[string]any{"team": team, "owner_user_id": owner})

		return p.st.SetStatus(ctx, e.ID, model.StatusPendingApproval, d.Reason)

	default:
		log.Error("unhandled routing action", "action", d.Action)
		return p.st.SetStatus(ctx, e.ID, model.StatusNeedsHumanReview, "unhandled_action")
	}
}

// fail retries with exponential backoff, then dead-letters. An enquiry that
// exhausts its budget lands in `failed` where a human can see it — never dropped
// (docs/01-PRD.md §6, docs/04-ARCHITECTURE.md §4).
func (p *Pool) fail(ctx context.Context, log *slog.Logger, e *model.Enquiry, cause error) {
	// Cancellation is a shutdown, not a processing failure: release the lock and
	// let the enquiry be reclaimed, without burning a retry.
	if errors.Is(cause, context.Canceled) && ctx.Err() != nil {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = p.st.Requeue(bg, e.ID, 0, "shutdown_requeue")
		return
	}

	log.Error("stage failed", "attempt", e.Attempts, "err", cause)
	_ = p.st.Audit(ctx, &e.ID, "enquiry", &e.ID, "processing_failed", "system:worker",
		map[string]any{"attempt": e.Attempts, "error": cause.Error()})

	if e.Attempts >= p.cfg.MaxAttempts {
		_ = p.st.SetStatus(ctx, e.ID, model.StatusFailed, truncate(cause.Error(), 400))
		_ = p.st.Audit(ctx, &e.ID, "enquiry", &e.ID, "dead_lettered", "system:worker",
			map[string]any{"attempts": e.Attempts, "error": cause.Error()})
		log.Error("dead-lettered, needs a human", "attempts", e.Attempts)
		return
	}
	backoff := time.Duration(math.Pow(2, float64(e.Attempts))) * p.cfg.PollInterval
	_ = p.st.Requeue(ctx, e.ID, backoff, truncate(cause.Error(), 400))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
