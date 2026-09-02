package ingest

import (
	"strings"
	"testing"

	"github.com/beda/enquiry-pipeline/internal/model"
)

func mustNormalize(t *testing.T, raw string) *model.Enquiry {
	t.Helper()
	e, err := Normalize([]byte(raw))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return e
}

func TestNormalizeEmail(t *testing.T) {
	e := mustNormalize(t, `{"channel":"email","from":"Jane Doe <Jane.Doe@Acme.example>",
		"subject":"Pricing for 500 units","body":"Hi, what is your price for 500 units?"}`)

	if e.SenderIdentifier != "jane.doe@acme.example" {
		t.Errorf("sender = %q, want the bare lowercased address", e.SenderIdentifier)
	}
	if !strings.Contains(e.NormalizedText, "Subject: Pricing for 500 units") {
		t.Errorf("subject missing from normalized text:\n%s", e.NormalizedText)
	}
	if e.IdempotencyKey == "" || e.DedupeHash == "" {
		t.Error("dedupe hash and idempotency key must always be set")
	}
}

func TestNormalizeWebFormIncludesTypedFields(t *testing.T) {
	e := mustNormalize(t, `{"channel":"web_form","name":"Sam Patel","email":"sam@bigco.example",
		"phone":"020 7946 0958","company":"BigCo","message":"Need a demo next week."}`)

	for _, want := range []string{"Name: Sam Patel", "Email: sam@bigco.example", "Phone: 020 7946 0958", "Company: BigCo", "Need a demo next week."} {
		if !strings.Contains(e.NormalizedText, want) {
			t.Errorf("normalized text missing %q:\n%s", want, e.NormalizedText)
		}
	}
	// The classifier must be able to quote these verbatim for the grounding check.
	if e.SenderIdentifier != "sam@bigco.example" {
		t.Errorf("sender = %q", e.SenderIdentifier)
	}
}

func TestNormalizeMessaging(t *testing.T) {
	e := mustNormalize(t, `{"channel":"messaging","sender_handle":"@sam_p","text":"do you ship to Ireland?"}`)
	if e.SenderIdentifier != "@sam_p" || !strings.Contains(e.NormalizedText, "Ireland") {
		t.Fatalf("got sender %q text %q", e.SenderIdentifier, e.NormalizedText)
	}
}

// Trust-boundary validation: bad payloads are rejected, not stored as empty
// enquiries that later dead-letter.
func TestNormalizeRejectsBadPayloads(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"not json", `{`},
		{"unknown channel", `{"channel":"carrier_pigeon","from":"a@b.example","body":"hi"}`},
		{"no sender", `{"channel":"email","body":"hello"}`},
		{"empty body", `{"channel":"email","from":"a@b.example","body":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Normalize([]byte(tc.raw)); err == nil {
				t.Fatal("Normalize accepted an invalid payload")
			}
		})
	}
}

// Dedupe is the cheapest gate in the pipeline; it has to survive the formatting
// noise real clients add, and it has to keep distinct enquiries distinct.
func TestDedupeHash(t *testing.T) {
	base := `{"channel":"email","from":"jane@acme.example","subject":"Pricing","body":"How much for 500 units?"}`
	variants := map[string]string{
		"whitespace":     `{"channel":"email","from":"jane@acme.example","subject":"Pricing","body":"How  much   for 500 units?"}`,
		"crlf":           "{\"channel\":\"email\",\"from\":\"jane@acme.example\",\"subject\":\"Pricing\",\"body\":\"How much for 500 units?\\r\\n\"}",
		"sender casing":  `{"channel":"email","from":"JANE@ACME.example","subject":"Pricing","body":"How much for 500 units?"}`,
		"display name":   `{"channel":"email","from":"Jane <jane@acme.example>","subject":"Pricing","body":"How much for 500 units?"}`,
		"zero-width pad": "{\"channel\":\"email\",\"from\":\"jane@acme.example\",\"subject\":\"Pricing\",\"body\":\"How much for 500\\u200b units?\"}",
	}
	want := mustNormalize(t, base).DedupeHash
	for name, raw := range variants {
		if got := mustNormalize(t, raw).DedupeHash; got != want {
			t.Errorf("%s: hash differs from the original, duplicate would slip through", name)
		}
	}

	different := mustNormalize(t, `{"channel":"email","from":"jane@acme.example","subject":"Pricing","body":"How much for 900 units?"}`)
	if different.DedupeHash == want {
		t.Error("different enquiries collided on the dedupe hash")
	}
}

// A long thread must hash and classify on its newest message, not its history.
func TestTrimQuotedReply(t *testing.T) {
	e := mustNormalize(t, `{"channel":"email","from":"jane@acme.example","subject":"Re: Pricing",
		"body":"Any update on this?\n\nOn Mon, 3 Mar 2025 at 09:00, BEDA Team wrote:\n> Thanks for getting in touch\n> we will check stock levels"}`)
	if strings.Contains(e.NormalizedText, "stock levels") {
		t.Errorf("quoted history was not trimmed:\n%s", e.NormalizedText)
	}
	if !strings.Contains(e.NormalizedText, "Any update on this?") {
		t.Errorf("newest message was lost:\n%s", e.NormalizedText)
	}
}

// An absent idempotency key must still be idempotent, and a supplied one must win.
func TestIdempotencyKey(t *testing.T) {
	a := mustNormalize(t, `{"channel":"email","from":"j@a.example","body":"hello there friend"}`)
	b := mustNormalize(t, `{"channel":"email","from":"j@a.example","body":"hello there friend"}`)
	if a.IdempotencyKey != b.IdempotencyKey {
		t.Error("identical payloads produced different auto idempotency keys")
	}
	c := mustNormalize(t, `{"channel":"email","from":"j@a.example","body":"hello there friend","idempotency_key":"webhook-42"}`)
	if c.IdempotencyKey != "webhook-42" {
		t.Errorf("supplied idempotency key ignored: %q", c.IdempotencyKey)
	}
}

func TestSuspicious(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"marketing blast", `{"channel":"email","from":"x@y.example","body":"We offer premium SEO services and backlink packages, guest post opportunities available now"}`, true},
		{"disposable sender", `{"channel":"email","from":"a@mailinator.com","body":"Please send me your full product catalogue and pricing"}`, true},
		{"contentless", `{"channel":"messaging","sender_handle":"@x","text":"hi"}`, true},
		{"real sales enquiry", `{"channel":"email","from":"jane@acme.example","subject":"Pricing","body":"We are looking at 500 units for a March rollout, could you send pricing and lead times?"}`, false},
		{"short urgent support is not spam", `{"channel":"email","from":"ops@acme.example","subject":"URGENT: SITE DOWN","body":"OUR CHECKOUT IS DOWN"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Suspicious(mustNormalize(t, tc.raw))
			if got.Suspicious != tc.want {
				t.Fatalf("Suspicious = %v (%v), want %v", got.Suspicious, got.Reasons, tc.want)
			}
			if got.Suspicious && len(got.Reasons) == 0 {
				t.Error("suspicious verdict with no reason recorded for the audit log")
			}
		})
	}
}
