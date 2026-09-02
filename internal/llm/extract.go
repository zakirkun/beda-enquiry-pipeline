// Package llm wraps the two model tiers behind one interface. Every provider
// satisfies langchaingo's llms.Model, so escalation and provider fallback are
// "call a different value", not provider-specific branches
// (docs/04-ARCHITECTURE.md §2, §8).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/openai"

	"github.com/beda/enquiry-pipeline/internal/config"
	"github.com/beda/enquiry-pipeline/internal/model"
)

type Client struct {
	tier1     llms.Model // cheap, fast, first pass
	tier2     llms.Model // escalation + all drafting
	tier1Name string
	tier2Name string
	timeout   time.Duration
	threshold float64
	log       *slog.Logger
}

// newModel builds a langchaingo client for whichever provider a tier names. The
// tiers are configuration, not code: either can be either provider, and both can
// be the same one.
func newModel(t config.Tier, p config.Provider) (llms.Model, error) {
	switch t.Provider {
	case config.ProviderOpenAI:
		opts := []openai.Option{openai.WithToken(p.Key), openai.WithModel(t.Model)}
		if p.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(p.BaseURL))
		}
		return openai.New(opts...)
	case config.ProviderAnthropic:
		opts := []anthropic.Option{anthropic.WithToken(p.Key), anthropic.WithModel(t.Model)}
		if p.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(p.BaseURL))
		}
		return anthropic.New(opts...)
	default:
		return nil, fmt.Errorf("unsupported provider %q", t.Provider)
	}
}

func New(cfg config.Config, log *slog.Logger) (*Client, error) {
	t1, err := newModel(cfg.Tier1, cfg.Providers[cfg.Tier1.Provider])
	if err != nil {
		return nil, fmt.Errorf("tier1 (%s): %w", cfg.Tier1, err)
	}
	t2, err := newModel(cfg.Tier2, cfg.Providers[cfg.Tier2.Provider])
	if err != nil {
		return nil, fmt.Errorf("tier2 (%s): %w", cfg.Tier2, err)
	}
	return &Client{
		tier1: t1, tier2: t2,
		tier1Name: cfg.Tier1.Model, tier2Name: cfg.Tier2.Model,
		timeout: cfg.LLMTimeout, threshold: cfg.ConfidenceThreshold, log: log,
	}, nil
}

// extractionSchema is the JSON schema from docs/04-ARCHITECTURE.md §8, enforced
// as a tool definition. The model fills a schema; it does not emit prose we parse.
var extractionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"enquiry_type": map[string]any{
			"type": "string",
			"enum": []string{model.TypeSales, model.TypeSupport, model.TypeJunk, model.TypeInsufficientInfo},
		},
		"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		"urgency":    map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
		"intent_summary": map[string]any{
			"type":        "string",
			"description": "One or two sentences on what the sender wants.",
		},
		"contact": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":         map[string]any{"type": []string{"string", "null"}},
				"email":        map[string]any{"type": []string{"string", "null"}},
				"phone":        map[string]any{"type": []string{"string", "null"}},
				"company_name": map[string]any{"type": []string{"string", "null"}},
			},
		},
		"missing_fields": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Fields required to act on this enquiry that are absent from the text. Never invent a value for these.",
		},
		"grounding_quotes": map[string]any{
			"type":                 "object",
			"description":          "For each extracted field, the verbatim span of the source text it came from. Keys: enquiry_type, urgency, name, email, phone, company_name.",
			"additionalProperties": map[string]any{"type": "string"},
		},
	},
	"required": []string{"enquiry_type", "confidence", "urgency", "intent_summary", "missing_fields", "grounding_quotes"},
}

var extractionTool = llms.Tool{
	Type: "function",
	Function: &llms.FunctionDefinition{
		Name:        "record_enquiry_extraction",
		Description: "Record the classification and extracted fields for one business enquiry.",
		Parameters:  extractionSchema,
	},
}

const extractionPrompt = `You classify inbound business enquiries for BEDA and extract structured data from them.

Rules:
- Call record_enquiry_extraction exactly once. Do not reply in prose.
- Extract only what the text states. If a field is absent, set it to null and list it in missing_fields. Never guess, infer, or complete a value.
- For every field you do extract, put the exact verbatim substring of the source text you took it from in grounding_quotes. Copy it character for character; do not paraphrase, reformat, or fix typos. A quote that is not literally present in the text will be rejected.
- confidence is your own calibrated probability that enquiry_type is correct.
- enquiry_type: sales for buying intent or pricing, support for an existing-customer problem, junk for spam or marketing blasts, insufficient_info when there is genuinely not enough content to tell.
- urgency: high only when the sender states a deadline, outage, or blocked work.`

// Extract runs Tier 1, escalates to Tier 2 on low confidence, and falls back to
// Tier 2 when Tier 1 errors — a vendor outage stalls nothing.
func (c *Client) Extract(ctx context.Context, text string) (model.Extraction, string, error) {
	ex, raw, err := c.callExtract(ctx, c.tier1, c.tier1Name, text)
	if err != nil {
		c.log.Warn("tier1 extract failed, falling back to tier2", "err", err)
		ex, raw, err2 := c.callExtract(ctx, c.tier2, c.tier2Name, text)
		if err2 != nil {
			return model.Extraction{}, "", fmt.Errorf("both providers failed: tier1=%v tier2=%w", err, err2)
		}
		ex.Escalated = true
		return ex, raw, nil
	}

	if ex.Confidence < c.threshold {
		esc, escRaw, err2 := c.callExtract(ctx, c.tier2, c.tier2Name, text)
		if err2 == nil {
			esc.Escalated = true
			return esc, escRaw, nil
		}
		// Escalation failed: keep the tier-1 result. The router's confidence
		// gate still sends it to a human, so this degrades safely.
		c.log.Warn("escalation to tier2 failed, keeping tier1 result", "err", err2)
	}
	return ex, raw, nil
}

func (c *Client) callExtract(ctx context.Context, m llms.Model, name, text string) (model.Extraction, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := m.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, extractionPrompt),
			llms.TextParts(llms.ChatMessageTypeHuman, text),
		},
		llms.WithTools([]llms.Tool{extractionTool}),
		llms.WithTemperature(0),
		// Required by the Anthropic Messages API, and harmless on OpenAI. The
		// extraction schema is small; this is a ceiling, not a target.
		llms.WithMaxTokens(2000),
	)
	if err != nil {
		return model.Extraction{}, "", err
	}
	if len(resp.Choices) == 0 {
		return model.Extraction{}, "", errors.New("empty response")
	}

	raw := firstToolArgs(resp.Choices[0])
	if raw == "" {
		// Neither provider is forced to call the tool, so accept a bare JSON
		// object in the content as a fallback. Better than dead-lettering an
		// enquiry over a formatting choice — validate() still gates the values.
		raw = jsonObject(resp.Choices[0].Content)
	}
	if raw == "" {
		return model.Extraction{}, resp.Choices[0].Content, errors.New("model returned no tool call")
	}
	var ex model.Extraction
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		return model.Extraction{}, raw, fmt.Errorf("decode tool args: %w", err)
	}
	ex.ModelUsed = name
	if err := validate(&ex); err != nil {
		return model.Extraction{}, raw, err
	}
	normalizeContact(&ex.Contact)
	Ground(&ex, text)
	return ex, raw, nil
}

func firstToolArgs(ch *llms.ContentChoice) string {
	for _, tc := range ch.ToolCalls {
		if tc.FunctionCall != nil && tc.FunctionCall.Arguments != "" {
			return tc.FunctionCall.Arguments
		}
	}
	return ""
}

// jsonObject pulls the outermost {...} out of a response, tolerating a fenced
// code block or a sentence around it. Returns "" when there is no object.
func jsonObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	candidate := s[start : end+1]
	if !json.Valid([]byte(candidate)) {
		return ""
	}
	return candidate
}

// validate rejects out-of-schema values the provider let through. Enum
// violations are a hard error, not something to coerce to a default.
func validate(ex *model.Extraction) error {
	switch ex.EnquiryType {
	case model.TypeSales, model.TypeSupport, model.TypeJunk, model.TypeInsufficientInfo:
	default:
		return fmt.Errorf("invalid enquiry_type %q", ex.EnquiryType)
	}
	switch ex.Urgency {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("invalid urgency %q", ex.Urgency)
	}
	if ex.Confidence < 0 || ex.Confidence > 1 {
		return fmt.Errorf("confidence %v out of range", ex.Confidence)
	}
	if strings.TrimSpace(ex.IntentSummary) == "" {
		return errors.New("empty intent_summary")
	}
	return nil
}

var wsRun = regexp.MustCompile(`\s+`)

func norm(s string) string {
	return wsRun.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), " ")
}

// normalizeContact cleans up field values before they are matched or written.
// Models routinely hand back a value with the sentence punctuation still
// attached ("acme@x.example." or "Acme Ltd again"), and an email with a trailing
// period will not match an existing contact — so this runs before both the
// grounding check and the CRM write. Blanks become nil so "not found" and
// "empty" stay distinguishable.
func normalizeContact(c *model.ExtractedContact) {
	trimEnds := func(s string) string {
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), ".,;:!?\"'()<>"))
	}
	clean := func(p **string, f func(string) string) {
		if *p == nil {
			return
		}
		v := f(**p)
		if v == "" {
			*p = nil
			return
		}
		*p = &v
	}
	clean(&c.Name, trimEnds)
	clean(&c.CompanyName, trimEnds)
	clean(&c.Phone, trimEnds)
	clean(&c.Email, func(s string) string {
		s = strings.ToLower(trimEnds(s))
		// Reject anything that is not shaped like an address rather than writing
		// a broken one into the CRM.
		at := strings.Index(s, "@")
		if at <= 0 || !strings.Contains(s[at+1:], ".") || strings.ContainsAny(s, " \t") {
			return ""
		}
		return s
	})
}

// Ground is the hallucination check from docs/04-ARCHITECTURE.md §4. Every
// extracted field must be backed by a quote that actually appears in the source
// text; anything else is flagged unverified and excluded from CRM writes.
// Whitespace and case are normalized before comparison — a model reflowing a
// line break is a formatting difference, not a fabrication.
func Ground(ex *model.Extraction, sourceText string) {
	haystack := norm(sourceText)
	ex.UnverifiedFields = nil

	check := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		// A value quoted verbatim from the text is grounded by definition,
		// whether or not the model also supplied a quote for it.
		if strings.Contains(haystack, norm(value)) {
			return
		}
		q, ok := ex.GroundingQuotes[field]
		if !ok || strings.TrimSpace(q) == "" || !strings.Contains(haystack, norm(q)) {
			ex.UnverifiedFields = append(ex.UnverifiedFields, field)
		}
	}

	check("name", deref(ex.Contact.Name))
	check("email", deref(ex.Contact.Email))
	check("phone", deref(ex.Contact.Phone))
	check("company_name", deref(ex.Contact.CompanyName))

	// Classification is a judgment, not a copied span, so it is only checked for
	// a quote that exists in the text — not for the label appearing literally.
	for _, f := range []string{"enquiry_type", "urgency"} {
		if q, ok := ex.GroundingQuotes[f]; ok && strings.TrimSpace(q) != "" && !strings.Contains(haystack, norm(q)) {
			ex.UnverifiedFields = append(ex.UnverifiedFields, f)
		}
	}
}

// Trusted returns the contact block with every ungrounded field dropped. This is
// what the CRM sync worker writes: the model proposes, code decides.
func Trusted(ex model.Extraction) model.ExtractedContact {
	c := ex.Contact
	if ex.Unverified("name") {
		c.Name = nil
	}
	if ex.Unverified("email") {
		c.Email = nil
	}
	if ex.Unverified("phone") {
		c.Phone = nil
	}
	if ex.Unverified("company_name") {
		c.CompanyName = nil
	}
	return c
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
