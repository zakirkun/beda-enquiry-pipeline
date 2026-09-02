package llm

import (
	"encoding/json"
	"testing"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// The simulator's output goes straight into the webhook, so key filtering is a
// trust boundary of its own: a model that invents an extra field must not have
// it forwarded, and a channel's real keys must survive intact.
func TestPayloadForKeepsOnlyChannelKeys(t *testing.T) {
	out := "Here you go:\n{\"from\":\"Sam Reid <sam@northwind.example>\",\"subject\":\"Setters for Q4\",\"body\":\"We need four setters.\",\"channel\":\"messaging\",\"status\":\"sent\"}"
	payload, err := payloadFor(model.ChannelEmail, out)
	if err != nil {
		t.Fatalf("payloadFor: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	if len(got) != 3 || got["from"] == nil || got["subject"] == nil || got["body"] == nil {
		t.Fatalf("got %v, want exactly from/subject/body", got)
	}
	// A payload that could claim its own channel or status would let the
	// simulator bypass what the gateway decides.
	for _, k := range []string{"channel", "status"} {
		if _, ok := got[k]; ok {
			t.Errorf("forwarded out-of-schema key %q", k)
		}
	}
}

func TestPayloadForRejectsUnusableOutput(t *testing.T) {
	for _, tc := range []struct{ name, channel, out string }{
		{"no json at all", model.ChannelEmail, "I'm afraid I can't help with that."},
		{"none of the channel's keys", model.ChannelEmail, `{"text":"wrong channel shape"}`},
		{"all values blank", model.ChannelMessaging, `{"sender_handle":"  ","text":""}`},
		{"unknown channel", "carrier_pigeon", `{"text":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := payloadFor(tc.channel, tc.out); err == nil {
				t.Fatal("accepted an unusable generation")
			}
		})
	}
}

// Every scenario must name a channel the simulator can actually shape a payload
// for, and a key the API can look up.
func TestScenariosAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Scenarios {
		if s.Key == "" || s.Label == "" || s.brief == "" || s.Expect == "" {
			t.Errorf("scenario %+v has an empty field", s)
		}
		if seen[s.Key] {
			t.Errorf("duplicate scenario key %q", s.Key)
		}
		seen[s.Key] = true
		if _, ok := channelKeys[s.Channel]; !ok {
			t.Errorf("scenario %q uses channel %q with no key template", s.Key, s.Channel)
		}
		if got, ok := ScenarioByKey(s.Key); !ok || got.Key != s.Key {
			t.Errorf("ScenarioByKey(%q) did not round-trip", s.Key)
		}
	}
	if _, ok := ScenarioByKey("nope"); ok {
		t.Error("ScenarioByKey accepted an unknown key")
	}
}
