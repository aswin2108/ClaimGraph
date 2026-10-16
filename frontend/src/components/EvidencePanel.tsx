import type { GraphStats, Path, Report, Step } from "../types";
import { CATEGORY_COLOR, CATEGORY_LABEL, DISPOSITION_LABEL, ENTITY_LABEL } from "../ui";

interface Props {
  report: Report | null;
  focusClaim: string | null;
  onFocus: (claimId: string | null) => void;
  graphSize?: GraphStats | null; // the whole graph the traversal ran on (db.meta.stats)
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

// "Nothing found" is a stated result, never an empty list to interpret.
const OUTCOME: Record<string, { title: string; body: string }> = {
  no_supporting_connection: {
    title: "No supporting connection found in the graph",
    body: "No claim shares evidence with this one within the searched hops. This is a stated result, not a clearance: the graph only knows the evidence recorded in it.",
  },
  infrastructure_only: {
    title: "Linked only through shared infrastructure",
    body: "Every link runs through infrastructure many unrelated claims use (a busy shop, a carrier IP) or a context-only identifier (a vendor settlement account, a kiosk, a company network). They are reported, but their users are not treated as connections.",
  },
};

export default function EvidencePanel({ report, focusClaim, onFocus, graphSize }: Props) {
  const inv = report?.investigation;
  const stats = inv?.traversal_stats;
  const total = inv?.connected_total ?? inv?.related_claims.length ?? 0;
  const outcome = inv?.outcome ? OUTCOME[inv.outcome] : undefined;
  return (
    <section className="card evidence">
      <header className="card-head">
        <h2>Evidence</h2>
        {inv && (
          <span className="muted small">
            {total > (inv.related_claims.length ?? 0) ? `${inv.related_claims.length} of ${total}` : inv.related_claims.length} connected claim(s)
          </span>
        )}
      </header>
      {stats && (
        <p className="stats-line" title="Wall clock measured in Go around the FalkorDB queries; it varies between runs and is never part of the score">
          <span className="dot dot-ok" /> Traversal ran inside FalkorDB
          {graphSize && graphSize.nodes > 0
            ? ` on a graph of ${graphSize.nodes.toLocaleString("en-US")} nodes / ${graphSize.relationships.toLocaleString("en-US")} relationships`
            : ""}{" "}
          · {stats.queries} quer{stats.queries === 1 ? "y" : "ies"} · {stats.paths_returned} path
          {stats.paths_returned === 1 ? "" : "s"} examined · {stats.duration_ms} ms
        </p>
      )}

      {outcome && (
        <div className="outcome" role="status">
          <strong>{outcome.title}</strong>
          <p className="muted small">{outcome.body}</p>
        </div>
      )}
      {inv && !inv.outcome && inv.related_claims.length === 0 && <p className="muted">{inv.message ?? "No supporting connection was found."}</p>}
      {inv?.truncated && <p className="note bad small">A query limit was reached, so some evidence may be missing from this view.</p>}
      {inv?.paths_trimmed && <p className="muted tiny">Some claims show only example paths; no evidence is lost.</p>}

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

      {!!inv?.infrastructure_links?.length && (
        <div className="block">
          <h3>Shared infrastructure and context (not treated as connections)</h3>
          <ul className="plain">
            {inv.infrastructure_links.map((l) => (
              <li key={`${l.entity_type}:${l.entity_id}`}>
                {CATEGORY_LABEL[l.category] ?? l.category} <strong>{l.entity_id}</strong>
                <span className="muted small"> · used by {l.other_claims.toLocaleString("en-US")} other claims</span>
              </li>
            ))}
          </ul>
        </div>
      )}

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
