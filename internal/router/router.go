// Package router holds the deterministic routing decision. No LLM call happens
// here: this is business policy, so it must be explainable, testable, and
// editable by Ops (docs/04-ARCHITECTURE.md §3).
package router

import (
	"sort"
	"strings"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// Thresholds are the gates that decide automation vs. human review.
type Thresholds struct {
	Confidence      float64
	AutoAttachMatch float64
}

// Route implements docs/04-ARCHITECTURE.md §8. Order matters and is deliberate:
// the confidence gate runs before the completeness gate, because a model that
// isn't confident in its classification isn't trustworthy about what's missing
// either (docs/03-FLOW.md).
func Route(ex model.Extraction, dup *model.DuplicateMatch, rules []model.RoutingRule, t Thresholds) model.Decision {
	// A field that failed the grounding check is not evidence of anything, so
	// treat a low-confidence-or-ungrounded classification the same way.
	if ex.Confidence < t.Confidence || ex.Unverified("enquiry_type") {
		return model.Decision{Action: model.ActionNeedsHumanReview, Reason: "low_confidence"}
	}

	if ex.EnquiryType == model.TypeJunk {
		// Archiving junk is cheap and reversible (the row and raw payload are
		// kept), so this is the one terminal path that needs no approval.
		return model.Decision{Action: model.ActionArchiveJunk, Reason: "classified_junk"}
	}

	if len(ex.MissingFields) > 0 || ex.EnquiryType == model.TypeInsufficientInfo || !identifiable(ex) {
		return model.Decision{
			Action:           model.ActionDraftClarifyingQuestion,
			Reason:           "missing_required_fields",
			DraftResponse:    true,
			RequiresApproval: true,
		}
	}

	if dup != nil {
		if dup.Score >= t.AutoAttachMatch {
			// Attach only. Merging two records is destructive and stays human
			// regardless of score (docs/04-ARCHITECTURE.md §7).
			id := dup.ContactID
			return model.Decision{
				Action:           model.ActionAttachToExistingContact,
				Reason:           "high_confidence_match",
				ContactID:        &id,
				DraftResponse:    true,
				RequiresApproval: true,
			}
		}
		cid := dup.ID
		return model.Decision{
			Action:      model.ActionFlagForHumanMergeReview,
			Reason:      "ambiguous_match",
			CandidateID: &cid,
		}
	}

	for _, r := range sortByPriority(rules) {
		if !r.Active || !r.Matches(ex) {
			continue
		}
		return model.Decision{
			Action:           model.ActionCreateOrUpdateCRM,
			Reason:           "matched_rule",
			RuleName:         r.Name,
			Team:             r.TargetTeam,
			OwnerUserID:      r.TargetUserID,
			DraftResponse:    true,
			RequiresApproval: true, // the draft still needs a human to send it
		}
	}

	// No rule matched: fail to a human rather than guessing an owner.
	return model.Decision{Action: model.ActionNeedsHumanReview, Reason: "no_matching_rule"}
}

// identifiable reports whether there is a grounded way to reach or recognise the
// sender. Without one, a CRM record is an unusable stub and a reply has nowhere
// to go, so the pipeline asks instead of guessing — regardless of whether the
// model bothered to list anything in missing_fields.
func identifiable(ex model.Extraction) bool {
	has := func(field string, v *string) bool {
		return v != nil && strings.TrimSpace(*v) != "" && !ex.Unverified(field)
	}
	return has("email", ex.Contact.Email) ||
		has("phone", ex.Contact.Phone) ||
		has("name", ex.Contact.Name)
}

// sortByPriority orders rules ascending by priority (1 beats 10), with the name
// as a tiebreaker so evaluation order is stable and reproducible in the audit log.
func sortByPriority(rules []model.RoutingRule) []model.RoutingRule {
	out := make([]model.RoutingRule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CRMRecordType maps an enquiry type to the CRM record the sync worker creates.
func CRMRecordType(enquiryType string) string {
	if enquiryType == model.TypeSupport {
		return "ticket"
	}
	return "lead"
}
