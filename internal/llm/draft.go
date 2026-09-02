package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/llms"

	"github.com/beda/enquiry-pipeline/internal/model"
)

const draftReplyPrompt = `You draft replies to inbound business enquiries for BEDA. A human reviews and approves every draft before it is sent, so write it ready to send but never assume it will go out unedited.

Rules:
- Plain email body only. No subject line, no markdown, no placeholder brackets like [Name] — if you do not know a name, open without one.
- Ground everything in the enquiry. Do not invent prices, discounts, delivery dates, availability, staff names, or commitments of any kind. If the sender asked for a number you were not given, say a colleague will follow up with it.
- 120 words or fewer. Acknowledge what they asked, state the next step, sign off as "The BEDA Team".`

const draftQuestionPrompt = `You draft short clarifying questions for inbound business enquiries at BEDA. A human reviews and approves every draft before it is sent.

Rules:
- Plain email body only. No subject line, no markdown, no placeholder brackets.
- Ask only for the listed missing fields, in one short paragraph or a brief list. Do not ask for anything else and do not restate their whole enquiry back to them.
- Do not invent prices, dates, or commitments. 100 words or fewer, sign off as "The BEDA Team".`

// DraftReply writes the customer-facing reply. Always Tier 2: this is the text a
// human will send under BEDA's name, so writing quality matters more than the
// per-call cost (docs/04-ARCHITECTURE.md §6).
func (c *Client) DraftReply(ctx context.Context, ex model.Extraction, enquiryText string) (string, string, error) {
	user := fmt.Sprintf("Enquiry type: %s\nUrgency: %s\nWhat they want: %s\n\n--- Original enquiry ---\n%s",
		ex.EnquiryType, ex.Urgency, ex.IntentSummary, enquiryText)
	return c.draft(ctx, draftReplyPrompt, user, 0.3)
}

// DraftClarifyingQuestion asks for what is missing instead of creating a
// half-populated CRM record from guesses (docs/04-ARCHITECTURE.md §4).
func (c *Client) DraftClarifyingQuestion(ctx context.Context, ex model.Extraction, enquiryText string) (string, string, error) {
	missing := strings.Join(ex.MissingFields, ", ")
	if missing == "" {
		missing = "the details needed to help them"
	}
	user := fmt.Sprintf("Missing fields to ask about: %s\nWhat they seem to want: %s\n\n--- Original enquiry ---\n%s",
		missing, ex.IntentSummary, enquiryText)
	return c.draft(ctx, draftQuestionPrompt, user, 0.3)
}

// draft calls Tier 2 and falls back to Tier 1 on failure, so a single vendor
// outage degrades draft quality rather than stalling the enquiry.
func (c *Client) draft(ctx context.Context, system, user string, temperature float64) (string, string, error) {
	body, err := c.generate(ctx, c.tier2, system, user, temperature)
	if err == nil {
		return body, c.tier2Name, nil
	}
	c.log.Warn("tier2 draft failed, falling back to tier1", "err", err)
	body, err2 := c.generate(ctx, c.tier1, system, user, temperature)
	if err2 != nil {
		return "", "", fmt.Errorf("both providers failed: tier2=%v tier1=%w", err, err2)
	}
	return body, c.tier1Name, nil
}

func (c *Client) generate(ctx context.Context, m llms.Model, system, user string, temperature float64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := m.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, system),
			llms.TextParts(llms.ChatMessageTypeHuman, user),
		},
		llms.WithTemperature(temperature),
		llms.WithMaxTokens(600),
	)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("empty response")
	}
	body := strings.TrimSpace(resp.Choices[0].Content)
	if body == "" {
		return "", errors.New("empty draft")
	}
	return body, nil
}
