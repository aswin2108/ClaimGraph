import type { Path, Report, Step } from "../types";
import { CATEGORY_COLOR, DISPOSITION_LABEL, ENTITY_LABEL } from "../ui";

interface Props {
  report: Report | null;
  focusClaim: string | null;
  onFocus: (claimId: string | null) => void;
}

const catOf = (s: Step): string => {
  switch (s.entity_type) {
    case "PayoutDestination":
      return s.entity_subtype === "claimant" ? "payout" : "vendor_payout";
    case "Device":
      return s.entity_public ? "public_device" : "device";
    case "ContactPoint":
      return "contact";
    case "Address":
      return "address";
    case "RepairShop":
      return "repair_shop";
    case "IPAddress":
      return s.entity_public === false ? "private_ip" : "public_ip";
    default:
      return "tow_provider";
  }
};

function PathTrail({ path }: { path: Path }) {
  const first = path.steps[0];
  return (
    <div className="trail" title={path.description}>
      <span className="node-chip claim">{first.from_claim}</span>
      {path.steps.map((s, i) => (
        <span className="seg" key={i}>
          <span className="link-line" />
          <span
            className="node-chip ent"
            style={{ borderColor: CATEGORY_COLOR[catOf(s)] ?? "#94a3b8", color: CATEGORY_COLOR[catOf(s)] ?? "#475569" }}
            title={[s.from_evidence && `Evidence ${s.from_evidence}`, s.from_recorded_at && `Recorded ${s.from_recorded_at}`].filter(Boolean).join(" · ")}
          >
            <small>{ENTITY_LABEL[s.entity_type] ?? s.entity_type}</small>
            {s.entity_id}
          </span>
          <span className="link-line" />
          <span className="node-chip claim">{s.to_claim}</span>
        </span>
      ))}
    </div>
  );
}

export default function EvidencePanel({ report, focusClaim, onFocus }: Props) {
  const inv = report?.investigation;
  return (
    <section className="card evidence">
      <header className="card-head">
        <h2>Evidence</h2>
        {inv && <span className="muted small">{inv.related_claims.length} connected claim(s)</span>}
      </header>

      {inv && inv.related_claims.length === 0 && <p className="muted">{inv.message ?? "No supporting connection was found."}</p>}

      <div className="ev-list">
        {inv?.related_claims.map((rc) => (
          <article key={rc.claim_id} className={`ev-item ${focusClaim === rc.claim_id ? "on" : ""}`}>
            <button className="ev-head" onClick={() => onFocus(focusClaim === rc.claim_id ? null : rc.claim_id)}>
              <strong>{rc.claim_id}</strong>
              <span className="chip small-chip">{rc.hops} hop{rc.hops === 1 ? "" : "s"}</span>
              <span className="muted small">Filed {rc.filing_date ?? "—"}</span>
              <span className="muted small push">{rc.paths.length} path{rc.paths.length === 1 ? "" : "s"}</span>
            </button>
            {rc.paths.map((p, i) => (
              <PathTrail key={i} path={p} />
            ))}
          </article>
        ))}
      </div>

      {report && report.prior_cases.length > 0 && (
        <div className="block">
          <h3>Prior outcomes on related claims</h3>
          <ul className="plain">
            {report.prior_cases.map((c) => (
              <li key={c.case_id}>
                <strong>{c.claim_id}</strong> — {DISPOSITION_LABEL[c.disposition] ?? c.disposition}
                {c.timestamp && <span className="muted small"> · {c.timestamp}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      {report && report.uncertainty.length > 0 && (
        <div className="block">
          <h3>Uncertainty and innocent explanations</h3>
          <ul className="plain">
            {report.uncertainty.map((u) => (
              <li key={u}>{u}</li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}
