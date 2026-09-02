# Design Notes

Why this system is built the way it is. Answers eight review questions against the
code in this repository, not against an intention — every claim below points at a
file you can open.

## 1. Architecture and data flow

```
        email          web_form         messaging
          └────────────────┼────────────────┘
                           │  POST /webhook/{channel}
                           │  HMAC-SHA256 over the raw body, or bearer token
                           ▼
     ┌───────────────────────────────────────────────┐
     │ ingestion gateway        internal/api         │
     │ verify signature · 1 MiB cap · channel from   │
     │ the URL path, never the payload               │
     │ normalize · dedupe hash  internal/ingest      │
     └───────────────────────┬───────────────────────┘
                             │ INSERT enquiry (status=received, raw payload as jsonb)
                             ▼
              ┌────────────────────────────────┐
              │ Postgres is the queue          │
              │ FOR UPDATE SKIP LOCKED         │
              │ locked_until lease, backoff    │
              └───────────────┬────────────────┘
                              │ claim
                              ▼
     ┌────────────────────────────────────────────────────────┐
     │ worker pool              internal/worker/worker.go     │
     │  1. exact duplicate in window ............ no LLM      │
     │  2. spam heuristics (mark only) .......... no LLM      │
     │  3. classify + extract ................... tier1→tier2 │
     │  4. grounding check, then entity match ... code decides │
     │  5. route ................................ no LLM      │
     └────────┬──────────────────────────────┬────────────────┘
              │ act()                        │ draft (tier2)
              ▼                              ▼
      contact / crm_record            message (pending_approval)
              └──────────────┬───────────────┘
                             ▼
              ┌──────────────────────────────┐
              │ dashboard API + Next.js UI   │
              │ a human reads and decides    │
              └──────────────┬───────────────┘
                             ▼
        one transaction: row lock · status flip · approval_action
                       · enquiry status · audit row
                             ▼
                     send (wired to that same boundary)

  every stage above appends to audit_log_entry
```

Two properties worth naming. The model call is never on the webhook's critical
path — ingestion verifies, normalizes, inserts and returns. And a worker killed
mid-enquiry loses its lease, so the row is re-claimed: no work is lost and none is
done twice, because every write is idempotent by unique index.

## 2. Model and tool choices

| Choice | Why |
|---|---|
| **Go, one binary** | The work is IO-bound fan-out over a queue. Goroutines make the worker pool ~30 lines, and a single static binary means the reviewer runs `docker compose up` and gets the whole system. |
| **Postgres as queue, store, and dedupe index** | The docs specify NATS + Redis + MinIO. All three semantics fit in Postgres: `FOR UPDATE SKIP LOCKED` for the queue, a unique index for dedupe, a `jsonb` column for raw payloads. One dependency to run, one place to look when something is wrong, and transactional consistency between "work claimed" and "work done" that a separate broker cannot give without an outbox. |
| **langchaingo** | One `llms.Model` interface over both providers, with tool/function calling. Escalation and fallback become "call a different value", not a provider branch. |
| **Two tiers, neither vendor-pinned** | A tier is a `(provider, model)` pair from the environment. Tier 1 is the cheap first pass; tier 2 takes low-confidence escalations and every customer-facing draft. Both tiers may name the same provider. |
| **Tool calling over "reply in JSON"** | The provider enforces the schema. A bare-JSON fallback exists in `callExtract` for models that answer in prose anyway. |
| **Next.js App Router** | The review screen is the product surface for the human gate; server-side rendering is not the point, so client components against the REST API. |
| **No agent framework** | See §3. |

Default models are `openai/gpt-4o-mini` and `anthropic/claude-sonnet-4-5`, both
overridable. Nothing in the worker or router knows either name.

## 3. LLM vs deterministic code

The rule: **the model reads, code decides.**

| Uses an LLM | Stays deterministic Go |
|---|---|
| Classify enquiry type and urgency | Dedupe (SHA-256 over sender + collapsed text) |
| Extract contact fields, list missing ones, self-report confidence | Spam heuristics (`ingest.Suspicious`) |
| Produce grounding quotes | Grounding verification (`llm.Ground`) |
| Draft the reply, or the clarifying question | Entity matching and scoring |
| Generate demo enquiries (`internal/llm/simulate.go`, ops-only) | Routing (`internal/router`) — no LLM in the package at all |
| | CRM writes, approval transaction, sending |

Anything an auditor might have to explain to a customer is code. Routing is
business policy: it must be explainable ("rule *au-sales-priority* matched"),
testable without a network call, and editable by Ops — so the rules live in
`config/routing_rule` rows seeded from `config/routing_rules.json`, not in a
prompt.

**Deliberately not an agent.** There is no planner choosing its own next step. The
pipeline is a fixed five-stage sequence because the stages are known in advance,
and a fixed sequence is the one you can retry, resume mid-way, and reason about
when it dead-letters. Agent loops buy flexibility this problem does not need and
cost determinism it cannot give up. The LLM is called as a function, twice.

## 4. Failure handling

**Incomplete information — ask, never invent.** Two gates. `ingest.Normalize`
refuses a payload with no usable content instead of writing an empty enquiry
(trust-boundary validation: junk does not enter the pipeline as a row). Then the
router: any missing field, an `insufficient_info` classification, or no grounded
way to reach the sender at all sends the enquiry down
`ActionDraftClarifyingQuestion` — a drafted question, for a human to approve.
`router.identifiable` is checked independently of the model's own
`missing_fields`, so an over-confident extraction that forgot to mention it has
no email is still caught.

**Hallucination — every field must be quotable.** `llm.Ground` requires each
extracted value to appear verbatim in the source text, whitespace- and
case-normalised (a reflowed line break is formatting, not fabrication); a model
may instead supply a grounding quote, which must itself appear in the text.
Failures are recorded on the extraction as `unverified` and shown in the UI.
`llm.Trusted` then strips every ungrounded field before any CRM write, so a
hallucinated email address cannot become a contact row or match an existing one.
An unverified `enquiry_type` is treated exactly like low confidence: straight to
human review. `normalizeContact` runs first, because models return
`"acme@x.example."` with the sentence punctuation attached and a trailing period
would fail both the match and the grounding check for a reason that is not
fabrication.

**Duplicates — one index per invariant, and merging is never automatic.** An exact
duplicate inside `DEDUPE_WINDOW` is caught by hash before any LLM call is made,
so redelivery is free. Redelivery of the same webhook is caught earlier still, by
the unique index on `enquiry.idempotency_key`. Fuzzy contact matches are scored:
at or above `AUTO_ATTACH_MATCH_THRESHOLD` the enquiry is *attached* to the
existing contact; anything below becomes `ambiguous_match` and stops for a person.
Records are never combined either way. Idempotency of everything downstream is
also index-enforced — `crm_record(enquiry_id)`, `extraction_result(enquiry_id)`,
`message(enquiry_id, kind)` — which is what makes a mid-enquiry crash safe to
replay.

**Model and API failure — retry, cross-tier fallback, dead-letter.** A tier 1
error falls back to tier 2 and vice versa (`llm.Extract`, `llm.draft`), so a
single provider outage degrades cost, not availability. Every provider call is
bounded by `LLM_TIMEOUT`. A stage that fails requeues with exponential backoff
(`2^attempts × POLL_INTERVAL`) and dead-letters to `status='failed'` after
`MAX_ATTEMPTS`, with the reason in `status_reason` and an audit row. Shutdown is
distinguished from failure: `context.Canceled` during drain requeues the enquiry
*without* burning a retry, so a deploy does not eat the retry budget. If
escalation to tier 2 fails, the tier 1 result is kept rather than discarded —
its low confidence routes it to a human anyway, which is the right destination.

Nothing in this list ends in a guess. Every failure path terminates at a person or
at a queue, never at an automatic decision made on bad data.

## 5. Permissions, secrets, sensitive data

**Permissions.** Four roles. `ops_admin` and `manager` see everything; `sales_rep`
and `support_agent` are scoped to their team, enforced in the query, not in the UI.
Two refusals are load-bearing: a manager may read the entire audit trail but may
not approve (`403`) — oversight and authority are separate — and a rep cannot
approve another team's message (`403`). Approving "as-is" forcibly clears any body
in the request, so an approval cannot silently rewrite the draft it approved; that
requires `approved_with_edit`, which is a different recorded action. A second
approval of the same message gets `409`.

**Secrets.** Provider keys and the webhook secret come from the environment only.
`docker-compose.yml` carries no secret values and the Dockerfile bakes none into
the image. `.gitignore` excludes `.env` and `.env.*` while allowing
`.env.example`, which holds names and comments but no values. `internal/config`
refuses to boot without the webhook secret or a key for a provider a tier actually
names — a credential-less pipeline would silently dead-letter every enquiry, which
is a worse failure than not starting. Only providers a tier names need a key, so a
single-vendor deployment carries one credential instead of two. Keys are never
logged; the startup line logs `provider/model`, never the token.

**Sensitive business data.** Customer text is what the model sees, and it leaves
the building on every call — so the boundary is drawn narrowly and on purpose. The
webhook verifies HMAC-SHA256 over the *raw* body with a constant-time compare
before parsing, caps bodies at 1 MiB, and takes the channel from the URL path
rather than the payload, so a caller cannot talk its way into another channel's
handling. `clean()` strips zero-width, bidi, and BOM runes: they let an attacker
pad the dedupe hash, and they hide text from the human reviewer that the model
still reads. Raw payloads are retained as `jsonb` so a disputed extraction can be
re-checked against exactly what arrived. Every state change appends to
`audit_log_entry` with actor and snapshot; simulated enquiries are audited as
`simulated_enquiry_generated`, so the review screen says out loud which rows are
synthetic and who asked for them.

`ponytail:` the `X-Actor-Id` header stands in for SSO. It is the one place this
prototype trusts the caller, marked as such in the code, and it is the seam where
real session auth goes.

## 6. Cost and latency

Cost first, because it compounds:

- **Cheap gates before expensive ones.** Exact-duplicate detection and spam
  heuristics run before any provider call. A redelivered or duplicated enquiry
  costs zero tokens.
- **Tier 1 handles the volume.** Only extractions below `CONFIDENCE_THRESHOLD`
  escalate to tier 2. The threshold is the cost/quality dial and it is a config
  value, not a code change.
- **Drafts are generated once.** `message(enquiry_id, kind)` is unique, so a
  retried enquiry does not pay for a second draft.
- **Junk stops early.** Archiving needs no draft and no approval, so the spam path
  costs one tier 1 call.
- **Temperature is chosen per task, not globally** — `0.0` for extraction, `0.3`
  for drafting, `1.0` only for the demo generator, where variety is the point.

Latency:

- **Nothing waits on a model.** The webhook verifies, normalizes, inserts, and
  returns; extraction and drafting happen behind the queue. Ingest latency is a
  Postgres insert.
- **`WORKER_COUNT` scales the slow part** independently of ingest, and
  `SKIP LOCKED` makes adding workers safe with no coordination.
- **`LLM_TIMEOUT` bounds the tail.** A hung provider costs one timeout, then the
  retry path, not a stuck worker.
- **The human gate is the real latency budget**, and it is where the effort went:
  the review screen shows the customer's words, the model's claims, which claims
  are grounded, the routing reason, and the draft on one screen, so approval is
  seconds of reading rather than minutes of cross-checking.

What is deliberately not done yet: prompt caching, batching, and per-tenant token
budgets. All three are worth doing under real traffic; none of them can be tuned
honestly without traffic to measure.

## 7. What I refuse to automate

**Sending anything to a customer.** Every draft is stored `pending_approval` and a
person approves, edits, or rejects it. There is no confidence score high enough to
unlock an automatic send in this design, and that is a product decision rather than
a missing feature. BEDA's enquiries are commercial conversations — a confidently
wrong reply about pricing, availability, or a support outage costs a customer
relationship, and it cannot be recalled. The asymmetry is the whole argument: an
unnecessary human read costs thirty seconds, an unrecallable wrong reply costs the
account.

Second, narrower, refusal for the same reason: **merging two contact records.**
High-confidence matches *attach* an enquiry to an existing contact, which is
reversible. Declaring two records the same person destroys information that cannot
be reconstructed from an audit log, so `ambiguous_match` routes to
`ActionFlagForHumanMergeReview` and waits, no matter what the score says.

The line between the two lists is reversibility. Archiving junk is automatic
because the row and its raw payload are kept and un-archiving is one update.

## 8. One part in code

The grounding check — the piece that decides whether the model's output is allowed
to touch the database.

```go
// Ground: every extracted field must be backed by a quote that actually appears
// in the source text. Whitespace and case are normalized first, so a model
// reflowing a line break is a formatting difference, not a fabrication.
func Ground(ex *model.Extraction, sourceText string) {
    haystack := norm(sourceText)
    ex.UnverifiedFields = nil

    check := func(field, value string) {
        if strings.TrimSpace(value) == "" {
            return // absent is not ungrounded
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

    // Classification is a judgment, not a copied span: only its quote is
    // checked, never the label itself.
    for _, f := range []string{"enquiry_type", "urgency"} {
        if q, ok := ex.GroundingQuotes[f]; ok && strings.TrimSpace(q) != "" &&
            !strings.Contains(haystack, norm(q)) {
            ex.UnverifiedFields = append(ex.UnverifiedFields, f)
        }
    }
}

// Trusted returns the contact block with every ungrounded field dropped. This is
// what the CRM sync writes: the model proposes, code decides.
func Trusted(ex model.Extraction) model.ExtractedContact {
    c := ex.Contact
    if ex.Unverified("name")         { c.Name = nil }
    if ex.Unverified("email")        { c.Email = nil }
    if ex.Unverified("phone")        { c.Phone = nil }
    if ex.Unverified("company_name") { c.CompanyName = nil }
    return c
}
```

Two properties matter more than the code. `Trusted` is called *before*
`MatchContact`, not just before the write — so a hallucinated email cannot even be
used to look up a contact, let alone create one. And an unverified `enquiry_type`
is handled by the router as low confidence
(`internal/router/router.go:26`), which means an ungrounded classification cannot
route itself.

The configuration side of the same idea — the model tier as data, so escalation is
a value and not a branch:

```go
// A tier is one rung of the two-tier setup. Neither is tied to a vendor: tier 2
// can be OpenAI, both tiers can be the same provider, or they can be split.
type Tier struct {
    Provider string // openai | anthropic
    Model    string
}

// newModel is the only provider-specific code in the project.
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
```

```env
# Split across vendors (the default)
TIER1_PROVIDER=openai      TIER1_MODEL=gpt-4o-mini
TIER2_PROVIDER=anthropic   TIER2_MODEL=claude-sonnet-4-5

# Or one vendor, one credential — no code change, no key you never use
TIER1_PROVIDER=anthropic   TIER1_MODEL=claude-haiku-4-5
TIER2_PROVIDER=anthropic   TIER2_MODEL=claude-sonnet-4-5
```

And the queue claim, because it is the reason a crash is safe:

```sql
UPDATE enquiry SET status='processing', attempts=attempts+1,
       locked_until=now()+$lock_ttl
WHERE id = (
    SELECT id FROM enquiry
    WHERE (status='received'   AND next_attempt_at <= now())  -- due
       OR (status='processing' AND locked_until    <  now())  -- lease expired
    ORDER BY received_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING ...
```

The second `WHERE` arm is the recovery path: a worker that dies holding a row does
not have to clean up after itself, because the expired lease makes the row due
again. Combined with the unique indexes, replaying an enquiry is safe — which is
what lets the retry policy be as blunt as "try again".

