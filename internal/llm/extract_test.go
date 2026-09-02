package llm

import (
	"strings"
	"testing"

	"github.com/beda/enquiry-pipeline/internal/model"
)

func ptr(s string) *string { return &s }

const source = `Hi there,

I'm Jane Doe from Acme Widgets Ltd. We need pricing on 500 units
before Friday. Reach me at jane.doe@acme.example or 020 7946 0958.

Thanks`

func TestGroundKeepsFieldsPresentInSource(t *testing.T) {
	ex := model.Extraction{
		Contact: model.ExtractedContact{
			Name:        ptr("Jane Doe"),
			Email:       ptr("jane.doe@acme.example"),
			CompanyName: ptr("Acme Widgets Ltd"),
		},
		GroundingQuotes: map[string]string{"name": "I'm Jane Doe"},
	}
	Ground(&ex, source)
	if len(ex.UnverifiedFields) != 0 {
		t.Fatalf("unverified = %v, want none", ex.UnverifiedFields)
	}
}

// A fabricated value with no supporting quote is the case this check exists for.
func TestGroundFlagsFabricatedField(t *testing.T) {
	ex := model.Extraction{
		Contact:         model.ExtractedContact{Email: ptr("jane@totally-different.example")},
		GroundingQuotes: map[string]string{},
	}
	Ground(&ex, source)
	if !ex.Unverified("email") {
		t.Fatal("fabricated email was not flagged")
	}
	if Trusted(ex).Email != nil {
		t.Fatal("Trusted() passed an ungrounded email through to the CRM")
	}
}

// A confident-sounding quote that is not in the text must not rescue the field.
func TestGroundFlagsFabricatedQuote(t *testing.T) {
	ex := model.Extraction{
		Contact:         model.ExtractedContact{Phone: ptr("555-0100")},
		GroundingQuotes: map[string]string{"phone": "you can call me on 555-0100"},
	}
	Ground(&ex, source)
	if !ex.Unverified("phone") {
		t.Fatal("field with a fabricated quote was not flagged")
	}
}

// Line breaks and casing differ between the source and a model's copy all the
// time; that is formatting, not fabrication.
func TestGroundToleratesWhitespaceAndCase(t *testing.T) {
	ex := model.Extraction{
		Contact:         model.ExtractedContact{Name: ptr("jane   doe")},
		GroundingQuotes: map[string]string{"name": "I'm Jane Doe from Acme Widgets Ltd. We need pricing on 500 units before Friday."},
	}
	Ground(&ex, source)
	if len(ex.UnverifiedFields) != 0 {
		t.Fatalf("unverified = %v, want none (whitespace/case only)", ex.UnverifiedFields)
	}
}

// enquiry_type is a judgment, so the label itself is never expected in the text —
// only a quote the model claims to have read must actually exist.
func TestGroundClassificationLabelNotRequiredInText(t *testing.T) {
	ex := model.Extraction{
		EnquiryType:     model.TypeSales,
		Urgency:         "high",
		GroundingQuotes: map[string]string{"enquiry_type": "We need pricing on 500 units", "urgency": "before Friday"},
	}
	Ground(&ex, source)
	if len(ex.UnverifiedFields) != 0 {
		t.Fatalf("unverified = %v, want none", ex.UnverifiedFields)
	}

	bad := model.Extraction{
		EnquiryType:     model.TypeSales,
		GroundingQuotes: map[string]string{"enquiry_type": "our production line is down"},
	}
	Ground(&bad, source)
	if !bad.Unverified("enquiry_type") {
		t.Fatal("classification with an invented quote was not flagged")
	}
}

// Ground must be idempotent: re-running it (a retried worker) may not accumulate
// duplicate flags.
func TestGroundIsIdempotent(t *testing.T) {
	ex := model.Extraction{Contact: model.ExtractedContact{Email: ptr("nope@nowhere.example")}}
	Ground(&ex, source)
	Ground(&ex, source)
	if len(ex.UnverifiedFields) != 1 {
		t.Fatalf("unverified = %v, want exactly one entry", ex.UnverifiedFields)
	}
}

// Models routinely return a value with the sentence's punctuation still attached.
// An email with a trailing period will not match an existing contact, so this
// runs before both the grounding check and any CRM write.
func TestNormalizeContact(t *testing.T) {
	c := model.ExtractedContact{
		Name:        ptr("  Jane Doe.  "),
		Email:       ptr("Jane.Doe@Acme.example."),
		Phone:       ptr("020 7946 0958,"),
		CompanyName: ptr("(Acme Widgets Ltd)"),
	}
	normalizeContact(&c)

	for _, tc := range []struct {
		field string
		got   *string
		want  string
	}{
		{"name", c.Name, "Jane Doe"},
		{"email", c.Email, "jane.doe@acme.example"},
		{"phone", c.Phone, "020 7946 0958"},
		{"company_name", c.CompanyName, "Acme Widgets Ltd"},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s = %v, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// A value that is only punctuation, or not shaped like an address, must become
// nil rather than a broken CRM row.
func TestNormalizeContactDropsUnusableValues(t *testing.T) {
	c := model.ExtractedContact{
		Name:        ptr("   "),
		Email:       ptr("not-an-address"),
		Phone:       ptr("..."),
		CompanyName: ptr(""),
	}
	normalizeContact(&c)
	if c.Name != nil || c.Email != nil || c.Phone != nil || c.CompanyName != nil {
		t.Fatalf("expected all nil, got %+v", c)
	}

	// An address with no dot in the domain, or embedded whitespace, is not usable.
	for _, bad := range []string{"jane@localhost", "jane doe@acme.example", "@acme.example"} {
		c := model.ExtractedContact{Email: ptr(bad)}
		normalizeContact(&c)
		if c.Email != nil {
			t.Errorf("accepted malformed email %q", bad)
		}
	}
}

func TestValidateRejectsOutOfSchemaValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		ex   model.Extraction
	}{
		{"bad enquiry_type", model.Extraction{EnquiryType: "maybe_sales", Urgency: "low", Confidence: 0.9, IntentSummary: "x"}},
		{"bad urgency", model.Extraction{EnquiryType: "sales", Urgency: "urgent", Confidence: 0.9, IntentSummary: "x"}},
		{"confidence out of range", model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 1.4, IntentSummary: "x"}},
		{"empty summary", model.Extraction{EnquiryType: "sales", Urgency: "low", Confidence: 0.9, IntentSummary: "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(&tc.ex); err == nil {
				t.Fatal("validate accepted an out-of-schema value")
			}
		})
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	ex := model.Extraction{EnquiryType: model.TypeSupport, Urgency: "high", Confidence: 0.81, IntentSummary: "printer offline"}
	if err := validate(&ex); err != nil {
		t.Fatalf("validate rejected a valid extraction: %v", err)
	}
}

// The prompt promises the schema keys the code reads; drift between the two is
// silent and would show up as everything being flagged unverified.
func TestSchemaAndPromptAgreeOnGroundingKeys(t *testing.T) {
	for _, k := range []string{"enquiry_type", "urgency", "name", "email", "phone", "company_name"} {
		if !strings.Contains(extractionPrompt+extractionSchema["properties"].(map[string]any)["grounding_quotes"].(map[string]any)["description"].(string), k) {
			t.Errorf("grounding key %q is read by code but never named to the model", k)
		}
	}
}
