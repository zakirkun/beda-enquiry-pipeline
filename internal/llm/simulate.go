package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// Scenario is one simulated inbound enquiry: which pipeline path it should
// exercise, and which channel it arrives on. Kept as data so the dashboard can
// list them and the API can validate against them.
type Scenario struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Channel string `json:"channel"`
	Expect  string `json:"expect"` // the path this should take, for the operator's benefit
	brief   string // what to tell the model to write
}

// Scenarios covers every branch the pipeline can take. Company context is real:
// BEDA places remote sales and marketing people in Bali with Australian
// companies (wearebeda.com), so an enquiry is either a company wanting to hire,
// a candidate wanting a role, an existing client with a problem, or noise.
var Scenarios = []Scenario{
	{
		Key: "hiring_urgent", Label: "Australian company hiring, deadline", Channel: model.ChannelEmail,
		Expect: "sales / high urgency / drafted reply",
		brief:  `An operations or revenue lead at an Australian company (pick a plausible industry: SaaS, solar, trades, insurance, med-tech) emailing to ask about building a remote appointment-setting or closing team in Bali. They state a real deadline or a blocked situation — a campaign launching, a quarter ending, a team member leaving. Include how many seats they are thinking about, their full name, company name, work email and a direct phone number, all written naturally inside the email body.`,
	},
	{
		Key: "candidate", Label: "Candidate asking about roles", Channel: model.ChannelWebForm,
		Expect: "sales / drafted reply",
		brief:  `A sales or marketing professional, currently somewhere other than Bali, filling in the "Ready to build something bigger" form on wearebeda.com. They want to know what roles are open, what the on-target earnings look like, and what relocation involves. Give the form fields (name, email, phone, current employer as company) and a message of three or four sentences that mentions their current role and how much closing or campaign experience they have.`,
	},
	{
		Key: "client_support", Label: "Existing client with a problem", Channel: model.ChannelEmail,
		Expect: "support / drafted reply",
		brief:  `A contact at a company already working with BEDA, emailing about something broken rather than something to buy: dashboard or reporting access not working, an invoice that does not match the agreed rate, a setter whose call recordings are not syncing to their CRM. They are a known customer, mildly frustrated, and they mention roughly when the problem started. Include their name, company and work email in the body.`,
	},
	{
		Key: "junk", Label: "Marketing spam", Channel: model.ChannelEmail,
		Expect: "junk / archived, no CRM record",
		brief:  `A cold outreach blast from an SEO or lead-generation agency offering backlinks, guest posts or a "guaranteed first page" package. Generic, addressed to no one in particular, two or three URLs, a discount that expires. It must not contain any real enquiry about BEDA's services.`,
	},
	{
		Key: "vague", Label: "One-line message, nothing to act on", Channel: model.ChannelMessaging,
		Expect: "insufficient_info / clarifying question drafted",
		brief:  `A single short direct message on Instagram or WhatsApp — under fifteen words, no name, no contact details, no company. Something like asking whether there is anything going in Bali right now, or whether they still need people. It must be too thin to act on.`,
	},
}

// ScenarioByKey looks up a scenario. Unknown keys are rejected rather than
// defaulted: this endpoint spends money and writes rows.
func ScenarioByKey(key string) (Scenario, bool) {
	for _, s := range Scenarios {
		if s.Key == key {
			return s, true
		}
	}
	return Scenario{}, false
}

const simulatePrompt = `You write realistic inbound enquiries for a test harness. Output is fed to a classification pipeline as if a real person had sent it.

BEDA (wearebeda.com) places ambitious remote sales and marketing people in Bali, working for Australian companies. Its inbound enquiries come from Australian companies wanting to hire a remote team, from candidates wanting one of those roles, from existing clients with problems, and from spam.

Rules:
- Reply with one JSON object and nothing else. No prose, no markdown fence, no explanation.
- Use exactly the keys listed for the channel. No extra keys.
- Write like the person, not like a template: no [placeholders], no "Dear Sir/Madam", no lorem ipsum. Australian spelling and phrasing where the sender is Australian.
- Invent a specific person, company and contact details. Fictional but plausible, and consistent between the fields and the body. Use .example, .com.au or .co domains.
- Every fact a reader could act on must appear in the text itself. Never write details into a field that the body contradicts.
- Vary the details on every request: different name, company, city, industry, numbers and phrasing than an obvious first choice.`

var channelKeys = map[string]string{
	model.ChannelEmail:     `{"from": "Full Name <address>", "subject": "...", "body": "the full email, 60-150 words, with line breaks as \n"}`,
	model.ChannelWebForm:   `{"name": "...", "email": "...", "phone": "...", "company": "...", "message": "3-4 sentences"}`,
	model.ChannelMessaging: `{"sender_handle": "@handle", "text": "the message"}`,
}

// GenerateEnquiry writes one synthetic enquiry payload for a scenario, shaped for
// that scenario's channel. It returns the payload as raw JSON so the caller can
// hand it to the same ingest path a real webhook uses — the simulator gets no
// shortcut through validation.
func (c *Client) GenerateEnquiry(ctx context.Context, s Scenario) ([]byte, string, error) {
	keys, ok := channelKeys[s.Channel]
	if !ok {
		return nil, "", fmt.Errorf("scenario %q has unsupported channel %q", s.Key, s.Channel)
	}
	// The pipeline deduplicates on sender plus content, so two identical
	// generations would collapse into one enquiry. High temperature plus a
	// throwaway seed is what keeps repeated clicks producing distinct enquiries.
	user := fmt.Sprintf("Channel: %s\nKeys: %s\n\nWrite this enquiry:\n%s\n\nVariation seed %d — use it to pick a different name, company, city and set of numbers than you would otherwise.",
		s.Channel, keys, s.brief, rand.IntN(1_000_000))

	out, modelName, err := c.draft(ctx, simulatePrompt, user, 1.0)
	if err != nil {
		return nil, "", err
	}
	payload, err := payloadFor(s.Channel, out)
	if err != nil {
		return nil, "", err
	}
	return payload, modelName, nil
}

// payloadFor keeps only the keys the channel declares, so a stray field the model
// invented cannot ride into the payload the gateway parses. Blank strings are
// dropped rather than sent: ingest treats "" and absent the same way, and an
// empty required field should fail loudly at the webhook, not silently here.
func payloadFor(channel, modelOutput string) ([]byte, error) {
	keys, ok := channelKeys[channel]
	if !ok {
		return nil, fmt.Errorf("unsupported channel %q", channel)
	}
	raw := jsonObject(modelOutput)
	if raw == "" {
		return nil, errors.New("model did not return a JSON object")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return nil, fmt.Errorf("decode generated payload: %w", err)
	}

	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(keys), &want); err != nil {
		return nil, fmt.Errorf("channel key template for %q is not valid json: %w", channel, err)
	}
	clean := map[string]any{}
	for k := range want {
		v, present := got[k]
		if !present {
			continue
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("generated payload had none of the %s keys", channel)
	}
	return json.Marshal(clean)
}
