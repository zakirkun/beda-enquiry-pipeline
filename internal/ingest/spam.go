package ingest

import (
	"regexp"
	"strings"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// Tier 0 spam heuristics: free and instant, so they run before any LLM call
// (docs/04-ARCHITECTURE.md §6). These only decide whether the enquiry is
// *suspicious*, never whether it is junk — the LLM confirms, because the brief's
// expensive failure is a real sales enquiry dropped as spam (docs/01-PRD.md §7).

var (
	spamPhrases = []string{
		"viagra", "casino", "crypto giveaway", "forex signals", "seo services",
		"guest post", "backlink", "increase your ranking", "work from home",
		"you have won", "inheritance fund", "bitcoin investment", "click here to claim",
		"unsubscribe from this list", "this is not spam",
	}
	urlRE      = regexp.MustCompile(`https?://`)
	disposable = []string{"mailinator.com", "guerrillamail.com", "10minutemail.com", "yopmail.com", "tempmail."}
)

// SpamSignals is the heuristic verdict, kept as data so the reason lands in the
// audit log rather than being reduced to a bare boolean.
type SpamSignals struct {
	Suspicious bool     `json:"suspicious"`
	Reasons    []string `json:"reasons"`
}

// Suspicious scores an enquiry on cheap signals. A hit routes to the LLM for
// confirmation; it does not archive anything on its own.
func Suspicious(e *model.Enquiry) SpamSignals {
	var s SpamSignals
	text := strings.ToLower(e.NormalizedText)
	words := strings.Fields(text)

	for _, p := range spamPhrases {
		if strings.Contains(text, p) {
			s.Reasons = append(s.Reasons, "phrase:"+p)
			break
		}
	}
	for _, d := range disposable {
		if strings.Contains(strings.ToLower(e.SenderIdentifier), d) {
			s.Reasons = append(s.Reasons, "disposable_sender_domain")
			break
		}
	}
	// Link-heavy and near-contentless: a marketing blast, not a question.
	if links := len(urlRE.FindAllString(text, -1)); links >= 3 && len(words) < 120 {
		s.Reasons = append(s.Reasons, "link_dense")
	}
	if len(words) < 4 {
		s.Reasons = append(s.Reasons, "no_content_signal")
	}
	// Shouting in caps, but only once there is enough text for the ratio to mean
	// something — a short "URGENT: SITE DOWN" is a real support enquiry.
	if letters, upper := caseCounts(e.NormalizedText); letters > 60 && float64(upper)/float64(letters) > 0.7 {
		s.Reasons = append(s.Reasons, "all_caps")
	}
	s.Suspicious = len(s.Reasons) > 0
	return s
}

func caseCounts(s string) (letters, upper int) {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
		case r >= 'A' && r <= 'Z':
			letters++
			upper++
		}
	}
	return letters, upper
}
