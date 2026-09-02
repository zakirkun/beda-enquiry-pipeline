package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/google/uuid"

	"github.com/beda/enquiry-pipeline/internal/llm"
	"github.com/beda/enquiry-pipeline/internal/model"
)

// handleScenarios lists what the simulator can produce, so the dashboard does not
// hardcode a copy of the scenario table.
func (s *Server) handleScenarios(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, llm.Scenarios)
}

type simulateRequest struct {
	Scenario string `json:"scenario"`
}

// handleSimulate generates one realistic enquiry with the LLM and feeds it in
// through the real signed webhook — same auth, same normalization, same queue.
// The simulator gets no shortcut: if a generated payload would be rejected from
// the public internet, it is rejected here too.
//
// Ops-only, because it spends provider tokens and writes enquiry rows. It is a
// demo affordance, not a product feature.
// ponytail: role check is the control. Add a SIMULATOR_ENABLED flag if this ever
// runs somewhere that is not a demo.
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	if u.Role != model.RoleOpsAdmin {
		writeErr(w, http.StatusForbidden, "only ops_admin may generate simulated enquiries")
		return
	}

	var req simulateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	sc, ok := llm.ScenarioByKey(strings.TrimSpace(req.Scenario))
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown scenario")
		return
	}

	payload, modelUsed, err := s.llm.GenerateEnquiry(r.Context(), sc)
	if err != nil {
		s.log.Error("simulate generation failed", "err", err, "scenario", sc.Key)
		writeErr(w, http.StatusBadGateway, "could not generate an enquiry: "+err.Error())
		return
	}

	// Replay it against our own webhook handler, signed, in process. Nothing
	// leaves the machine and no code path is bypassed.
	rec := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/"+sc.Channel, strings.NewReader(string(payload)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Beda-Signature", "sha256="+s.signBody(payload))
	req2.SetPathValue("channel", sc.Channel)
	s.requireWebhookAuth(http.HandlerFunc(s.handleWebhook)).ServeHTTP(rec, req2)

	var ingested struct {
		EnquiryID uuid.UUID `json:"enquiry_id"`
		Status    string    `json:"status"`
		Error     string    `json:"error,omitempty"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ingested)
	if rec.Code >= 300 {
		s.log.Warn("simulated enquiry was rejected by the gateway", "code", rec.Code, "body", rec.Body.String())
		writeErr(w, http.StatusBadGateway, "the generated enquiry was rejected by the webhook: "+ingested.Error)
		return
	}

	// Audited against the enquiry itself, so the trail on the review screen says
	// out loud that this one was synthetic and who asked for it.
	_ = s.st.Audit(r.Context(), &ingested.EnquiryID, "enquiry", &ingested.EnquiryID,
		"simulated_enquiry_generated", "user:"+u.ID.String(),
		map[string]any{"scenario": sc.Key, "channel": sc.Channel, "expect": sc.Expect, "model": modelUsed})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"scenario":   sc.Key,
		"expect":     sc.Expect,
		"channel":    sc.Channel,
		"model_used": modelUsed,
		"payload":    json.RawMessage(payload),
		"gateway":    map[string]any{"enquiry_id": ingested.EnquiryID, "status": ingested.Status},
	})
}
