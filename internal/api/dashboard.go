package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/beda/enquiry-pipeline/internal/model"
	"github.com/beda/enquiry-pipeline/internal/store"
)

type ctxActor struct{}

func withActor(ctx context.Context, u model.User) context.Context {
	return context.WithValue(ctx, ctxActor{}, u)
}

func actor(r *http.Request) model.User {
	u, _ := r.Context().Value(ctxActor{}).(model.User)
	return u
}

// scopeTeam returns the team a user's queue is limited to. Ops and managers see
// everything; reps and agents see their own team (docs/04-ARCHITECTURE.md §5).
func scopeTeam(u model.User) string {
	if u.Role == model.RoleOpsAdmin || u.Role == model.RoleManager {
		return ""
	}
	return u.Team
}

// handleUsers is deliberately unauthenticated: the demo dashboard needs a list of
// actors to pick from before anyone is signed in. It exposes names and roles only.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.Users(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	var statuses []string
	if v := strings.TrimSpace(r.URL.Query().Get("status")); v != "" {
		for _, st := range strings.Split(v, ",") {
			if st = strings.TrimSpace(st); st != "" {
				statuses = append(statuses, st)
			}
		}
	}
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	if statuses == nil {
		statuses = []string{}
	}

	items, err := s.st.Queue(r.Context(), statuses, scopeTeam(actor(r)), limit)
	if err != nil {
		s.log.Error("queue query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not load queue")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleEnquiry(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid enquiry id")
		return
	}
	d, err := s.st.EnquiryDetail(r.Context(), id)
	if notFound(err) {
		writeErr(w, http.StatusNotFound, "enquiry not found")
		return
	}
	if err != nil {
		s.log.Error("enquiry detail failed", "err", err, "enquiry", id)
		writeErr(w, http.StatusInternalServerError, "could not load enquiry")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type decisionRequest struct {
	Decision string `json:"decision"` // approved | approved_with_edit | rejected
	Body     string `json:"body"`     // required when approved_with_edit
	Notes    string `json:"notes"`
}

// handleDecision is the human approval gate. This is the only path by which a
// message can reach `sent` (docs/01-PRD.md F9, docs/04-ARCHITECTURE.md §7).
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	msgID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid message id")
		return
	}
	var req decisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}

	u := actor(r)
	// Managers audit; they do not send on someone else's behalf.
	if u.Role == model.RoleManager {
		writeErr(w, http.StatusForbidden, "managers may review the audit trail but not approve sends")
		return
	}
	// A rep or agent may only approve their own team's outbound message. Ops can
	// approve anything, including drafts on enquiries with no team yet.
	if scope := scopeTeam(u); scope != "" {
		team, err := s.st.MessageTeam(r.Context(), msgID)
		if notFound(err) {
			writeErr(w, http.StatusNotFound, "message not found")
			return
		}
		if err != nil {
			s.log.Error("message team lookup failed", "err", err, "message", msgID)
			writeErr(w, http.StatusInternalServerError, "could not load message")
			return
		}
		if team != scope {
			writeErr(w, http.StatusForbidden, "this message belongs to another team")
			return
		}
	}

	req.Body = strings.TrimSpace(req.Body)
	switch req.Decision {
	case "approved":
		req.Body = "" // approving as-is must not silently rewrite the draft
	case "approved_with_edit":
		if req.Body == "" {
			writeErr(w, http.StatusBadRequest, "approved_with_edit requires a non-empty body")
			return
		}
	case "rejected":
		req.Body = ""
	default:
		writeErr(w, http.StatusBadRequest, "decision must be approved, approved_with_edit, or rejected")
		return
	}

	switch err := s.st.Approve(r.Context(), msgID, u.ID, req.Decision, req.Body, req.Notes); {
	case errors.Is(err, store.ErrNotPending):
		// Double-click or two reviewers racing: report it rather than sending twice.
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.log.Error("approval failed", "err", err, "message", msgID, "actor", u.ID)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message_id": msgID, "decision": req.Decision})
}

type resolveRequest struct {
	Resolution string `json:"resolution"` // attached | separate
}

// handleResolveDuplicate records the human merge decision. No merge is performed:
// the reviewer either attaches this enquiry to the matched contact or declares it
// a different person (docs/04-ARCHITECTURE.md §7).
func (s *Server) handleResolveDuplicate(w http.ResponseWriter, r *http.Request) {
	candID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid candidate id")
		return
	}
	var req resolveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	u := actor(r)
	if u.Role == model.RoleManager {
		writeErr(w, http.StatusForbidden, "managers may review the audit trail but not resolve matches")
		return
	}

	enquiryID, contactID, err := s.st.ResolveDuplicate(r.Context(), candID, u.ID, req.Resolution)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.st.Audit(r.Context(), &enquiryID, "duplicate_match_candidate", &candID, "duplicate_resolved_"+req.Resolution,
		"user:"+u.ID.String(), map[string]any{"matched_contact_id": contactID, "resolution": req.Resolution})

	// Resolved, so the enquiry can flow again: requeue it. On `attached` the
	// candidate is now a decided match; on `separate` matching is skipped.
	if err := s.st.Requeue(r.Context(), enquiryID, 0, "duplicate_resolved_"+req.Resolution); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not requeue enquiry")
		return
	}
	s.wake()
	writeJSON(w, http.StatusOK, map[string]any{"enquiry_id": enquiryID, "resolution": req.Resolution})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats(r.Context())
	if err != nil {
		s.log.Error("stats failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not load stats")
		return
	}
	writeJSON(w, http.StatusOK, st)
}
