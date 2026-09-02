# Process Flow — BEDA Enquiry Intelligence Pipeline

## End-to-end decision flow

```mermaid
flowchart TD
    S0([Enquiry received]) --> S1[Normalize and store raw payload]
    S1 --> S2{Exact duplicate of recent enquiry?}
    S2 -- yes --> S2a[Discard and log as duplicate]
    S2 -- no --> S3[Spam heuristic check]
    S3 -- looks like spam --> S3a{LLM confirms junk?}
    S3a -- yes --> J1[Archive as junk]
    S3a -- no --> S4[LLM classify and extract fields]
    S3 -- passes --> S4
    S4 --> S5{Confidence above threshold?}
    S5 -- no --> H1[Route to human review queue]
    S5 -- yes --> S6{Required fields present?}
    S6 -- no --> S7[Draft clarifying question]
    S7 --> APR1[Await human approval]
    S6 -- yes --> S8[Match against existing CRM records]
    S8 --> S9{Confident match found?}
    S9 -- yes --> S10[Attach to existing contact]
    S9 -- ambiguous --> S9a[Flag for human merge decision]
    S9 -- no match --> S11[Create new CRM record]
    S10 --> S12[Assign owner via routing rules]
    S11 --> S12
    S12 --> S13[Draft next response]
    S12 --> S16[Notify owner]
    S13 --> APR2[Await human approval]
    APR2 -- approved or edited --> S14[Send response]
    APR2 -- rejected --> S15[Return to owner queue]
    S14 --> AUD[Write audit log entry]
    S15 --> AUD
    J1 --> AUD
    S2a --> AUD
    H1 --> AUD
    S9a --> AUD
```

## Enquiry status lifecycle

```mermaid
stateDiagram-v2
    [*] --> Received
    Received --> Deduplicated
    Deduplicated --> Discarded_Duplicate
    Deduplicated --> SpamCheck
    SpamCheck --> Archived_Junk
    SpamCheck --> Classified
    Classified --> NeedsHumanReview
    Classified --> NeedsInfo
    Classified --> Routed
    NeedsInfo --> PendingApproval
    Routed --> PendingApproval
    PendingApproval --> Sent
    PendingApproval --> ReturnedToQueue
    ReturnedToQueue --> PendingApproval
    Sent --> Closed
    NeedsHumanReview --> Routed
    NeedsHumanReview --> Archived_Junk
    Discarded_Duplicate --> [*]
    Archived_Junk --> [*]
    Closed --> [*]
```

## Why the flow is shaped this way
- **Cheap checks run before expensive ones.** Exact-duplicate hashing and rule-based spam heuristics run before any LLM call, so the model is only invoked on enquiries that actually need judgment — this is most of the cost control described in `04-ARCHITECTURE.md` §6.
- **Confidence gates before completeness gates.** If the model itself isn't confident in its classification, it goes to a human before the system even asks "is this enquiry complete?" — a low-confidence classification shouldn't be trusted to decide it needs more info either.
- **Every terminal path writes an audit entry**, including the "boring" ones (discarded duplicate, archived junk). Nothing exits the pipeline without a trace.
- **Two distinct approval gates exist** — one for drafted messages (`APR1`/`APR2`) and one for ambiguous duplicate merges (`S9a`). They're separate because they carry different risk: a bad draft wastes a customer's time, a bad merge corrupts the CRM.
