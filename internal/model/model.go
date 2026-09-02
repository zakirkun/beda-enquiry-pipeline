// Package model holds the shared domain types. These mirror docs/02-ERD.md and
// the extraction schema in docs/04-ARCHITECTURE.md §8.
package model

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// Enquiry statuses. These are the states in docs/03-FLOW.md's lifecycle diagram,
// and double as the queue state: only `received` rows are claimable by a worker.
const (
	StatusReceived           = "received"
	StatusProcessing         = "processing"
	StatusDiscardedDuplicate = "discarded_duplicate"
	StatusArchivedJunk       = "archived_junk"
	StatusNeedsHumanReview   = "needs_human_review"
	StatusNeedsInfo          = "needs_info"
	StatusPendingApproval    = "pending_approval"
	StatusSent               = "sent"
	StatusClosed             = "closed"
	StatusFailed             = "failed" // dead letter: retries exhausted
)

const (
	ChannelEmail     = "email"
	ChannelWebForm   = "web_form"
	ChannelMessaging = "messaging"
)

const (
	TypeSales            = "sales"
	TypeSupport          = "support"
	TypeJunk             = "junk"
	TypeInsufficientInfo = "insufficient_info"
)

// Enquiry is the normalized shape every inbound channel is mapped to.
type Enquiry struct {
	ID               uuid.UUID `json:"id"`
	SourceChannel    string    `json:"source_channel"`
	ReceivedAt       time.Time `json:"received_at"`
	SenderIdentifier string    `json:"sender_identifier"`
	Subject          string    `json:"subject"`
	NormalizedText   string    `json:"normalized_text"`
	DedupeHash       string    `json:"dedupe_hash"`
	IdempotencyKey   string    `json:"idempotency_key"`
	Status           string    `json:"status"`
	StatusReason     string    `json:"status_reason"`
	Attempts         int       `json:"attempts"`
	RawPayload       []byte    `json:"-"`
}

// ExtractedContact is the contact block of the extraction schema. Pointers, not
// empty strings: "the model did not find an email" and "the email is blank" are
// different facts and only one of them is safe to write to the CRM.
type ExtractedContact struct {
	Name        *string `json:"name"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	CompanyName *string `json:"company_name"`
}

// Extraction is the model's structured output plus the verdicts deterministic
// code attaches to it. Nothing in ModelUsed/UnverifiedFields comes from the model.
type Extraction struct {
	EnquiryType     string            `json:"enquiry_type"`
	Confidence      float64           `json:"confidence"`
	Urgency         string            `json:"urgency"`
	IntentSummary   string            `json:"intent_summary"`
	Contact         ExtractedContact  `json:"contact"`
	MissingFields   []string          `json:"missing_fields"`
	GroundingQuotes map[string]string `json:"grounding_quotes"`

	// Set by code after the grounding check, never by the model.
	ModelUsed        string   `json:"model_used"`
	UnverifiedFields []string `json:"unverified_fields"`
	Escalated        bool     `json:"escalated"`
}

// Unverified reports whether a named field failed the grounding check and so
// must be excluded from automatic CRM writes.
func (e Extraction) Unverified(field string) bool {
	return slices.Contains(e.UnverifiedFields, field)
}

// Action is what the deterministic router decided to do with an enquiry.
type Action string

const (
	ActionNeedsHumanReview        Action = "needs_human_review"
	ActionArchiveJunk             Action = "archive_junk"
	ActionDraftClarifyingQuestion Action = "draft_clarifying_question"
	ActionAttachToExistingContact Action = "attach_to_existing_contact"
	ActionFlagForHumanMergeReview Action = "flag_for_human_merge_review"
	ActionCreateOrUpdateCRM       Action = "create_or_update_crm"
)

// Decision is the router's output. RequiresApproval is what stops the pipeline
// from sending anything on its own.
type Decision struct {
	Action           Action     `json:"action"`
	Reason           string     `json:"reason,omitempty"`
	Team             string     `json:"team,omitempty"`
	OwnerUserID      *uuid.UUID `json:"owner_user_id,omitempty"`
	RuleName         string     `json:"rule_name,omitempty"`
	ContactID        *uuid.UUID `json:"contact_id,omitempty"`
	CandidateID      *uuid.UUID `json:"candidate_id,omitempty"`
	DraftResponse    bool       `json:"draft_response"`
	RequiresApproval bool       `json:"requires_approval"`
}

// RoutingRule is policy-as-data: Ops edits these rows, not a prompt.
type RoutingRule struct {
	ID           uuid.UUID         `json:"id"`
	Name         string            `json:"name"`
	Condition    map[string]string `json:"condition"`
	TargetTeam   string            `json:"target_team"`
	TargetUserID *uuid.UUID        `json:"target_user_id,omitempty"`
	Priority     int               `json:"priority"`
	Active       bool              `json:"active"`
}

// Matches reports whether every condition key equals the corresponding
// extraction field. An unknown key never matches, so a typo in an Ops-authored
// rule fails closed (no route) instead of matching everything.
// ponytail: two matchable fields is not an expression language. Add a real
// predicate parser only when Ops needs OR/ranges/negation.
func (r RoutingRule) Matches(ex Extraction) bool {
	for k, want := range r.Condition {
		var got string
		switch k {
		case "enquiry_type":
			got = ex.EnquiryType
		case "urgency":
			got = ex.Urgency
		default:
			return false
		}
		if got != want {
			return false
		}
	}
	return true
}

// DuplicateMatch is a candidate CRM entity match found by deterministic matching.
type DuplicateMatch struct {
	ID          uuid.UUID `json:"id"`
	ContactID   uuid.UUID `json:"contact_id"`
	ContactName string    `json:"contact_name"`
	Score       float64   `json:"match_score"`
	Method      string    `json:"match_method"`
	Resolution  string    `json:"resolution"`
}

// Message is a customer-facing message. Outbound ones never reach `sent`
// without an ApprovalAction. SentAt is emitted even when null: the dashboard
// distinguishes "not sent" from "field missing".
type Message struct {
	ID          uuid.UUID  `json:"id"`
	EnquiryID   uuid.UUID  `json:"enquiry_id"`
	CRMRecordID *uuid.UUID `json:"crm_record_id"`
	Direction   string     `json:"direction"`
	Kind        string     `json:"kind"`
	Body        string     `json:"body"`
	Status      string     `json:"status"`
	DraftedBy   string     `json:"drafted_by"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at"`
}

const (
	MsgDraft           = "draft"
	MsgPendingApproval = "pending_approval"
	MsgApproved        = "approved"
	MsgSent            = "sent"
	MsgRejected        = "rejected"

	KindReply              = "reply"
	KindClarifyingQuestion = "clarifying_question"
)

// User is a dashboard actor. Role drives what they can see and approve.
type User struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Role string    `json:"role"`
	Team string    `json:"team"`
}

const (
	RoleSalesRep     = "sales_rep"
	RoleSupportAgent = "support_agent"
	RoleOpsAdmin     = "ops_admin"
	RoleManager      = "manager"
)

// AuditEntry records one automated decision or one human action.
type AuditEntry struct {
	ID         uuid.UUID  `json:"id"`
	EnquiryID  *uuid.UUID `json:"enquiry_id,omitempty"`
	EntityType string     `json:"entity_type"`
	EntityID   *uuid.UUID `json:"entity_id,omitempty"`
	Action     string     `json:"action"`
	Actor      string     `json:"actor"`
	Payload    []byte     `json:"payload"`
	CreatedAt  time.Time  `json:"created_at"`
}

// CRMRecord is a lead/opportunity/ticket.
type CRMRecord struct {
	ID          uuid.UUID  `json:"id"`
	Type        string     `json:"type"`
	ContactID   uuid.UUID  `json:"contact_id"`
	OwnerUserID *uuid.UUID `json:"owner_user_id,omitempty"`
	Team        string     `json:"team"`
	Status      string     `json:"status"`
	Stage       string     `json:"stage"`
	CreatedAt   time.Time  `json:"created_at"`
}
