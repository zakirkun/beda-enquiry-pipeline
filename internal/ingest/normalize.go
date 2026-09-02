// Package ingest normalizes inbound webhook payloads into one Enquiry shape.
// No LLM call and no CRM access happens here: the public-facing surface stays
// small so it is simple to secure (docs/04-ARCHITECTURE.md §1, §5).
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// Payload is the union of the three channel shapes. Each channel sends a subset;
// Normalize decides what the canonical text is per channel.
type Payload struct {
	// Common
	Channel        string `json:"channel"`
	IdempotencyKey string `json:"idempotency_key"`

	// email
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    string `json:"body"`

	// web_form
	Name    string `json:"name"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Company string `json:"company"`
	Message string `json:"message"`

	// messaging
	SenderHandle string `json:"sender_handle"`
	Text         string `json:"text"`
}

var (
	quotedReply  = regexp.MustCompile(`(?m)^\s*>.*$`)
	replyDivider = regexp.MustCompile(`(?mi)^\s*(-{2,}\s*original message\s*-{2,}|on .{0,80}wrote:)\s*$`)
	sigDivider   = regexp.MustCompile(`(?m)^--\s*$`)
	wsRun        = regexp.MustCompile(`[ \t]+`)
	blankLineRun = regexp.MustCompile(`\n{3,}`)
)

// Normalize maps a raw payload to an Enquiry. It returns an error rather than a
// best-effort guess when the payload has no usable content — trust-boundary
// validation, so junk never enters the pipeline as an empty enquiry.
func Normalize(raw []byte) (*model.Enquiry, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}

	e := &model.Enquiry{SourceChannel: p.Channel, RawPayload: raw, Subject: strings.TrimSpace(p.Subject)}
	var body string

	switch p.Channel {
	case model.ChannelEmail:
		e.SenderIdentifier = normalizeSender(p.From)
		body = trimQuotedReply(p.Body)
	case model.ChannelWebForm:
		e.SenderIdentifier = normalizeSender(p.Email)
		// The form's own fields become part of the enquiry text, so the
		// classifier sees the contact details the sender typed and can quote them.
		fields := joinNonEmpty("\n",
			labeled("Name", p.Name),
			labeled("Email", p.Email),
			labeled("Phone", p.Phone),
			labeled("Company", p.Company),
		)
		body = joinNonEmpty("\n\n", fields, p.Message)
	case model.ChannelMessaging:
		e.SenderIdentifier = normalizeSender(firstNonEmpty(p.SenderHandle, p.From))
		body = strings.TrimSpace(p.Text)
	default:
		return nil, fmt.Errorf("unsupported channel %q", p.Channel)
	}

	if e.SenderIdentifier == "" {
		return nil, fmt.Errorf("missing sender identifier for channel %q", p.Channel)
	}
	e.NormalizedText = clean(joinNonEmpty("\n\n", labeled("Subject", e.Subject), body))
	if e.NormalizedText == "" {
		return nil, fmt.Errorf("empty enquiry body")
	}

	e.DedupeHash = DedupeHash(e.SenderIdentifier, e.NormalizedText)
	// Absent a client-supplied key, the content hash IS the idempotency key: a
	// webhook redelivered without one still cannot create a second enquiry.
	e.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	if e.IdempotencyKey == "" {
		e.IdempotencyKey = "auto:" + e.DedupeHash
	}
	return e, nil
}

// DedupeHash keys on sender plus normalized content, so retries and
// double-submits collide while two different enquiries from one sender do not.
func DedupeHash(sender, text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(sender) + "\x00" + strings.ToLower(collapse(text))))
	return hex.EncodeToString(sum[:])
}

// normalizeSender extracts the bare address from "Name <addr>" forms and lowers
// it, so the same sender hashes identically across channels and clients.
func normalizeSender(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if a, err := mail.ParseAddress(s); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(s)
}

// trimQuotedReply drops quoted history and signatures so a long thread hashes
// and classifies on its newest message (docs/04-ARCHITECTURE.md §6).
func trimQuotedReply(body string) string {
	if loc := replyDivider.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	body = quotedReply.ReplaceAllString(body, "")
	if loc := sigDivider.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	return strings.TrimSpace(body)
}

// clean strips invisible/bidi runes and collapses runs of whitespace. The
// invisible-rune strip matters twice: it stops zero-width padding from defeating
// the dedupe hash, and it removes a channel for hiding text from a human
// reviewer that the model still reads.
func clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(dropInvisible, s)
	s = wsRun.ReplaceAllString(s, " ")
	s = blankLineRun.ReplaceAllString(s, "\n\n")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func labeled(label, v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return label + ": " + strings.TrimSpace(v)
}

func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// dropInvisible removes zero-width, bidi-override, word-joiner and BOM runes.
// Two reasons, both real: they let a sender pad text to defeat the dedupe hash,
// and they let text be hidden from a human reviewer that the model still reads.
func dropInvisible(r rune) rune {
	switch {
	case r == 0x00AD, // soft hyphen
		r >= 0x200B && r <= 0x200F, // zero-width + LTR/RTL marks
		r >= 0x202A && r <= 0x202E, // bidi embedding/override
		r >= 0x2060 && r <= 0x2064, // word joiner + invisible operators
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r == 0xFEFF:                // BOM / zero-width no-break space
		return -1
	}
	return r
}
