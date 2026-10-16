import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import type { AgentResult, ClaimDetail, ClaimSummary, Counterfactual, GraphStats, GraphView, Meta, Report, Ring } from "./types";
import { presetsFor } from "./presets";
import { CATEGORY_LABEL, DISPOSITION_LABEL, money, OUTCOME_LABEL } from "./ui";
import QueuePanel from "./components/QueuePanel";
import GraphPanel from "./components/GraphPanel";
import EvidencePanel from "./components/EvidencePanel";
import PriorityPanel from "./components/PriorityPanel";
import CounterfactualPanel from "./components/CounterfactualPanel";
import NextStepPanel from "./components/NextStepPanel";
import DecisionBox from "./components/DecisionBox";
import ClosedPanel from "./components/ClosedPanel";
import RingPanel from "./components/RingPanel";
import type { GraphViewMode } from "./components/ViewTabs";

const msg = (e: unknown) => (e instanceof ApiError ? e.message : "Something went wrong.");

export default function App() {
  const [meta, setMeta] = useState<Meta | null>(null);
  const [queue, setQueue] = useState<ClaimSummary[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [hops, setHops] = useState(3);

  const [detail, setDetail] = useState<ClaimDetail | null>(null);
  const [report, setReport] = useState<Report | null>(null);
  const [graph, setGraph] = useState<GraphView | null>(null);
  const [excluded, setExcluded] = useState<string | null>(null);
  const [cf, setCf] = useState<Counterfactual | null>(null);
  const [focus, setFocus] = useState<string | null>(null);

  const [loading, setLoading] = useState(false);
  const [cfBusy, setCfBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [agent, setAgent] = useState<AgentResult | null>(null);
  const [agentBusyFor, setAgentBusyFor] = useState<string | null>(null); // claim id of the in-flight agent run
  const [agentErr, setAgentErr] = useState<string | null>(null);

  const [view, setView] = useState<GraphViewMode>("traversal");
  const [ring, setRing] = useState<Ring | null>(null);
  const [ringErr, setRingErr] = useState<string | null>(null);
  const [ringBusy, setRingBusy] = useState(false);
  const [ringsVersion, setRingsVersion] = useState(0); // new claims or outcomes can change the book-wide ring list
  const [graphSize, setGraphSize] = useState<GraphStats | null>(null);
  const [resetArmed, setResetArmed] = useState(false);
  const [resetBusy, setResetBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [intakeKey, setIntakeKey] = useState(0);

  const [dispBusy, setDispBusy] = useState(false);
  const [dispMsg, setDispMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const ticket = useRef(0); // ignore responses that belong to an older selection
  const ringTicket = useRef(0);
  const selectedRef = useRef<string | null>(null);
  selectedRef.current = selected;

  // The queue shows one page of claims (newest first), optionally filtered by claim id; "Load more" appends.
  const [queueTotal, setQueueTotal] = useState(0);
  const [queueQ, setQueueQ] = useState("");
  const [moreBusy, setMoreBusy] = useState(false);
  const queueQRef = useRef("");
  queueQRef.current = queueQ;
  const queueTicket = useRef(0);
  const refreshQueue = useCallback(async (q: string = queueQRef.current) => {
    const mine = ++queueTicket.current;
    try {
      const p = await api.claims({ q });
      if (mine === queueTicket.current) {
        setQueue(p.claims);
        setQueueTotal(p.total);
      }
      return p.claims;
    } catch (e) {
      setError(msg(e));
      return [];
    }
  }, []);
  const loadMore = async () => {
    const mine = queueTicket.current;
    setMoreBusy(true);
    try {
      const p = await api.claims({ q: queueQ, offset: queue.length });
      if (mine === queueTicket.current) {
        setQueue((cur) => [...cur, ...p.claims.filter((c) => !cur.some((x) => x.claim_id === c.claim_id))]);
        setQueueTotal(p.total);
      }
    } catch (e) {
      setError(msg(e));
    } finally {
      setMoreBusy(false);
    }
  };
  // Typing in the search box refreshes the list shortly after the last keystroke.
  const searchStarted = useRef(false);
  useEffect(() => {
    if (!searchStarted.current) {
      searchStarted.current = true;
      return;
    }
    const t = window.setTimeout(() => refreshQueue(queueQ), 250);
    return () => window.clearTimeout(t);
  }, [queueQ, refreshQueue]);

  const refreshGraphSize = useCallback(() => {
    api.graphStats().then(setGraphSize).catch(() => setGraphSize(null)); // context only; never blocks the app
  }, []);

  useEffect(() => {
    api.meta().then(setMeta).catch((e) => setError(msg(e)));
    refreshGraphSize();
    api
      .claims()
      .then((p) => {
        setQueue(p.claims);
        setQueueTotal(p.total);
        if (p.claims.length) setSelected(p.claims[0].claim_id);
      })
      .catch((e) => setError(msg(e)));
  }, []);

  const load = useCallback(async (id: string, h: number, exclude: string | null, resetAgent: boolean) => {
    const mine = ++ticket.current;
    setLoading(true);
    setError(null);
    if (resetAgent) {
      setAgent(null);
      setAgentErr(null);
      setDispMsg(null);
    }
    try {
      const [d, r, g, c] = await Promise.all([
        api.claim(id),
        api.investigation(id, h),
        api.graph(id, h, exclude ?? undefined),
        exclude ? api.counterfactual(id, exclude, h) : Promise.resolve(null),
      ]);
      if (mine !== ticket.current) return;
      setDetail(d);
      setReport(r);
      setGraph(g);
      setCf(c);
    } catch (e) {
      if (mine === ticket.current) setError(msg(e));
    } finally {
      if (mine === ticket.current) setLoading(false);
    }
  }, []);

  // The ring (algo.WCC + algo.MSF) depends only on the claim and the recorded evidence, not on hops or exclusions.
  // It is fetched on its own so a failing algorithm never blocks the investigation.
  const loadRing = useCallback(async (id: string, clear: boolean) => {
    const mine = ++ringTicket.current;
    if (clear) setRing(null);
    setRingErr(null);
    setRingBusy(true);
    try {
      const r = await api.ring(id);
      if (mine === ringTicket.current) setRing(r);
    } catch (e) {
      if (mine === ringTicket.current) {
        setRing(null);
        setRingErr(msg(e));
      }
    } finally {
      if (mine === ringTicket.current) setRingBusy(false);
    }
  }, []);

  // Selecting a claim starts a fresh investigation.
  useEffect(() => {
    if (!selected) return;
    investigate(selected);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected]);

  // Start a fresh investigation of a claim (also used when the selection does not change, e.g. after a reset).
  const investigate = (id: string) => {
    setExcluded(null);
    setFocus(null);
    load(id, hops, null, true);
    loadRing(id, true);
  };

  // "Reset demo": restore the graph's pristine snapshot. Two clicks, because it discards every claim
  // submitted and every outcome recorded since the demo was loaded (for every visitor of this server).
  const resetDemo = async () => {
    if (!resetArmed) {
      setResetArmed(true);
      window.setTimeout(() => setResetArmed(false), 5000);
      return;
    }
    setResetArmed(false);
    setResetBusy(true);
    setError(null);
    setNotice(null);
    try {
      const r = await api.resetDemo();
      setNotice(`Demo reset: the graph is back to its starting state (${r.claims.toLocaleString("en-US")} claims, restored in ${r.duration_ms} ms).`);
      setView("traversal");
      setRingsVersion((v) => v + 1);
      setIntakeKey((k) => k + 1);
      refreshGraphSize();
      api.meta().then(setMeta).catch(() => undefined);
      setQueueQ("");
      const c = await refreshQueue("");
      const first = c[0]?.claim_id ?? null;
      if (first && first === selectedRef.current) investigate(first);
      else setSelected(first);
    } catch (e) {
      setError(msg(e));
    } finally {
      setResetBusy(false);
    }
  };

  const changeHops = (h: number) => {
    setHops(h);
    setFocus(null);
    if (selected) load(selected, h, excluded, false);
  };

  const toggle = async (category: string) => {
    if (!selected) return;
    const next = excluded === category ? null : category;
    setExcluded(next);
    setFocus(null);
    setCfBusy(true);
    await load(selected, hops, next, false);
    setCfBusy(false);
  };

  const agentLock = useRef(false); // synchronous guard: at most one agent (LLM) run at a time, however fast the clicks
  const runAgent = async () => {
    if (!selected || agentLock.current) return;
    agentLock.current = true;
    const id = selected;
    setAgentBusyFor(id);
    setAgentErr(null);
    try {
      const r = await api.agent(id, hops);
      if (id === selectedRef.current) setAgent(r); // the investigator may have moved on while the agent ran
      api.meta().then(setMeta).catch(() => undefined);
    } catch (e) {
      setAgentErr(msg(e));
    } finally {
      agentLock.current = false;
      setAgentBusyFor(null);
    }
  };

  const record = async (disposition: string, notes: string): Promise<boolean> => {
    if (!selected) return false;
    setDispBusy(true);
    setDispMsg(null);
    try {
      const c = await api.disposition(selected, disposition, notes);
      setDispMsg({
        ok: true,
        text:
          c.disposition === "needs_info"
            ? `Recorded: ${DISPOSITION_LABEL[c.disposition]}. The investigation stays open.`
            : `${c.created ? "Recorded" : "Updated"}: ${DISPOSITION_LABEL[c.disposition]}. It will appear in future related investigations.`,
      });
      refreshQueue();
      api.claim(selected).then(setDetail).catch(() => undefined); // the selected claim may not be on the loaded page
      loadRing(selected, false); // the ring's recorded cases changed
      setRingsVersion((v) => v + 1);
      return true;
    } catch (e) {
      setDispMsg({ ok: false, text: msg(e) });
      return false;
    } finally {
      setDispBusy(false);
    }
  };

  const closeInvestigation = async (outcome: "approved" | "rejected", reason: string): Promise<boolean> => {
    const id = selected;
    const runId = agent?.report.target_claim === id ? agent?.run_id : undefined;
    if (!id || !runId) return false;
    setDispBusy(true);
    setDispMsg(null);
    try {
      const c = await api.close(id, runId, outcome, reason);
      refreshQueue();
      api.claim(id).then(setDetail).catch(() => undefined); // brings the closure: the panel switches to "Investigation closed"
      loadRing(id, false); // the ring's recorded cases changed
      setRingsVersion((v) => v + 1);
      setDispMsg({ ok: true, text: `${OUTCOME_LABEL[c.outcome]}: investigation closed.` });
      return true;
    } catch (e) {
      setDispMsg({ ok: false, text: msg(e) });
      return false;
    } finally {
      setDispBusy(false);
    }
  };

  const shown = cf ? cf.counterfactual : report;
  const closure = detail?.claim_id === selected ? detail?.closure : undefined;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <svg viewBox="0 0 32 32" width="30" height="30" aria-hidden>
            <rect width="32" height="32" rx="8" fill="#4f46e5" />
            <path d="M9 16L23 9M9 16L23 23" stroke="#fff" strokeWidth="1.8" />
            <circle cx="9" cy="16" r="3.2" fill="#fff" />
            <circle cx="23" cy="9" r="3.2" fill="#fff" />
            <circle cx="23" cy="23" r="3.2" fill="#fff" />
          </svg>
          <div>
            <h1>ClaimGraph</h1>
            <p>Every claim looks innocent on its own. The fraud is in the connections.</p>
          </div>
        </div>
        <div className="top-right">
          {detail && (
            <div className="claim-strip">
              <strong>{detail.claim_id}</strong>
              <span>{money(detail.amount)}</span>
              <span>{detail.loss_type ?? "collision"}</span>
              <span className={`chip ${detail.policy_check_status === "passed" ? "chip-ok" : detail.policy_check_status === "failed" ? "chip-bad" : ""}`}>
                Policy checks {detail.policy_check_status}
              </span>
              {detail.closure && (
                <span className={`chip ${detail.closure.outcome === "approved" ? "chip-ok" : "chip-bad"}`}>
                  {OUTCOME_LABEL[detail.closure.outcome] ?? detail.closure.outcome} · closed
                </span>
              )}
            </div>
          )}
          {meta?.demo_reset_available && (
            <button
              className={`btn btn-small ${resetArmed ? "btn-danger" : "btn-ghost"}`}
              onClick={resetDemo}
              disabled={resetBusy}
              title="Restores the graph to its starting state: removes every claim submitted and every outcome recorded since the demo was loaded."
            >
              {resetBusy ? "Resetting…" : resetArmed ? "Click again to reset" : "Reset demo"}
            </button>
          )}
          <span className="chip">Synthetic data</span>
        </div>
      </header>

      {error && (
        <div className="banner" role="alert">
          {error}
        </div>
      )}
      {notice && (
        <div className="banner ok" role="status">
          {notice}
        </div>
      )}

      <main className="layout">
        <QueuePanel
          claims={queue}
          total={queueTotal}
          search={queueQ}
          onSearch={setQueueQ}
          onLoadMore={loadMore}
          loadingMore={moreBusy}
          selected={selected}
          onSelect={setSelected}
          ringsVersion={ringsVersion}
          activeRingId={ring?.ring_id}
          onOpenRing={(id) => {
            setSelected(id);
            setView("ring");
          }}
          presets={presetsFor(meta?.graph)}
          intakeKey={intakeKey}
          onSubmitted={async (id) => {
            setRingsVersion((v) => v + 1);
            refreshGraphSize();
            setNotice(null);
            await refreshQueue();
            setSelected(id);
          }}
        />

        <div className="center">
          {view === "ring" ? (
            <RingPanel ring={ring} error={ringErr} busy={ringBusy} traversal={report?.investigation ?? null} hops={hops} onView={setView} />
          ) : (
            <GraphPanel
              graph={graph}
              hops={hops}
              onHops={changeHops}
              focusClaim={focus}
              onFocus={setFocus}
              excluded={excluded}
              busy={loading}
              ringSize={ring?.size}
              onView={setView}
            />
          )}
          <EvidencePanel report={shown} focusClaim={focus} onFocus={setFocus} graphSize={graphSize} />
        </div>

        <div className="right">
          <PriorityPanel
            failedChecks={detail?.claim_id === selected ? detail?.policy_checks?.filter((c) => !c.passed).map((c) => c.name) : undefined}
            report={shown}
            originalScore={cf ? cf.original.score : undefined}
            excludedLabel={excluded ? (CATEGORY_LABEL[excluded] ?? excluded) : undefined}
            loading={loading}
          />
          <CounterfactualPanel
            categories={meta?.counterfactual_categories ?? []}
            report={report}
            excluded={excluded}
            result={cf}
            busy={cfBusy || loading}
            onToggle={toggle}
          />
          {closure ? (
            <ClosedPanel closure={closure} loadSealed={() => api.sealedReport(closure.claim_id)} />
          ) : (
            <NextStepPanel
              meta={meta}
              result={agent}
              busy={agentBusyFor !== null && agentBusyFor === selected}
              blocked={agentBusyFor !== null && agentBusyFor !== selected}
              error={agentErr}
              onRun={runAgent}
              plan={report?.recommended_actions}
            >
              <DecisionBox
                claimId={selected}
                hasReport={!!agent && agentBusyFor !== selected}
                canClose={!!agent?.run_id}
                decision={detail?.claim_id === selected ? detail?.recorded_decision : null}
                busy={dispBusy}
                message={dispMsg}
                onClose={closeInvestigation}
                onNeedInfo={(notes) => record("needs_info", notes)}
              />
            </NextStepPanel>
          )}
        </div>
      </main>

      <footer className="foot">{meta?.disclaimer} All data is synthetic.</footer>
    </div>
  );
}
