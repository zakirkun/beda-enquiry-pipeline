# BEDA Enquiry Intelligence Pipeline

Inbound customer enquiries (email / web form / messaging) arrive on a webhook, are
classified and extracted by an LLM, routed by deterministic rules, written to a CRM,
and answered by a **draft that a human must approve before anything is sent**.

Implements `docs/01-PRD.md` … `docs/04-ARCHITECTURE.md`, trimmed to one Go binary +
Postgres (see [Deviations](#deviations-from-the-docs)). `docs/05-DESIGN-NOTES.md`
explains the design decisions: the LLM/deterministic split, failure handling,
permissions and secrets, cost and latency, and what is deliberately left manual.

## Run it

```bash
cp .env.example .env      # fill in WEBHOOK_SECRET and the key(s) your tiers need
docker compose up --build
```

Three containers: `postgres` (schema applied on boot), `api` (`:8080`), `dashboard`
(`:3000`). The API refuses to start without the webhook secret or a key for either
tier's provider — a pipeline that boots credential-less would silently dead-letter
every enquiry.

Then post the sample enquiries and watch them land:

```bash
WEBHOOK_SECRET=<same value> ./scripts/send-samples.sh http://localhost:8080
# Windows: pwsh scripts/send-samples.ps1 -Base http://localhost:8080
```

Open <http://localhost:3000>, pick an actor, open an enquiry: the customer's words,
what the model claimed, which of those claims are grounded in the source text, the
routing decision, the draft reply, and the audit trail.

### Simulate an enquiry from the dashboard

As `ops_admin`, the panel at the top of the queue has one button per scenario:
an Australian company hiring against a deadline, a candidate asking about roles,
an existing client with a broken integration, marketing spam, and a one-line
message with nothing to act on. Each covers a different branch of the pipeline.

The LLM writes the enquiry (BEDA's real business, invented people and companies),
then the API **posts it to its own signed webhook in process** — same HMAC, same
normalization, same queue. The simulator gets no shortcut: a generated payload
that would be rejected from the public internet is rejected here too. Only the
keys the channel declares are forwarded, so the model cannot inject a `channel`
or `status` field the gateway is supposed to decide. Every generation is audited
against the enquiry as `simulated_enquiry_generated`, so the review screen says
out loud which enquiries were synthetic and who asked for them.

Ops-only, because it spends provider tokens and writes rows.

### End-to-end assertions

```bash
BASE=http://localhost:8080 WEBHOOK_SECRET=<same value> ./scripts/verify.sh
```

38 checks: webhook auth, idempotent redelivery, classification, routing, the approval
gate (including role and team refusals), junk archiving, and the insufficient-info
path. Exits non-zero on any failure.

Unit tests: `go test ./...` (63 tests, no database needed).

## Shape

```
cmd/api            one binary: HTTP gateway + worker pool + dashboard API
internal/ingest    channel payload -> normalized enquiry, dedupe hash
internal/llm       two-tier extraction and drafting, grounding check
internal/router    the decision. no LLM.
internal/store     Postgres, incl. the queue and the approval transaction
internal/worker    claim -> spam -> extract -> match -> route -> act
dashboard/         Next.js review UI
```

**Postgres is the queue.** `enquiry` rows are claimed with
`FOR UPDATE SKIP LOCKED` under a `locked_until` lease, retried with exponential
backoff, and dead-lettered to `status='failed'` after `MAX_ATTEMPTS`. A worker that
is killed mid-enquiry loses its lease and the row is re-claimed — no work is lost and
none is done twice, because every write is idempotent by unique index
(`enquiry.idempotency_key`, `crm_record.enquiry_id`, `message(enquiry_id, kind)`).

**LLM at the edges, code in the middle.** The model classifies and extracts; it never
decides. Dedupe, spam heuristics, entity matching, routing, CRM writes, and sending
are plain Go. Tier 1 is the cheap first pass; low-confidence extractions escalate to
Tier 2, which also writes every customer-facing draft. Either tier failing falls back
to the other.

**Neither tier is pinned to a vendor.** A tier is a `(provider, model)` pair from the
environment — `TIER1_PROVIDER` / `TIER1_MODEL`, `TIER2_PROVIDER` / `TIER2_MODEL`. Both
may name the same provider, or different ones; only the providers a tier actually names
need a key, so an all-OpenAI or all-Anthropic setup carries one credential. Every
provider satisfies langchaingo's `llms.Model`, so escalation and fallback stay "call a
different value" rather than a provider branch. Defaults: `openai/gpt-4o-mini` and
`anthropic/claude-sonnet-4-5`.

**Grounding check.** Every extracted field must be quotable verbatim from the source
text (whitespace- and case-normalised). Fields that are not get flagged `unverified`
and are stripped before any CRM write, so a hallucinated email address cannot become
a contact row. Confidence below `CONFIDENCE_THRESHOLD`, or an unverified
`enquiry_type`, sends the enquiry to human review instead.

**Nothing sends itself.** Drafts are stored `pending_approval`. Approval is one
transaction: row lock, status flip, `approval_action` insert, enquiry status, audit
row. A second approval gets `409`. Managers may read the trail but not approve
(`403`); a rep cannot approve another team's message (`403`).

## Configuration

`.env.example` documents the required secrets and the tier/provider pairing. Optional
knobs (all with defaults in `internal/config/config.go`): `CONFIDENCE_THRESHOLD`,
`AUTO_ATTACH_MATCH_THRESHOLD`, `TRGM_MATCH_FLOOR`, `DEDUPE_WINDOW`, `WORKER_COUNT`,
`MAX_ATTEMPTS`, `POLL_INTERVAL`, `LOCK_TTL`, `LLM_TIMEOUT`, `TIER1_PROVIDER`,
`TIER1_MODEL`, `TIER2_PROVIDER`, `TIER2_MODEL`, `DASHBOARD_ORIGIN`, `RULES_SEED_FILE`.
`OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` repoint either provider at a gateway, an Azure
deployment, or a local mock.

Routing rules are data, not prompt: `config/routing_rules.json`, seeded into
`routing_rule`. Ops edits rows.

## API

| | |
|---|---|
| `POST /webhook/{email\|web_form\|messaging}` | ingest. HMAC-SHA256 `X-Beda-Signature` over the raw body, or bearer token |
| `GET /api/users` | actor picker (names and roles only) |
| `GET /api/queue?status=&limit=` | review queue, scoped to the actor's team |
| `GET /api/enquiries/{id}` | everything about one enquiry |
| `POST /api/messages/{id}/decision` | `approved` / `approved_with_edit` / `rejected` |
| `POST /api/duplicates/{id}/resolve` | `attached` / `separate` |
| `GET /api/stats` | dashboard metrics |
| `GET /api/simulate/scenarios` | what the demo simulator can generate |
| `POST /api/simulate` | `{"scenario":"<key>"}` — generate one enquiry and feed it through the signed webhook. `ops_admin` only |
| `GET /healthz` | liveness |

Dashboard endpoints need `X-Actor-Id`. **`ponytail:` that header stands in for SSO** —
it is the one place this prototype trusts the caller, and the seam where real session
auth goes.

## Deviations from the docs

`docs/04-ARCHITECTURE.md` specifies five services plus NATS, Redis, and MinIO. This
runs the same semantics on Postgres alone: NATS becomes the `enquiry` queue table,
Redis dedupe becomes a unique index, MinIO becomes a `jsonb` raw-payload column. Each
is marked with a `ponytail:` comment naming its ceiling and upgrade path. Split the
binary when one stage needs to scale independently of the others; pull the queue out
when polling throughput actually hurts.

Not built: real SMTP/messaging egress (approval flips status and audits; the send
client wires into that same transaction boundary), contact merging (a human declares
"same person" or "different person" — no records are combined), and SSO.
