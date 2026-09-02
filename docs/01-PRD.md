# Product Requirements Document — BEDA Enquiry Intelligence Pipeline

## 1. Problem Statement
BEDA receives business enquiries through email, website forms, and messaging channels. Volume and format are inconsistent: some enquiries are qualified sales opportunities, some are support questions, some are junk, and many are missing information needed to act on them. Today this triage is manual, slow, and inconsistent — risking lost opportunities and delayed support responses.

## 2. Goals
- Ingest enquiries from multiple channels into one consistent pipeline.
- Classify each enquiry (sales / support / junk / insufficient information) and extract structured data.
- Create or update the correct CRM record without creating duplicates.
- Draft a next response for every actionable enquiry, ready for human review.
- Route and alert the correct owner or team.
- Keep a complete, reliable audit trail of every automated decision and every human action.
- Never let the system autonomously perform a consequential, hard-to-reverse action.

## 3. Non-Goals (v1)
- Fully autonomous sending of replies with no human in the loop.
- Outbound campaigns or cold outreach.
- Voice channel ingestion (phone calls).
- Deep translation beyond basic language detection (unsupported languages route to a human).
- CRM analytics/BI — the CRM here is a system of record, not a reporting tool.

## 4. Users / Personas
- **Sales rep** — owns leads, reviews/approves drafted replies, resolves ambiguous duplicate matches.
- **Support agent** — owns tickets, reviews/approves drafted replies.
- **Ops/Admin** — configures routing rules, monitors the review queue, manages permissions.
- **Manager** — audits decisions, reviews cost/quality metrics, resolves escalations.

## 5. Functional Requirements
| # | Requirement |
|---|---|
| F1 | Accept enquiries via email, website form, and messaging webhooks; normalize into one internal schema |
| F2 | Deduplicate exact repeat submissions (retries, double-clicks, forwards) before any processing |
| F3 | Classify enquiry type and extract structured fields with a confidence score |
| F4 | Detect and archive junk/spam without creating CRM noise |
| F5 | When required information is missing, draft a clarifying question instead of guessing |
| F6 | Match enquiries against existing Contacts/Companies before creating new CRM records |
| F7 | Create or update CRM record (Lead, Opportunity, or Ticket) deterministically from validated data |
| F8 | Draft the next customer-facing response for every actionable enquiry |
| F9 | Require explicit human approval before any customer-facing message is sent |
| F10 | Require explicit human approval before any destructive/consequential CRM action (merge, delete, mark-as-lost, discount commitment) |
| F11 | Route/alert the correct owner or team based on configurable rules |
| F12 | Record an immutable audit log entry for every automated decision and every human action, including the model's raw output where relevant |
| F13 | Provide a review dashboard for low-confidence enquiries, ambiguous duplicates, and pending approvals |

## 6. Non-Functional Requirements
- **Reliability** — no enquiry is silently dropped; every failure path lands somewhere a human can see it.
- **Auditability** — every automated decision must be reconstructable: what was seen, what the model said, what action was taken, who approved it.
- **Latency** — inbound webhook acknowledgment under ~1s; end-to-end classification and draft within a couple of minutes is fine (not a real-time chat use case).
- **Cost** — LLM spend should scale sub-linearly with volume via tiered models and caching, not one expensive call per enquiry regardless of difficulty.
- **Security** — least-privilege access to CRM and secrets; sensitive data encrypted at rest and in transit.
- **Idempotency** — reprocessing an enquiry after a crash/retry must never create duplicate CRM records or duplicate outbound messages.

## 7. Success Metrics
- % of enquiries correctly classified (via periodic human sampling / override rate).
- % of drafts a human had to fully rewrite (should trend down over time).
- False-negative rate on junk — a missed sales enquiry misclassified as junk is expensive, so thresholds should be biased against that failure mode.
- Median time from enquiry received to draft ready for approval.
- Duplicate CRM record rate.
- Cost per processed enquiry.

## 8. Key Design Principles
1. **Deterministic core, LLM at the edges** — anything that must be reliable, explainable, and cheap (dedup, routing, CRM writes) is plain code; anything that needs language understanding (classification, extraction, drafting) uses an LLM.
2. **Human approves anything consequential or externally visible** — sending messages and destructive CRM changes always wait for a person.
3. **Everything is logged** — raw input, model output, decision, and human action are all persisted and linkable.
4. **Uncertainty routes to a human, it doesn't get guessed away.**

See `04-ARCHITECTURE.md` for how each of these is implemented.
