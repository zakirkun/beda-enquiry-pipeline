# Entity Relationship Diagram — BEDA Enquiry Intelligence Pipeline

Everything downstream of the raw enquiry is designed to be traceable back to it: every extraction, every CRM write, every message, and every human decision links back through `enquiry_id`.

```mermaid
erDiagram
    ENQUIRY ||--o{ ATTACHMENT : has
    ENQUIRY ||--|| EXTRACTION_RESULT : produces
    ENQUIRY ||--o{ AUDIT_LOG_ENTRY : generates
    ENQUIRY ||--o{ DUPLICATE_MATCH_CANDIDATE : checked_against
    ENQUIRY ||--o| CRM_RECORD : creates_or_updates
    CONTACT }o--|| COMPANY : belongs_to
    CONTACT ||--o{ CRM_RECORD : owns
    CRM_RECORD }o--|| USER : assigned_to
    CRM_RECORD ||--o{ MESSAGE : has
    MESSAGE ||--o| APPROVAL_ACTION : reviewed_by
    APPROVAL_ACTION }o--|| USER : performed_by
    DUPLICATE_MATCH_CANDIDATE }o--|| CONTACT : matches
    ROUTING_RULE }o--o| USER : targets

    ENQUIRY {
        uuid id PK
        string source_channel
        timestamp received_at
        string sender_identifier
        text normalized_text
        string dedupe_hash
        string status
    }
    ATTACHMENT {
        uuid id PK
        uuid enquiry_id FK
        string storage_ref
        string mime_type
        int size_bytes
    }
    EXTRACTION_RESULT {
        uuid id PK
        uuid enquiry_id FK
        string model_used
        json extracted_json
        float confidence_score
        json hallucination_flags
        timestamp created_at
    }
    CONTACT {
        uuid id PK
        string name
        string email
        string phone
        uuid company_id FK
        uuid source_enquiry_id FK
    }
    COMPANY {
        uuid id PK
        string name
        string domain
        string industry
    }
    CRM_RECORD {
        uuid id PK
        string type
        uuid contact_id FK
        uuid owner_user_id FK
        string status
        string stage
        timestamp created_at
        timestamp updated_at
    }
    MESSAGE {
        uuid id PK
        uuid crm_record_id FK
        string direction
        text body
        string status
        string drafted_by
        timestamp created_at
    }
    APPROVAL_ACTION {
        uuid id PK
        uuid message_id FK
        uuid actor_user_id FK
        string decision
        text notes
        timestamp created_at
    }
    AUDIT_LOG_ENTRY {
        uuid id PK
        uuid enquiry_id FK
        string entity_type
        uuid entity_id
        string action
        string actor
        string payload_snapshot_ref
        timestamp created_at
    }
    DUPLICATE_MATCH_CANDIDATE {
        uuid id PK
        uuid enquiry_id FK
        uuid matched_contact_id FK
        float match_score
        string match_method
        string resolution
        uuid resolved_by FK
    }
    ROUTING_RULE {
        uuid id PK
        json condition_json
        string target_team
        uuid target_user_id FK
        int priority
        boolean active
    }
    USER {
        uuid id PK
        string name
        string role
        string team
    }
```

## Notes on the model
- **`EXTRACTION_RESULT` is kept separate from `ENQUIRY`**, not merged into it. This preserves the raw model output (including confidence and hallucination flags) as its own auditable record, distinct from the source text — you should always be able to compare what the model said against what the customer actually wrote.
- **`DUPLICATE_MATCH_CANDIDATE` is a first-class table, not a boolean flag.** Every dedupe check against the CRM is recorded, whether or not it resulted in a merge — this is what makes duplicate handling explainable rather than a black box.
- **`MESSAGE.status`** moves through `draft → pending_approval → approved → sent` or `rejected`. No message reaches `sent` without a corresponding `APPROVAL_ACTION`.
- **`ROUTING_RULE`** is plain configuration data, editable by Ops without a code deploy — see `04-ARCHITECTURE.md` §3 for why routing is deterministic rather than LLM-driven.
