"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { ACTOR_KEY, apiFetch, type QueueItem } from "@/lib/api";
import { Simulator } from "./simulator";

const FILTERS: { label: string; statuses: string }[] = [
  { label: "Pending approval", statuses: "pending_approval" },
  { label: "Needs human review", statuses: "needs_human_review" },
  { label: "Failed", statuses: "failed" },
  { label: "Everything", statuses: "" },
];

function confidenceBadge(item: QueueItem) {
  if (item.confidence === null) return <span className="muted">—</span>;
  // 0.72 is the routing threshold (config CONFIDENCE_THRESHOLD): below it the
  // pipeline never acts on its own.
  const cls = item.confidence < 0.72 ? "badge warn" : "badge";
  return <span className={cls}>{item.confidence.toFixed(2)}</span>;
}

export default function QueuePage() {
  const [filter, setFilter] = useState(FILTERS[0]);
  const [items, setItems] = useState<QueueItem[] | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    const actor = localStorage.getItem(ACTOR_KEY);
    if (!actor) {
      setError("Pick an actor above to load the queue.");
      return;
    }
    try {
      const q = filter.statuses ? `?status=${filter.statuses}` : "";
      setItems(await apiFetch<QueueItem[]>(`/api/queue${q}`, actor));
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, [filter]);

  useEffect(() => {
    void load();
    // Poll: enquiries arrive from webhooks, so the queue changes without any
    // action on this page.
    const t = setInterval(() => void load(), 5000);
    return () => clearInterval(t);
  }, [load]);

  return (
    <>
      <Simulator onSent={() => setFilter(FILTERS[3])} />

      <div className="panel">
        <h2>Review queue</h2>
        <div className="row">
          {FILTERS.map((f) => (
            <button
              key={f.label}
              className={f.label === filter.label ? "primary" : ""}
              onClick={() => setFilter(f)}
            >
              {f.label}
            </button>
          ))}
          <button onClick={() => void load()}>Refresh</button>
        </div>
      </div>

      {error && <p className="err">{error}</p>}

      <div className="panel">
        {items === null && !error && <p className="muted">Loading…</p>}
        {items?.length === 0 && (
          <p className="muted">
            Nothing in this queue. Post an enquiry to a webhook and it will
            appear within a few seconds.
          </p>
        )}
        {items && items.length > 0 && (
          <table>
            <thead>
              <tr>
                <th>Received</th>
                <th>From</th>
                <th>Subject / intent</th>
                <th>Type</th>
                <th>Urgency</th>
                <th>Conf.</th>
                <th>Owner</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((it) => (
                <tr key={it.enquiry_id}>
                  <td className="small muted">
                    {new Date(it.received_at).toLocaleString()}
                  </td>
                  <td className="small">
                    {it.sender_identifier}
                    <div className="muted">{it.source_channel}</div>
                  </td>
                  <td>
                    {it.subject || <span className="muted">(no subject)</span>}
                    {it.intent_summary && (
                      <div className="muted small">{it.intent_summary}</div>
                    )}
                    {it.unverified_count > 0 && (
                      <div>
                        <span className="badge danger">
                          {it.unverified_count} ungrounded field
                          {it.unverified_count > 1 ? "s" : ""}
                        </span>
                      </div>
                    )}
                  </td>
                  <td>{it.enquiry_type ?? <span className="muted">—</span>}</td>
                  <td>
                    {it.urgency === "high" ? (
                      <span className="badge warn">high</span>
                    ) : (
                      (it.urgency ?? <span className="muted">—</span>)
                    )}
                  </td>
                  <td>{confidenceBadge(it)}</td>
                  <td className="small">
                    {it.owner_name ?? <span className="muted">unassigned</span>}
                    {it.team && <div className="muted">{it.team}</div>}
                  </td>
                  <td className="small">
                    {it.status}
                    {it.status_reason && (
                      <div className="muted">{it.status_reason}</div>
                    )}
                  </td>
                  <td>
                    <Link href={`/enquiries/${it.enquiry_id}`}>
                      {it.message_status === "pending_approval"
                        ? "Review draft"
                        : "Open"}
                    </Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
