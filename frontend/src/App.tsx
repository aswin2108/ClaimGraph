import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import type { AgentResult, ClaimDetail, ClaimSummary, Counterfactual, GraphView, Meta, Report } from "./types";
import { CATEGORY_LABEL, DISPOSITION_LABEL, money } from "./ui";
import QueuePanel from "./components/QueuePanel";
import GraphPanel from "./components/GraphPanel";
import EvidencePanel from "./components/EvidencePanel";
import PriorityPanel from "./components/PriorityPanel";
import CounterfactualPanel from "./components/CounterfactualPanel";
import NextStepPanel from "./components/NextStepPanel";
import DispositionPanel from "./components/DispositionPanel";

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

  const [dispBusy, setDispBusy] = useState(false);
  const [dispMsg, setDispMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const ticket = useRef(0); // ignore responses that belong to an older selection
  const selectedRef = useRef<string | null>(null);
  selectedRef.current = selected;

  const refreshQueue = useCallback(async () => {
    try {
      setQueue(await api.claims());
    } catch (e) {
      setError(msg(e));
    }
  }, []);

  useEffect(() => {
    api.meta().then(setMeta).catch((e) => setError(msg(e)));
    api
      .claims()
      .then((c) => {
        setQueue(c);
        if (c.length) setSelected(c[0].claim_id);
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

  // Selecting a claim starts a fresh investigation.
  useEffect(() => {
    if (!selected) return;
    setExcluded(null);
    setFocus(null);
    load(selected, hops, null, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected]);

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
        text: `${c.created ? "Recorded" : "Updated"}: ${DISPOSITION_LABEL[c.disposition]}. It will appear in future related investigations.`,
      });
      refreshQueue();
      return true;
    } catch (e) {
      setDispMsg({ ok: false, text: msg(e) });
      return false;
    } finally {
      setDispBusy(false);
    }
  };

  const shown = cf ? cf.counterfactual : report;
  const current = queue.find((c) => c.claim_id === selected)?.disposition;

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
              <span className={`chip ${detail.policy_check_status === "passed" ? "chip-ok" : ""}`}>Policy checks {detail.policy_check_status}</span>
            </div>
          )}
          <span className="chip">Synthetic data</span>
        </div>
      </header>

      {error && (
        <div className="banner" role="alert">
          {error}
        </div>
      )}

      <main className="layout">
        <QueuePanel
          claims={queue}
          selected={selected}
          onSelect={setSelected}
          onSubmitted={async (id) => {
            await refreshQueue();
            setSelected(id);
          }}
        />

        <div className="center">
          <GraphPanel
            graph={graph}
            hops={hops}
            onHops={changeHops}
            focusClaim={focus}
            onFocus={setFocus}
            excluded={excluded}
            busy={loading}
          />
          <EvidencePanel report={shown} focusClaim={focus} onFocus={setFocus} />
        </div>

        <div className="right">
          <PriorityPanel
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
          <NextStepPanel
            meta={meta}
            result={agent}
            busy={agentBusyFor !== null && agentBusyFor === selected}
            blocked={agentBusyFor !== null && agentBusyFor !== selected}
            error={agentErr}
            onRun={runAgent}
          />
          <DispositionPanel
            claimId={selected}
            dispositions={meta?.dispositions ?? []}
            current={current}
            busy={dispBusy}
            message={dispMsg}
            onRecord={record}
          />
        </div>
      </main>

      <footer className="foot">{meta?.disclaimer} All data is synthetic.</footer>
    </div>
  );
}
