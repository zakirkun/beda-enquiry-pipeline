"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import {
  ACTOR_KEY,
  apiFetch,
  type EnquiryDetail,
  type Extraction,
} from "@/lib/api";

function Grounding({ ex }: { ex: Extraction }) {
  const unverified = ex.unverified_fields ?? [];
  const fields: [string, string | null][] = [
    ["name", ex.contact.name],
    ["email", ex.contact.email],
    ["phone", ex.contact.phone],
    ["company_name", ex.contact.company_name],
  ];
  return (
    <table>
      <thead>
        <tr>
          <th>Field</th>
          <th>Extracted</th>
          <th>Supporting quote from the enquiry</th>
          <th>Verified</th>
        </tr>
      </thead>
      <tbody>
        {fields.map(([key, value]) => {
          const bad = unverified.includes(key);
          return (
            <tr key={key}>
              <td className="small muted">{key}</td>
              <td className="small">{value ?? <span className="muted">null</span>}</td>
              <td className="small muted">
                {ex.grounding_quotes?.[key] ?? "—"}
              </td>
              <td>
                {value === null ? (
                  <span className="muted small">n/a</span>
                ) : bad ? (
                  <span className="badge danger">not in source</span>
                ) : (
                  <span className="badge ok">quoted</span>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export default function EnquiryPage() {
  const { id } = useParams<{ id: string }>();
  const [d, setD] = useState<EnquiryDetail | null>(null);
  const [body, setBody] = useState("");
  const [notes, setNotes] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState("");

  const actorId = () => localStorage.getItem(ACTOR_KEY) ?? "";

  const load = useCallback(async () => {
    try {
      const detail = await apiFetch<EnquiryDetail>(
        `/api/enquiries/${id}`,
        actorId(),
      );
      setD(detail);
      const pending = detail.messages.find(
        (m) => m.direction === "outbound" && m.status === "pending_approval",
      );
      // Only seed the editor once, so a poll cannot overwrite what is being typed.
      setBody((b) => (b === "" && pending ? pending.body : b));
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  const pending = d?.messages.find(
    (m) => m.direction === "outbound" && m.status === "pending_approval",
  );

  async function decide(decision: string) {
    if (!pending) return;
    setBusy(true);
    setError("");
    try {
      const edited = decision === "approved" && body.trim() !== pending.body.trim();
      await apiFetch(`/api/messages/${pending.id}/decision`, actorId(), {
        method: "POST",
        body: JSON.stringify({
          decision: edited ? "approved_with_edit" : decision,
          body: edited ? body : "",
          notes,
        }),
      });
      setDone(
        decision === "rejected"
          ? "Rejected. Back in the owner's queue, nothing sent."
          : edited
            ? "Approved with your edits and sent."
            : "Approved as drafted and sent.",
      );
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function resolveDuplicate(candidateId: string, resolution: string) {
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/api/duplicates/${candidateId}/resolve`, actorId(), {
        method: "POST",
        body: JSON.stringify({ resolution }),
      });
      setDone(
        resolution === "attached"
          ? "Attached to the existing contact. The enquiry is back in the pipeline."
          : "Marked as a different person. The enquiry is back in the pipeline.",
      );
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (error && !d) return <p className="err">{error}</p>;
  if (!d) return <p className="muted">Loading…</p>;

  const ex = d.extraction;
  const unresolved = d.duplicate_candidates.filter(
    (c) => c.resolution === "pending",
  );

  return (
    <>
      <p>
        <Link href="/">← Back to the queue</Link>
      </p>

      {done && <p className="notice">{done}</p>}
      {error && <p className="err">{error}</p>}

      <div className="grid2">
        <div className="panel">
          <h2>What the customer wrote</h2>
          <dl className="kv">
            <dt>From</dt>
            <dd>{d.enquiry.sender_identifier}</dd>
            <dt>Channel</dt>
            <dd>{d.enquiry.source_channel}</dd>
            <dt>Received</dt>
            <dd>{new Date(d.enquiry.received_at).toLocaleString()}</dd>
            <dt>Status</dt>
            <dd>
              {d.enquiry.status}
              {d.enquiry.status_reason && (
                <span className="muted"> ({d.enquiry.status_reason})</span>
              )}
            </dd>
          </dl>
          <h2 style={{ marginTop: 16 }}>Normalized text</h2>
          <pre>{d.enquiry.normalized_text}</pre>
        </div>

        <div className="panel">
          <h2>What the model said</h2>
          {!ex && <p className="muted">Not classified yet.</p>}
          {ex && (
            <>
              <dl className="kv">
                <dt>Type</dt>
                <dd>{ex.enquiry_type}</dd>
                <dt>Urgency</dt>
                <dd>{ex.urgency}</dd>
                <dt>Confidence</dt>
                <dd>
                  {ex.confidence.toFixed(2)}
                  {ex.confidence < 0.72 && (
                    <span className="badge warn" style={{ marginLeft: 8 }}>
                      below threshold
                    </span>
                  )}
                </dd>
                <dt>Model</dt>
                <dd>
                  {ex.model_used}
                  {ex.escalated && (
                    <span className="badge" style={{ marginLeft: 8 }}>
                      escalated to tier 2
                    </span>
                  )}
                </dd>
                <dt>Intent</dt>
                <dd>{ex.intent_summary}</dd>
                {(ex.missing_fields?.length ?? 0) > 0 && (
                  <>
                    <dt>Missing</dt>
                    <dd>{ex.missing_fields!.join(", ")}</dd>
                  </>
                )}
              </dl>
              <h2 style={{ marginTop: 16 }}>Grounding check</h2>
              <p className="small muted">
                Every extracted field must be quotable from the text above.
                Anything marked <em>not in source</em> was excluded from the CRM
                write.
              </p>
              <Grounding ex={ex} />
            </>
          )}
        </div>
      </div>

      {unresolved.length > 0 && (
        <div className="panel">
          <h2>Possible duplicate contact — your decision</h2>
          <p className="small">
            The system never merges records. Attach this enquiry to the existing
            contact, or say it is a different person.
          </p>
          <table>
            <thead>
              <tr>
                <th>Existing contact</th>
                <th>Score</th>
                <th>Method</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {unresolved.map((c) => (
                <tr key={c.id}>
                  <td>{c.contact_name}</td>
                  <td>{c.match_score.toFixed(2)}</td>
                  <td className="small muted">{c.match_method}</td>
                  <td>
                    <div className="row">
                      <button
                        className="primary"
                        disabled={busy}
                        onClick={() => void resolveDuplicate(c.id, "attached")}
                      >
                        Same person — attach
                      </button>
                      <button
                        disabled={busy}
                        onClick={() => void resolveDuplicate(c.id, "separate")}
                      >
                        Different person
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {d.crm_record && (
        <div className="panel">
          <h2>CRM record</h2>
          <dl className="kv">
            <dt>Type</dt>
            <dd>{d.crm_record.type}</dd>
            <dt>Owner</dt>
            <dd>
              {d.owner_name ?? <span className="muted">unassigned</span>}
              {d.crm_record.team && (
                <span className="muted"> · {d.crm_record.team}</span>
              )}
            </dd>
            <dt>Contact</dt>
            <dd>
              {d.contact?.name ?? "—"}
              {d.contact?.email && <span className="muted"> · {d.contact.email}</span>}
              {d.contact?.company_name && (
                <span className="muted"> · {d.contact.company_name}</span>
              )}
            </dd>
          </dl>
        </div>
      )}

      <div className="panel">
        <h2>
          Draft {pending?.kind === "clarifying_question" ? "question" : "reply"}
        </h2>
        {!pending && (
          <p className="muted">
            No draft is awaiting approval.
            {d.messages.length > 0 && " History is below."}
          </p>
        )}
        {pending && (
          <>
            <p className="small muted">
              Drafted by {pending.drafted_by}. Edit freely — approving with
              changes is recorded as your text, not the model&apos;s.
            </p>
            <textarea
              value={body}
              onChange={(e) => setBody(e.target.value)}
              spellCheck
              aria-label="Draft message body"
            />
            <div className="row" style={{ marginTop: 10 }}>
              <input
                style={{ flex: 1, minWidth: 200 }}
                placeholder="Notes for the audit log (optional)"
                value={notes}
                onChange={(e) => setNotes(e.target.value)}
                aria-label="Approval notes"
              />
              <button
                className="primary"
                disabled={busy || body.trim() === ""}
                onClick={() => void decide("approved")}
              >
                Approve &amp; send
              </button>
              <button
                className="danger"
                disabled={busy}
                onClick={() => void decide("rejected")}
              >
                Reject
              </button>
            </div>
          </>
        )}

        {d.messages.length > 0 && (
          <table style={{ marginTop: 16 }}>
            <thead>
              <tr>
                <th>Kind</th>
                <th>Status</th>
                <th>Drafted by</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {d.messages.map((m) => (
                <tr key={m.id}>
                  <td className="small">{m.kind}</td>
                  <td>
                    <span
                      className={
                        m.status === "sent"
                          ? "badge ok"
                          : m.status === "rejected"
                            ? "badge danger"
                            : "badge warn"
                      }
                    >
                      {m.status}
                    </span>
                  </td>
                  <td className="small muted">{m.drafted_by}</td>
                  <td className="small muted">
                    {new Date(m.created_at).toLocaleString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="panel">
        <h2>Audit trail</h2>
        <p className="small muted">
          Every automated decision and every human action, in order.
        </p>
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Action</th>
              <th>Actor</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {d.audit.map((a) => (
              <tr key={a.id}>
                <td className="small muted">
                  {new Date(a.created_at).toLocaleTimeString()}
                </td>
                <td className="small">{a.action}</td>
                <td className="small muted">{a.actor}</td>
                <td>
                  {a.payload ? (
                    <pre className="small">{JSON.stringify(a.payload)}</pre>
                  ) : (
                    <span className="muted">—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {d.raw_model_output && (
        <div className="panel">
          <h2>Raw model output</h2>
          <pre className="small">{d.raw_model_output}</pre>
        </div>
      )}
    </>
  );
}
