package router

import (
	"testing"

	"github.com/beda/enquiry-pipeline/internal/model"
	"github.com/google/uuid"
)

var rules = []model.RoutingRule{
	{Name: "general-sales", Condition: map[string]string{"enquiry_type": "sales"}, TargetTeam: "sales-inbound", Priority: 10, Active: true},
	{Name: "high-value-sales", Condition: map[string]string{"enquiry_type": "sales", "urgency": "high"}, TargetTeam: "enterprise-sales", Priority: 1, Active: true},
	{Name: "support-default", Condition: map[string]string{"enquiry_type": "support"}, TargetTeam: "support-l1", Priority: 10, Active: true},
	{Name: "disabled-catchall", Condition: map[string]string{"enquiry_type": "sales"}, TargetTeam: "nowhere", Priority: 0, Active: false},
}

var th = Thresholds{Confidence: 0.72, AutoAttachMatch: 0.95}

func ptr(s string) *string { return &s }

// contactable is the baseline: a grounded email, so the router's identifiability
// gate is satisfied and each test isolates the behaviour it names.
func contactable() model.ExtractedContact {
	return model.ExtractedContact{Name: ptr("Jane Doe"), Email: ptr("jane@acme.example")}
}

func good(t string, u string) model.Extraction {
	return model.Extraction{
		EnquiryType: t, Urgency: u, Confidence: 0.9,
		IntentSummary: "x", Contact: contactable(),
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		name     string
		ex       model.Extraction
		dup      *model.DuplicateMatch
		want     model.Action
		wantTeam string
		approval bool
	}{
		{name: "low confidence goes to a human before anything else",
			ex:   model.Extraction{EnquiryType: "sales", Urgency: "high", Confidence: 0.5, Contact: contactable()},
			want: model.ActionNeedsHumanReview},
		{name: "ungrounded classification is treated as untrusted",
			ex: model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.99,
				Contact: contactable(), UnverifiedFields: []string{"enquiry_type"}},
			want: model.ActionNeedsHumanReview},
		{name: "junk is archived without approval",
			ex:   good(model.TypeJunk, "low"),
			want: model.ActionArchiveJunk},
		{name: "missing fields draft a question, not a CRM record",
			ex: model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.9,
				Contact: contactable(), MissingFields: []string{"quantity"}},
			want:     model.ActionDraftClarifyingQuestion,
			approval: true},
		{name: "insufficient_info drafts a question even with no missing_fields",
			ex:       good(model.TypeInsufficientInfo, "low"),
			want:     model.ActionDraftClarifyingQuestion,
			approval: true},
		{name: "no way to reach the sender drafts a question, whatever the model claimed",
			ex: model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.9,
				IntentSummary: "x", MissingFields: nil},
			want:     model.ActionDraftClarifyingQuestion,
			approval: true},
		{name: "an ungrounded email is not a way to reach the sender",
			ex: model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.9,
				IntentSummary: "x", Contact: model.ExtractedContact{Email: ptr("made@up.example")},
				UnverifiedFields: []string{"email"}},
			want:     model.ActionDraftClarifyingQuestion,
			approval: true},
		{name: "priority wins: high-urgency sales beats general sales",
			ex:       good(model.TypeSales, "high"),
			want:     model.ActionCreateOrUpdateCRM,
			wantTeam: "enterprise-sales",
			approval: true},
		{name: "general sales falls through to the priority-10 rule",
			ex:       good(model.TypeSales, "low"),
			want:     model.ActionCreateOrUpdateCRM,
			wantTeam: "sales-inbound",
			approval: true},
		{name: "support routes to support",
			ex:       good(model.TypeSupport, "medium"),
			want:     model.ActionCreateOrUpdateCRM,
			wantTeam: "support-l1",
			approval: true},
		{name: "exact match attaches, and still needs approval to reply",
			ex:       good(model.TypeSales, "low"),
			dup:      &model.DuplicateMatch{ContactID: uuid.New(), Score: 1.0, Method: "email_exact"},
			want:     model.ActionAttachToExistingContact,
			approval: true},
		{name: "fuzzy match never auto-merges, it asks a human",
			ex:   good(model.TypeSales, "low"),
			dup:  &model.DuplicateMatch{ID: uuid.New(), ContactID: uuid.New(), Score: 0.8, Method: "trgm_name"},
			want: model.ActionFlagForHumanMergeReview},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Route(tc.ex, tc.dup, rules, th)
			if got.Action != tc.want {
				t.Fatalf("action = %q, want %q (reason %q)", got.Action, tc.want, got.Reason)
			}
			if tc.wantTeam != "" && got.Team != tc.wantTeam {
				t.Errorf("team = %q, want %q", got.Team, tc.wantTeam)
			}
			if got.RequiresApproval != tc.approval {
				t.Errorf("RequiresApproval = %v, want %v", got.RequiresApproval, tc.approval)
			}
		})
	}
}

// The routing table is Ops-editable, so an unroutable enquiry must land in front
// of a human rather than picking an arbitrary owner.
func TestRouteNoMatchingRule(t *testing.T) {
	d := Route(good(model.TypeSales, "low"), nil, nil, th)
	if d.Action != model.ActionNeedsHumanReview || d.Reason != "no_matching_rule" {
		t.Fatalf("got %+v, want needs_human_review/no_matching_rule", d)
	}
}

// An unknown condition key is an Ops typo. It must fail closed (no match) rather
// than matching everything.
func TestUnknownConditionKeyFailsClosed(t *testing.T) {
	r := model.RoutingRule{Name: "typo", Condition: map[string]string{"enquiry_typ": "sales"}, TargetTeam: "x", Active: true}
	if r.Matches(good(model.TypeSales, "low")) {
		t.Fatal("rule with unknown condition key matched")
	}
}

// Nothing customer-facing may be sent without approval: assert it for every
// action the router can produce, not just the ones enumerated above.
func TestOnlyJunkAndHumanReviewSkipApproval(t *testing.T) {
	for _, tc := range []struct {
		ex  model.Extraction
		dup *model.DuplicateMatch
	}{
		{ex: good(model.TypeSales, "high")},
		{ex: good(model.TypeSupport, "low")},
		{ex: model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.9,
			Contact: contactable(), MissingFields: []string{"quantity"}}},
		{ex: good(model.TypeSales, "low"), dup: &model.DuplicateMatch{ContactID: uuid.New(), Score: 1}},
	} {
		d := Route(tc.ex, tc.dup, rules, th)
		if d.DraftResponse && !d.RequiresApproval {
			t.Fatalf("action %q drafts a response but does not require approval", d.Action)
		}
	}
}
