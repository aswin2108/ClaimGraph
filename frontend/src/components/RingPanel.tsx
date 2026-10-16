import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "../api";
import type { AlgoLink, AlgoNode, Investigation, Ring, StrongestLink } from "../types";
import { ALGO_CATEGORY, CATEGORY_COLOR, DISPOSITION_LABEL, ENTITY_LABEL, money, RING_INDICATOR_LABEL } from "../ui";
import RingGraph from "./RingGraph";
import ViewTabs, { type GraphViewMode } from "./ViewTabs";

interface Props {
  ring: Ring | null;
  error: string | null;
  busy: boolean;
  traversal: Investigation | null; // the bounded traversal for the same claim, to show what only the ring reveals
  hops: number;
  onView: (v: GraphViewMode) => void;
}

const ENTITY_KIND: Record<string, string> = { ...ENTITY_LABEL, Claimant: "Same claimant" };

// bridge: a claimant between two claims (the same person filed both) is evidence; next to a contact or address it is
// only the route to that shared identifier.
function Chip({ n, bridge = false }: { n: AlgoNode; bridge?: boolean }) {
  if (n.type === "Claim") return <span className="node-chip claim">{n.id}</span>;
  const color = CATEGORY_COLOR[ALGO_CATEGORY[n.type] ?? ""];
  const kind = n.type === "Claimant" && !bridge ? "Claimant" : (ENTITY_KIND[n.type] ?? n.type);
  return (
    <span className={`node-chip ent ${color ? "" : "faint"}`} style={color ? { borderColor: color, color } : undefined}>
      <small>{kind}</small>
      {n.id}
    </span>
  );
}

function LinkTrail({ links }: { links: AlgoLink[] }) {
  return (
    <div className="trail">
      <Chip n={links[0].from} />
      {links.map((l, i) => (
        <span className="seg" key={i}>
          <span className="link-line" title={`${l.rel} · cost ${l.evidence_cost}`} />
          <Chip n={l.to} bridge={l.from.type === "Claim" && links[i + 1]?.to.type === "Claim"} />
        </span>
      ))}
    </div>
  );
}

function indicatorDetail(ind: string, r: Ring): string {
  switch (ind) {
    case "spans_households":
      return `${r.traits.addresses} addresses`;
    case "filing_burst":
      return `${r.traits.max_claims_in_90_days} claims in 90 days`;
    case "new_policies":
      return `${r.traits.claims_on_new_policies} on new policies`;
    case "confirmed_case":
      return `${r.confirmed_fraud_cases} case${r.confirmed_fraud_cases === 1 ? "" : "s"}`;
    default:
      return "";
  }
}

const msg = (e: unknown) => (e instanceof ApiError ? e.message : "Something went wrong.");

export default function RingPanel({ ring, error, busy, traversal, hops, onView }: Props) {
  const [pick, setPick] = useState<string | null>(null);
  const [link, setLink] = useState<StrongestLink | null>(null);
  const [linkBusy, setLinkBusy] = useState(false);
  const [linkErr, setLinkErr] = useState<string | null>(null);
  const ticket = useRef(0);

  // A new ring (another claim, or fresh evidence) clears the chosen link.
  useEffect(() => {
    ticket.current++;
    setPick(null);
    setLink(null);
    setLinkErr(null);
    setLinkBusy(false);
  }, [ring]);

  const choose = async (other: string | null) => {
    const mine = ++ticket.current;
    setPick(other);
    setLink(null);
    setLinkErr(null);
    if (!ring || !other || other === ring.target_claim) {
      setLinkBusy(false);
      return;
    }
    setLinkBusy(true);
    try {
      const l = await api.link(ring.target_claim, other);
      if (mine === ticket.current) setLink(l);
    } catch (e) {
      if (mine === ticket.current) setLinkErr(msg(e));
    } finally {
      if (mine === ticket.current) setLinkBusy(false);
    }
  };

  // Which members the bounded traversal also reached. Only meaningful when the traversal list is complete.
  const reached = new Map<string, number>();
  for (const rc of traversal?.related_claims ?? []) reached.set(rc.claim_id, rc.hops);
  const complete = !!traversal && traversal.target_claim === ring?.target_claim && !traversal.truncated &&
    (traversal.connected_total ?? traversal.related_claims.length) === traversal.related_claims.length;
  const isRing = !!ring && ring.size > 1;
  const ringOnly = isRing && complete ? ring!.members.filter((m) => m.claim_id !== ring!.target_claim && !reached.has(m.claim_id)).length : 0;
  const indicators = ring?.traits.indicators ?? [];

  return (
    <section className="card graph-card ring-card">
      <header className="card-head">
        <h2>Claim ring</h2>
        <div className="head-tools">
          {ring && (
            <span className="chip chip-ai" title={ring.algorithm_results_cached ? "Algorithm result reused for this evidence version" : undefined}>
              {ring.algorithm} · {ring.duration_ms} ms
            </span>
          )}
          <ViewTabs view="ring" onView={onView} hops={hops} ringSize={ring?.size} />
        </div>
      </header>

      {error && <p className="pad bad">{error}</p>}
      {!ring && !error && <p className="pad muted">{busy ? "Running algo.WCC in FalkorDB…" : "No claim selected."}</p>}

      {ring && !isRing && (
        <div className="pad">
          <p className="muted">{ring.message}</p>
          <p className="muted tiny">{ring.note}</p>
        </div>
      )}

      {isRing && ring && (
        <>
          <div className="ring-stats">
            <div>
              <b>{ring.size}</b>
              <span>claims</span>
            </div>
            <div>
              <b>{ring.traits.claimants}</b>
              <span>claimants</span>
            </div>
            <div>
              <b>{ring.traits.addresses}</b>
              <span>addresses</span>
            </div>
            <div>
              <b>{ring.filing_span_days}d</b>
              <span>filing span</span>
            </div>
            <div>
              <b>{money(ring.total_amount)}</b>
              <span>claimed</span>
            </div>
            <div>
              <b>{ring.confirmed_fraud_cases}</b>
              <span>confirmed cases</span>
            </div>
          </div>
          <div className="ring-tags">
            <span className="muted small">{ring.ring_id}</span>
            {indicators.map((i) => (
              <span key={i} className="chip chip-warn small-chip" title="Review indicator (queue order and context, not a score input)">
                {RING_INDICATOR_LABEL[i] ?? i}: {indicatorDetail(i, ring)}
              </span>
            ))}
            {indicators.length === 0 && <span className="chip small-chip">No ring indicators</span>}
            {complete && ringOnly > 0 && (
              <span className="chip chip-ai small-chip">
                {ringOnly} member{ringOnly === 1 ? "" : "s"} not reached by the ≤ {hops}-hop traversal
              </span>
            )}
          </div>

          <div className="graph-stage ring-stage">
            {ring.backbone.length > 0 ? (
              <RingGraph ring={ring} link={link} onPick={choose} />
            ) : (
              <div className="overlay-msg">{ring.message ?? "No backbone to draw."}</div>
            )}
            {ring.truncated && <div className="trunc">{ring.oversized ? "Oversized component: size only" : "Backbone cut at the display limit"}</div>}
          </div>

          <div className="graph-foot ring-foot">
            <div className="ring-link" aria-live="polite">
              {!pick && (
                <span className="muted small">
                  Lines are the minimum evidence backbone (algo.MSF). Click a member claim to find its strongest evidence chain to {ring.target_claim} (algo.SPpaths).
                </span>
              )}
              {pick && linkBusy && <span className="muted small">Finding the strongest link to {pick}…</span>}
              {pick && linkErr && <span className="bad small">{linkErr}</span>}
              {link && (
                <>
                  <div className="ring-link-head">
                    <strong>Strongest link to {link.to_claim}</strong>
                    {link.found && <span className="chip small-chip">evidence cost {link.total_evidence_cost} (lower is stronger)</span>}
                    <span className="muted small">{link.algorithm} · {link.duration_ms} ms</span>
                    <button className="link push" onClick={() => choose(null)}>
                      Clear
                    </button>
                  </div>
                  {link.found && link.links?.length ? <LinkTrail links={link.links} /> : <p className="muted small">{link.description}</p>}
                </>
              )}
            </div>

            <div className="ring-cols">
              <div>
                <h3>Shared identifiers</h3>
                <ul className="plain ring-ents">
                  {ring.shared_entities.map((e) => (
                    <li key={`${e.entity_type}:${e.entity_id}`}>
                      <i style={{ background: CATEGORY_COLOR[ALGO_CATEGORY[e.entity_type] ?? ""] ?? "#94a3b8" }} />
                      <span>{ENTITY_KIND[e.entity_type] ?? e.entity_type}</span> <b>{e.entity_id}</b>
                      <span className="muted small"> · {e.member_claims} claims</span>
                      {/* a "hub" only means something when it ties together more than a pair (a chain has none) */}
                      {e.member_claims >= 3 && ring.hub_entity?.entity_id === e.entity_id && ring.hub_entity.entity_type === e.entity_type && (
                        <span className="chip small-chip">hub</span>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
              <div>
                <h3>Members{ring.oversized ? ` (${ring.members.length} of ${ring.size})` : ""}</h3>
                <div className="ring-members">
                  {ring.members.map((m) => {
                    const target = m.claim_id === ring.target_claim;
                    const h = reached.get(m.claim_id);
                    const status = target ? "target" : complete ? (h ? `${h} hop${h === 1 ? "" : "s"}` : "ring only") : "";
                    const name = [m.claim_id, status, ...(m.recorded_dispositions ?? []).map((d) => DISPOSITION_LABEL[d] ?? d)].filter(Boolean).join(", ");
                    return (
                      <button
                        key={m.claim_id}
                        className={`member ${target ? "target" : ""} ${pick === m.claim_id ? "on" : ""}`}
                        disabled={target}
                        onClick={() => choose(pick === m.claim_id ? null : m.claim_id)}
                        aria-label={name}
                        title={[m.filing_date && `Filed ${m.filing_date}`, money(m.amount)].filter(Boolean).join(" · ")}
                      >
                        <strong>{m.claim_id}</strong>
                        {status && <small>{status}</small>}
                        {m.recorded_dispositions?.map((d) => (
                          <small key={d} className="disp">
                            {DISPOSITION_LABEL[d] ?? d}
                          </small>
                        ))}
                      </button>
                    );
                  })}
                </div>
              </div>
            </div>
            <p className="muted tiny">{ring.note} Ring results are context and queue order only; they never change the priority score.</p>
          </div>
        </>
      )}
    </section>
  );
}
