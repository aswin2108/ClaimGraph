import { useEffect, useState } from "react";
import type { AgentResult, Closure } from "../types";
import { OUTCOME_DETAIL, OUTCOME_LABEL } from "../ui";
import { when } from "./DecisionBox";

interface Props {
  closure: Closure;
  loadSealed: () => Promise<AgentResult>;
}

/** A closed investigation: the final decision, and the sealed report exactly as it was read. */
export default function ClosedPanel({ closure, loadSealed }: Props) {
  const [open, setOpen] = useState(false);
  const [sealed, setSealed] = useState<AgentResult | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setOpen(false);
    setSealed(null);
    setErr(null);
  }, [closure.claim_id, closure.closed_at]);

  const toggle = async () => {
    const next = !open;
    setOpen(next);
    if (next && !sealed) {
      try {
        setSealed(await loadSealed());
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      }
    }
  };
  const ok = closure.outcome === "approved";
  const ex = sealed?.explanation;

  return (
    <section className="card" aria-label="Investigation closed">
      <header className="card-head">
        <h2>Investigation closed</h2>
        <span className={`chip ${ok ? "chip-ok" : "chip-bad"}`}>{OUTCOME_LABEL[closure.outcome] ?? closure.outcome}</span>
      </header>
      <p className="small">
        <strong>{OUTCOME_LABEL[closure.outcome] ?? closure.outcome} by investigator</strong>
        <span className="muted"> · {when(closure.closed_at)}</span>
      </p>
      <p className="muted small">{OUTCOME_DETAIL[closure.outcome]}</p>
      <div className="decision">
        <small className="muted">Reason</small>
        <p className="small">“{closure.reason}”</p>
      </div>
      <p className="muted small">
        Report at closing: score {closure.report_score} · {closure.report_band} ·{" "}
        {closure.report_source === "llm" ? "AI agent" : "deterministic explanation"}
      </p>
      <button className="btn btn-ghost wide" onClick={toggle} aria-expanded={open}>
        {open ? "Hide sealed report" : "View sealed report"}
      </button>
      {open && err && <p className="note bad">{err}</p>}
      {open && !sealed && !err && <p className="muted small">Loading the sealed report…</p>}
      {open && sealed && ex && (
        <div className="agent-out sealed" aria-label="Sealed report">
          <p className="summary">{ex.summary}</p>
          {ex.key_findings.length > 0 && (
            <ul className="plain">
              {ex.key_findings.map((f, i) => (
                <li key={i}>{f.statement}</li>
              ))}
            </ul>
          )}
          <div className="next">
            <small>Recommended next step at the time</small>
            <strong>{ex.next_step.action}</strong>
          </div>
          {sealed.tool_channel && (
            <p className="muted tiny">
              {sealed.model ? `${sealed.model} · ` : ""}
              {sealed.tool_channel.discovered.length} tools found on the {sealed.tool_channel.server} MCP server, {sealed.tool_channel.calls} calls
            </p>
          )}
          {sealed.tool_trace.length > 0 && (
            <ul className="trace">
              {sealed.tool_trace.map((t) => (
                <li key={t.seq} className={t.ok ? "" : "bad"}>
                  <b>{t.tool}</b> <code>{t.input}</code>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      <p className="muted tiny">
        A closed investigation is final: no new agent runs or decisions. The graph, ring and evidence stay viewable. Reset demo
        reopens everything.
      </p>
    </section>
  );
}
