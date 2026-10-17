import { useEffect, useState } from "react";
import type { RecordedDecision } from "../types";
import { DISPOSITION_LABEL } from "../ui";

type Outcome = "approved" | "rejected";

interface Props {
  /** Changes with the selected claim: pending confirmations and the reason never carry over. */
  claimId: string | null;
  /** The investigator has a report to decide on (AI or deterministic). The buttons appear only then. */
  hasReport: boolean;
  /** The report can be sealed (the server kept it). */
  canClose: boolean;
  /** The latest recorded decision on this claim, with its date and notes. */
  decision?: RecordedDecision | null;
  busy: boolean;
  message: { ok: boolean; text: string } | null;
  onClose: (outcome: Outcome, reason: string) => Promise<boolean>;
  onNeedInfo: (notes: string) => Promise<boolean>;
}

export const when = (iso: string) => {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString("en-GB", { dateStyle: "medium", timeStyle: "short" });
};

const CONFIRM: Record<Outcome, string> = {
  approved: "Confirm: approve and close",
  rejected: "Confirm: reject and close",
};

/**
 * The investigator's decision, under the report it is based on. Approve / Reject close the
 * investigation for good and seal the report; Need more information keeps it open.
 */
export default function DecisionBox({ claimId, hasReport, canClose, decision, busy, message, onClose, onNeedInfo }: Props) {
  const [reason, setReason] = useState("");
  const [confirming, setConfirming] = useState<Outcome | null>(null);

  useEffect(() => {
    setReason("");
    setConfirming(null);
  }, [claimId]);

  const close = async (o: Outcome) => {
    if (confirming !== o) {
      setConfirming(o); // closing is final: ask once
      return;
    }
    setConfirming(null);
    if (await onClose(o, reason)) setReason("");
  };
  const needInfo = async () => {
    setConfirming(null);
    if (await onNeedInfo(reason)) setReason("");
  };
  const noReason = reason.trim() === "";

  return (
    <div className="decision-box" aria-label="Your decision">
      {decision && (
        <>
          {/* The latest decision stored on this claim: one made earlier in this app, or part of the synthetic
              case history the demo book is seeded with. It is not the decision being made below. */}
          <h3>Already recorded on this claim</h3>
          <div className="decision" aria-label="Recorded decision">
            <div>
              <strong>{DISPOSITION_LABEL[decision.disposition] ?? decision.disposition}</strong>
              <span className="muted small">
                {" "}
                · recorded {when(decision.recorded_at)}
                {decision.source && decision.source !== "investigator_simulated" ? " · from the synthetic case history" : ""}
              </span>
            </div>
            {decision.notes ? <p className="small">“{decision.notes}”</p> : <p className="muted small">No notes.</p>}
            <p className="muted tiny">An earlier decision (made before, or part of the demo's synthetic case history). Yours goes below.</p>
          </div>
        </>
      )}
      <h3>Your decision</h3>
      {!hasReport ? (
        <p className="muted small">Run the investigation agent first. Approve, Reject and Need more information appear under its report.</p>
      ) : (
        <>
          <p className="muted small">
            Based on the report above. Approve or Reject closes the investigation for good and seals this report; Need more
            information keeps it open.
          </p>
          <textarea
            value={reason}
            maxLength={500}
            rows={2}
            placeholder="Reason (required to close; never sent to the AI model)"
            aria-label="Decision reason"
            onChange={(e) => setReason(e.target.value)}
          />
          <div className="dispo">
            <button className="btn btn-ok" disabled={busy || !canClose || noReason} onClick={() => close("approved")}>
              {confirming === "approved" ? CONFIRM.approved : "Approve and close"}
            </button>
            <button className="btn btn-danger" disabled={busy || !canClose || noReason} onClick={() => close("rejected")}>
              {confirming === "rejected" ? CONFIRM.rejected : "Reject and close"}
            </button>
            <button className="btn btn-ghost" disabled={busy || !claimId} onClick={needInfo}>
              Need more information
            </button>
            {confirming && (
              <button className="link" onClick={() => setConfirming(null)}>
                Cancel
              </button>
            )}
          </div>
          {noReason && <p className="muted tiny">Write a reason to approve or reject.</p>}
          {!canClose && <p className="muted tiny">This report can no longer be sealed. Run the agent again to close the investigation.</p>}
        </>
      )}
      {message && <p className={`note ${message.ok ? "ok" : "bad"}`}>{message.text}</p>}
    </div>
  );
}
