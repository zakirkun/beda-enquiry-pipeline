// Package api is the HTTP surface: inbound webhooks and the dashboard's REST API.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/beda/enquiry-pipeline/internal/config"
	"github.com/beda/enquiry-pipeline/internal/ingest"
	"github.com/beda/enquiry-pipeline/internal/llm"
	"github.com/beda/enquiry-pipeline/internal/model"
	"github.com/beda/enquiry-pipeline/internal/store"
)

const maxBodyBytes = 1 << 20 // 1 MiB: webhook payloads, not file uploads

type Server struct {
	st   *store.Store
	cfg  config.Config
	llm  *llm.Client
	log  *slog.Logger
	wake func() // nudge the worker pool so a new enquiry is picked up immediately
}

func NewServer(st *store.Store, cfg config.Config, llmClient *llm.Client, log *slog.Logger, wake func()) *Server {
	return &Server{st: st, cfg: cfg, llm: llmClient, log: log, wake: wake}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Inbound webhooks. Authenticated with a shared secret; they only normalize
	// and enqueue — no LLM call, no CRM access (docs/04-ARCHITECTURE.md §5).
	mux.Handle("POST /webhook/{channel}", s.requireWebhookAuth(http.HandlerFunc(s.handleWebhook)))

	// Dashboard API. Every handler needs an actor, because the audit log records
	// who acted, not that "the system" did (docs/04-ARCHITECTURE.md §5).
	mux.HandleFunc("GET /api/users", s.handleUsers)
	mux.Handle("GET /api/queue", s.requireActor(http.HandlerFunc(s.handleQueue)))
	mux.Handle("GET /api/enquiries/{id}", s.requireActor(http.HandlerFunc(s.handleEnquiry)))
	mux.Handle("POST /api/messages/{id}/decision", s.requireActor(http.HandlerFunc(s.handleDecision)))
	mux.Handle("POST /api/duplicates/{id}/resolve", s.requireActor(http.HandlerFunc(s.handleResolveDuplicate)))
	mux.Handle("GET /api/stats", s.requireActor(http.HandlerFunc(s.handleStats)))

	// Demo simulator: generates a realistic enquiry with the LLM and posts it
	// through the same signed webhook a real provider would use.
	mux.Handle("GET /api/simulate/scenarios", s.requireActor(http.HandlerFunc(s.handleScenarios)))
	mux.Handle("POST /api/simulate", s.requireActor(http.HandlerFunc(s.handleSimulate)))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return s.withCORS(mux)
}

// ---------- middleware ----------

// requireWebhookAuth accepts either an HMAC-SHA256 signature over the raw body
// (X-Beda-Signature, what a real provider sends) or a bearer token. Constant-time
// comparison in both cases. Public endpoint, so this is a hard requirement, not
// a nicety — config.Load refuses to boot without the secret.
func (s *Server) requireWebhookAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}

		ok := false
		if sig := r.Header.Get("X-Beda-Signature"); sig != "" {
			ok = hmac.Equal([]byte(strings.TrimPrefix(sig, "sha256=")), []byte(s.signBody(body)))
		} else if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != "" {
			ok = subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.WebhookSecret)) == 1
		}
		if !ok {
			s.log.Warn("rejected unauthenticated webhook", "remote", r.RemoteAddr, "channel", r.PathValue("channel"))
			writeErr(w, http.StatusUnauthorized, "invalid or missing webhook signature")
			return
		}

		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next.ServeHTTP(w, r)
	})
}

// signBody is the HMAC-SHA256 hex digest a caller must send in X-Beda-Signature.
// Shared with the simulator so it signs exactly the way a real provider does.
func (s *Server) signBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.WebhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// requireActor resolves the acting user from X-Actor-Id. ponytail: header-based
// identity stands in for a session/SSO login — the audit trail and role checks
// below are the parts that had to be real. Swap for real auth before any
// deployment that is not a local demo.
func (s *Server) requireActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.Header.Get("X-Actor-Id"))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "missing or invalid X-Actor-Id header")
			return
		}
		u, err := s.st.User(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown actor")
			return
		}
		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), u)))
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Single configured origin, not "*": the dashboard sends an actor header
		// and reads CRM data, so it must not be callable from any page.
		if origin := r.Header.Get("Origin"); origin != "" && origin == s.cfg.DashboardOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Actor-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- webhooks ----------

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}

	// The URL path is authoritative for the channel, so a payload cannot claim to
	// be from a channel it was not posted to.
	channel := r.PathValue("channel")
	merged, err := setChannel(body, channel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	e, err := ingest.Normalize(merged)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	switch err := s.st.InsertEnquiry(r.Context(), e); {
	case errors.Is(err, store.ErrDuplicate):
		// Redelivery. Return the original id and 200 so the sender stops retrying.
		id, lookupErr := s.st.EnquiryByIdempotencyKey(r.Context(), e.IdempotencyKey)
		if lookupErr != nil {
			writeErr(w, http.StatusInternalServerError, "duplicate lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enquiry_id": id, "status": "already_received"})
		return
	case err != nil:
		s.log.Error("insert enquiry failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not accept enquiry")
		return
	}

	_ = s.st.Audit(r.Context(), &e.ID, "enquiry", &e.ID, "received", "system:gateway",
		map[string]any{"channel": e.SourceChannel, "sender": e.SenderIdentifier, "dedupe_hash": e.DedupeHash})

	// Acknowledge now, process off the queue: ingestion latency never depends on
	// LLM latency (docs/01-PRD.md §6).
	s.wake()
	writeJSON(w, http.StatusAccepted, map[string]any{"enquiry_id": e.ID, "status": model.StatusReceived})
}

// setChannel forces the channel field to the URL segment.
func setChannel(body []byte, channel string) ([]byte, error) {
	switch channel {
	case model.ChannelEmail, model.ChannelWebForm, model.ChannelMessaging:
	default:
		return nil, errors.New("unsupported channel " + strconv.Quote(channel))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, errors.New("invalid json")
	}
	m["channel"], _ = json.Marshal(channel)
	return json.Marshal(m)
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already out; nothing to do but note it.
		slog.Default().Error("write response failed", "err", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func notFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
