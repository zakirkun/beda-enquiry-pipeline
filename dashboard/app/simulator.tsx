"use client";

import { useEffect, useState } from "react";
import { ACTOR_KEY, API, apiFetch, type Scenario, type SimulateResult, type User } from "@/lib/api";

/** Simulator panel: asks the LLM to write a realistic BEDA enquiry for one
 *  scenario, then posts it through the real signed webhook. Ops-only — the API
 *  enforces that; hiding the controls just avoids offering a guaranteed 403. */
export function Simulator({ onSent }: { onSent: () => void }) {
  const [scenarios, setScenarios] = useState<Scenario[]>([]);
  const [isOps, setIsOps] = useState(false);
  const [busy, setBusy] = useState("");
  const [sent, setSent] = useState<SimulateResult[]>([]);
  const [error, setError] = useState("");

  // The picker stores only the id and reloads the page on change, so resolving
  // the role once on mount is enough.
  useEffect(() => {
    const id = localStorage.getItem(ACTOR_KEY);
    if (!id) return;
    fetch(`${API}/api/users`, { cache: "no-store" })
      .then((r) => r.json())
      .then((list: User[]) => {
        if (list.find((u) => u.id === id)?.role !== "ops_admin") return;
        setIsOps(true);
        return apiFetch<Scenario[]>("/api/simulate/scenarios", id).then(setScenarios);
      })
      .catch(() => setScenarios([]));
  }, []);

  async function send(key: string) {
    const id = localStorage.getItem(ACTOR_KEY);
    if (!id) return;
    setBusy(key);
    setError("");
    try {
      const res = await apiFetch<SimulateResult>("/api/simulate", id, {
        method: "POST",
        body: JSON.stringify({ scenario: key }),
      });
      setSent((s) => [res, ...s].slice(0, 5));
      onSent();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  if (!isOps) return null;

  return (
    <div className="panel">
      <h2>Simulate an enquiry</h2>
      <p className="muted small" style={{ margin: "0 0 12px" }}>
        Writes a realistic enquiry with the LLM and posts it to the signed
        webhook, exactly as an inbound provider would — same auth, same
        normalization, same queue. Each click generates fresh details, so
        enquiries do not collapse into one another on dedupe.
      </p>
      <div className="row">
        {scenarios.map((s) => (
          <button
            key={s.key}
            onClick={() => void send(s.key)}
            disabled={busy !== ""}
            title={`Expect: ${s.expect}`}
          >
            {busy === s.key ? "Writing…" : s.label}
          </button>
        ))}
        {scenarios.length === 0 && (
          <span className="muted small">Loading scenarios…</span>
        )}
      </div>

      {error && <p className="err small">{error}</p>}

      {sent.length > 0 && (
        <table style={{ marginTop: 14 }}>
          <thead>
            <tr>
              <th>Generated</th>
              <th>Channel</th>
              <th>Should become</th>
              <th>Written by</th>
            </tr>
          </thead>
          <tbody>
            {sent.map((r) => (
              <tr key={r.gateway.enquiry_id}>
                <td className="small">
                  {r.payload.subject ?? r.payload.text ?? r.payload.message ?? r.scenario}
                  <div className="muted">
                    {r.payload.from ?? r.payload.email ?? r.payload.sender_handle}
                  </div>
                </td>
                <td className="small">{r.channel}</td>
                <td className="small muted">{r.expect}</td>
                <td className="small muted">{r.model_used}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
