"use client";

import { useEffect, useState } from "react";
import { ACTOR_KEY, API, type User } from "@/lib/api";

/** Picks the acting user. ponytail: this stands in for SSO — the API still
 *  demands a known actor id and records it on every approval, which is the part
 *  that had to be real. Replace with a real session before any deployment. */
export function ActorPicker() {
  const [users, setUsers] = useState<User[]>([]);
  const [actor, setActor] = useState("");

  useEffect(() => {
    fetch(`${API}/api/users`, { cache: "no-store" })
      .then((r) => r.json())
      .then((list: User[]) => {
        setUsers(list);
        const saved = localStorage.getItem(ACTOR_KEY);
        const chosen = list.find((u) => u.id === saved)?.id ?? list[0]?.id ?? "";
        setActor(chosen);
        if (chosen) localStorage.setItem(ACTOR_KEY, chosen);
      })
      .catch(() => setUsers([]));
  }, []);

  function choose(id: string) {
    setActor(id);
    localStorage.setItem(ACTOR_KEY, id);
    // The queue is scoped by the actor's team, so it has to be re-fetched.
    window.location.reload();
  }

  const current = users.find((u) => u.id === actor);

  return (
    <div className="row">
      <label className="small muted" htmlFor="actor">
        Signed in as
      </label>
      <select
        id="actor"
        value={actor}
        onChange={(e) => choose(e.target.value)}
        disabled={users.length === 0}
      >
        {users.length === 0 && <option value="">API unreachable</option>}
        {users.map((u) => (
          <option key={u.id} value={u.id}>
            {u.name} — {u.role.replace(/_/g, " ")}
          </option>
        ))}
      </select>
      {current && (
        <span className="badge" title="Your queue is scoped to this team">
          {current.team}
        </span>
      )}
    </div>
  );
}
