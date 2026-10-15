import { useEffect, useState } from "react";
import { DISPOSITION_LABEL } from "../ui";

interface Props {
  /** Changes with the selected claim: pending confirmations and notes never carry over. */
  claimId: string | null;
  dispositions: string[];
  current?: string;
  busy: boolean;
  message: { ok: boolean; text: string } | null;
  onRecord: (disposition: string, notes: string) => Promise<boolean>;
}

const CONSEQUENTIAL = "confirmed_fraud";

export default function DispositionPanel({ claimId, dispositions, current, busy, message, onRecord }: Props) {
  const [notes, setNotes] = useState("");
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    setNotes("");
    setConfirming(false);
  }, [claimId]);

  const click = async (d: string) => {
    if (d === CONSEQUENTIAL && !confirming) {
      setConfirming(true); // a human outcome that raises future scores: ask once
      return;
    }
    setConfirming(false);
    if (await onRecord(d, notes)) setNotes("");
  };

  return (
    <section className="card">
      <header className="card-head">
        <h2>Record disposition</h2>
        {current && <span className="chip">{DISPOSITION_LABEL[current] ?? current}</span>}
      </header>
      <p className="muted small">A simulated investigator decision. It is stored in the graph and shown on future related investigations.</p>
      <textarea
        value={notes}
        maxLength={500}
        rows={2}
        placeholder="Optional notes (not sent to the AI model)"
        aria-label="Disposition notes"
        onChange={(e) => setNotes(e.target.value)}
      />
      <div className="dispo">
        {dispositions.map((d) => (
          <button
            key={d}
            className={`btn ${d === CONSEQUENTIAL ? "btn-danger" : "btn-ghost"}`}
            disabled={busy || !claimId}
            onClick={() => click(d)}
          >
            {d === CONSEQUENTIAL && confirming ? "Click again to confirm" : (DISPOSITION_LABEL[d] ?? d)}
          </button>
        ))}
        {confirming && (
          <button className="link" onClick={() => setConfirming(false)}>
            Cancel
          </button>
        )}
      </div>
      {message && <p className={`note ${message.ok ? "ok" : "bad"}`}>{message.text}</p>}
    </section>
  );
}
