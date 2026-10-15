import { useState } from "react";
import { api, ApiError } from "../api";
import type { ClaimSummary } from "../types";
import { DISPOSITION_LABEL, money } from "../ui";
import { newClaimId, PRESETS } from "../presets";

interface Props {
  claims: ClaimSummary[];
  selected: string | null;
  onSelect: (id: string) => void;
  onSubmitted: (id: string) => void;
}

export default function QueuePanel({ claims, selected, onSelect, onSubmitted }: Props) {
  const [preset, setPreset] = useState(PRESETS[0].key);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const current = PRESETS.find((p) => p.key === preset) ?? PRESETS[0];

  const submit = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const fnol = current.build(newClaimId());
      const res = await api.submitFNOL(fnol);
      const failed = res.policy_checks.filter((c) => !c.passed).map((c) => c.name.replaceAll("_", " "));
      setMsg({
        ok: true,
        text: `${res.created ? "Received" : "Updated"} ${res.claim_id}. ` +
          (failed.length ? `Ordinary policy checks flagged: ${failed.join(", ")}.` : "Ordinary policy checks passed."),
      });
      onSubmitted(res.claim_id);
    } catch (e) {
      const err = e as ApiError;
      setMsg({ ok: false, text: err.problems?.length ? err.problems.join("; ") : err.message });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="side">
      <section className="card">
        <header className="card-head">
          <h2>New claim (FNOL)</h2>
        </header>
        <div className="presets">
          {PRESETS.map((p) => (
            <button key={p.key} className={`preset ${p.key === preset ? "on" : ""}`} onClick={() => setPreset(p.key)}>
              {p.title}
            </button>
          ))}
        </div>
        <p className="muted small">{current.blurb}</p>
        <button className="btn btn-primary wide" onClick={submit} disabled={busy}>
          {busy ? "Submitting…" : "Submit synthetic FNOL"}
        </button>
        {msg && <p className={`note ${msg.ok ? "ok" : "bad"}`}>{msg.text}</p>}
      </section>

      <section className="card grow">
        <header className="card-head">
          <h2>Claim queue</h2>
          <span className="muted small">{claims.length}</span>
        </header>
        <ul className="queue" aria-label="Claim queue">
          {claims.map((c) => (
            <li key={c.claim_id}>
              <button className={`q-item ${c.claim_id === selected ? "on" : ""}`} onClick={() => onSelect(c.claim_id)}>
                <div className="q-top">
                  <strong>{c.claim_id}</strong>
                  <span className="muted small">{money(c.amount)}</span>
                </div>
                <div className="q-sub">
                  <span className="muted small">Filed {c.filing_date}</span>
                  <span className={`dot ${c.policy_check_status === "passed" ? "dot-ok" : c.policy_check_status === "failed" ? "dot-off" : "dot-unk"}`} title={`Policy checks: ${c.policy_check_status}`} />
                  {c.disposition && <span className="chip small-chip">{DISPOSITION_LABEL[c.disposition] ?? c.disposition}</span>}
                </div>
              </button>
            </li>
          ))}
          {claims.length === 0 && <li className="muted small pad">No claims yet. Run the seed command or submit an FNOL.</li>}
        </ul>
      </section>
    </div>
  );
}
