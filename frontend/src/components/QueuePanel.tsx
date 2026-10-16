import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "../api";
import type { ClaimSummary, RingList, RingSummary } from "../types";
import { DISPOSITION_LABEL, money, RING_INDICATOR_LABEL, RING_INDICATOR_SHORT } from "../ui";
import type { Preset } from "../presets";
import IntakeCard from "./IntakeCard";

interface Props {
  claims: ClaimSummary[];
  selected: string | null;
  onSelect: (id: string) => void;
  onSubmitted: (id: string) => void;
  ringsVersion: number; // bumped when new evidence or outcomes may change the ring list
  activeRingId?: string; // ring of the selected claim
  onOpenRing: (claimId: string) => void;
  presets: Preset[]; // FNOL scenarios for the graph the API serves
  intakeKey: number; // changes after a demo reset, so the intake card starts over
}

function traitDetail(ind: string, r: RingSummary): string {
  switch (ind) {
    case "spans_households":
      return `${r.addresses} addresses`;
    case "filing_burst":
      return `${r.max_claims_in_90_days} claims within 90 days`;
    case "new_policies":
      return `${r.claims_on_new_policies} claims on policies under 60 days old`;
    case "confirmed_case":
      return `${r.confirmed_fraud_cases} confirmed case${r.confirmed_fraud_cases === 1 ? "" : "s"}`;
    default:
      return "";
  }
}

/** Book-wide rings (algo.WCC) in review order. Loaded only when the tab is opened; queue order, never a score. */
function RingQueue({ version, activeRingId, onOpen }: { version: number; activeRingId?: string; onOpen: (claimId: string) => void }) {
  const [list, setList] = useState<RingList | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const loaded = useRef(-1); // the version the shown list belongs to
  const ticket = useRef(0);

  const load = async () => {
    const mine = ++ticket.current;
    loaded.current = version;
    setBusy(true);
    setErr(null);
    try {
      const l = await api.rings();
      if (mine === ticket.current) setList(l);
    } catch (e) {
      if (mine === ticket.current) setErr(e instanceof ApiError ? e.message : "Something went wrong.");
    } finally {
      if (mine === ticket.current) setBusy(false);
    }
  };

  useEffect(() => {
    if (loaded.current !== version) load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version]);

  return (
    <>
      <div className="ring-q-head">
        <span className="muted small">
          {list ? `${list.total_rings} ring${list.total_rings === 1 ? "" : "s"} · ${list.algorithm} · ${list.duration_ms} ms` : busy ? "Running algo.WCC…" : ""}
        </span>
        <button className="link" onClick={load} disabled={busy}>
          Refresh
        </button>
      </div>
      {err && <p className="note bad">{err}</p>}
      <ul className="queue" aria-label="Ring queue">
        {list?.rings.map((r) => {
          const inds = r.indicators ?? [];
          return (
            <li key={r.ring_id}>
              <button
                className={`q-item ${r.ring_id === activeRingId ? "on" : ""}`}
                onClick={() => onOpen(r.ring_id.replace(/^RING-/, ""))}
                aria-label={`${r.ring_id}, ${r.size} claims${inds.length ? `, ${inds.map((i) => RING_INDICATOR_LABEL[i] ?? i).join(", ")}` : ""}`}
              >
                <div className="q-top">
                  <strong>{r.ring_id}</strong>
                  <span className="muted small">{r.oversized ? `${r.size}+ claims` : `${r.size} claims`}</span>
                </div>
                <div className="q-sub">
                  <span className="muted small">
                    {r.filing_span_days}d span · {money(r.total_amount)}
                  </span>
                </div>
                {inds.length > 0 && (
                  <div className="q-tags">
                    {inds.map((i) => (
                      <span key={i} className="chip chip-warn small-chip" title={`${RING_INDICATOR_LABEL[i] ?? i}: ${traitDetail(i, r)}`}>
                        {RING_INDICATOR_SHORT[i] ?? i}
                      </span>
                    ))}
                  </div>
                )}
              </button>
            </li>
          );
        })}
        {list && list.rings.length === 0 && <li className="muted small pad">No group of 3 or more claims shares a strong identifier.</li>}
      </ul>
      {list && (
        <p className="muted tiny" title={list.ordering}>
          {list.truncated ? "The list may be incomplete (server reply limit). " : ""}Ordered by review indicators. Queue order only, not a score
          and not proof of wrongdoing.
        </p>
      )}
    </>
  );
}

export default function QueuePanel({ claims, selected, onSelect, onSubmitted, ringsVersion, activeRingId, onOpenRing, presets, intakeKey }: Props) {
  const [tab, setTab] = useState<"claims" | "rings">("claims");
  const [ringsOpened, setRingsOpened] = useState(false); // the ring list stays loaded once opened
  return (
    <div className="side">
      <IntakeCard key={intakeKey} presets={presets} onSubmitted={onSubmitted} onOpenRing={onOpenRing} />

      <section className="card grow">
        <header className="card-head">
          <h2>Queue</h2>
          <div className="tabs" role="tablist" aria-label="Queue view">
            <button role="tab" aria-selected={tab === "claims"} className={tab === "claims" ? "on" : ""} onClick={() => setTab("claims")}>
              Claims · {claims.length}
            </button>
            <button role="tab" aria-selected={tab === "rings"} className={tab === "rings" ? "on" : ""} onClick={() => (setTab("rings"), setRingsOpened(true))}>
              Rings
            </button>
          </div>
        </header>
        {ringsOpened && (
          <div className="ring-q" style={tab === "rings" ? undefined : { display: "none" }}>
            <RingQueue version={ringsVersion} activeRingId={activeRingId} onOpen={onOpenRing} />
          </div>
        )}
        {tab === "claims" && (
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
        )}
      </section>
    </div>
  );
}
