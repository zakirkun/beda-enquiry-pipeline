// Shared API types and fetch helpers. The Go API is the source of truth for
// these shapes; they mirror internal/store/review.go.
export type QueueItem = {
  enquiry_id: string;
  subject: string;
  sender_identifier: string;
  source_channel: string;
  status: string;
  status_reason: string;
  received_at: string;
  enquiry_type: string | null;
  urgency: string | null;
  confidence: number | null;
  intent_summary: string | null;
  team: string | null;
  owner_name: string | null;
  message_id: string | null;
  message_kind: string | null;
  message_status: string | null;
  unverified_count: number;
};

export type Extraction = {
  enquiry_type: string;
  confidence: number;
  urgency: string;
  intent_summary: string;
  contact: {
    name: string | null;
    email: string | null;
    phone: string | null;
    company_name: string | null;
  };
  missing_fields: string[] | null;
  grounding_quotes: Record<string, string> | null;
  model_used: string;
  unverified_fields: string[] | null;
  escalated: boolean;
};

export type Message = {
  id: string;
  direction: string;
  kind: string;
  body: string;
  status: string;
  drafted_by: string;
  created_at: string;
  sent_at: string | null;
};

export type DuplicateCandidate = {
  id: string;
  contact_id: string;
  contact_name: string;
  match_score: number;
  match_method: string;
  resolution: string;
};

export type AuditEntry = {
  id: string;
  entity_type: string;
  entity_id: string | null;
  action: string;
  actor: string;
  payload: unknown;
  created_at: string;
};

export type EnquiryDetail = {
  enquiry: {
    id: string;
    source_channel: string;
    received_at: string;
    sender_identifier: string;
    subject: string;
    normalized_text: string;
    dedupe_hash: string;
    status: string;
    status_reason: string;
    attempts: number;
  };
  extraction: Extraction | null;
  raw_model_output: string;
  crm_record: {
    id: string;
    type: string;
    status: string;
    stage: string;
    team: string;
    created_at: string;
  } | null;
  owner_name: string | null;
  contact: {
    name: string | null;
    email: string | null;
    phone: string | null;
    company_name: string | null;
  } | null;
  messages: Message[];
  duplicate_candidates: DuplicateCandidate[];
  audit: AuditEntry[];
};

export type User = { id: string; name: string; role: string; team: string };

export type Scenario = {
  key: string;
  label: string;
  channel: string;
  expect: string;
};

export type SimulateResult = {
  scenario: string;
  expect: string;
  channel: string;
  model_used: string;
  payload: Record<string, string>;
  gateway: { enquiry_id: string; status: string };
};

export const API =
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

/** The signed-in actor is stored client-side and sent on every request, because
 *  the API records who approved what, not that "the system" acted. */
export const ACTOR_KEY = "beda.actor";

export async function apiFetch<T>(
  path: string,
  actorId: string,
  init?: RequestInit,
): Promise<T> {
  const res = await fetch(`${API}${path}`, {
    ...init,
    cache: "no-store",
    headers: {
      "Content-Type": "application/json",
      "X-Actor-Id": actorId,
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const detail = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(detail.error ?? `request failed (${res.status})`);
  }
  return res.json() as Promise<T>;
}
