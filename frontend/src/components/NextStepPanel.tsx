import { useState } from "react";
import type { AgentResult, Meta } from "../types";

interface Props {
  meta: Meta | null;
  result: AgentResult | null;
  busy: boolean;
  /** An agent run for a different claim is still in flight. */
  blocked?: boolean;
  error: string | null;
  onRun: () => void;
}

const REASONS: Record<string, string> = {
  llm_disabled: "The language model is turned off.",
  llm_error: "The language model could not be reached.",
  llm_timeout: "The language model took too long.",
  model_refused: "The language model declined to answer.",
  output_truncated: "The language model's answer was cut off.",
  invalid_or_ungrounded_output: "The model's answer cited something the graph did not return, so it was discarded.",
  tool_scope_violations: "The model tried something outside its allowed tools.",
  turn_limit_reached: "The model used its full turn budget.",
  run_token_limit_reached: "The run reached its token limit.",
  llm_budget_exhausted: "The hourly or daily model budget is used up.",
};

export default function NextStepPanel({ meta, result, busy, blocked, error, onRun }: Props) {
  const [showTrace, setShowTrace] = useState(false);
  const ex = result?.explanation;
  const b = meta?.agent_budget;

  return (
    <section className="card">
      <header className="card-head">
        <h2>Next verification step</h2>
        {result && (
          <span className={`chip ${result.source === "llm" ? "chip-ai" : ""}`}>
            {result.source === "llm" ? `AI · ${result.model}` : "Deterministic"}
          </span>
        )}
      </header>

      {!result && !busy && (
        <>
          <p className="muted small">
            The investigation agent reads the graph through six fixed tools, then explains the findings and proposes what to verify next.
            It cannot write queries or decide claims.
          </p>
          <button className="btn btn-primary wide" onClick={onRun} disabled={blocked}>
            Run investigation agent
          </button>
          {blocked && <p className="muted tiny">Another claim's agent run is still finishing.</p>}
          {meta && (
            <p className="muted tiny">
              {meta.llm_enabled
                ? `Uses the language model · ${b?.llm_runs_last_hour ?? 0}/${b?.max_llm_runs_per_hour ?? 0} runs this hour`
                : "Language model not configured: you will get the deterministic explanation."}
            </p>
          )}
        </>
      )}
      {busy && <div className="pulse-line">Agent is reading the graph… this can take up to a minute.</div>}
      {error && <p className="note bad">{error}</p>}

      {ex && !busy && (
        <div className="agent-out">
          <p className="summary">{ex.summary}</p>
          {ex.key_findings.length > 0 && (
            <ul className="plain">
              {ex.key_findings.map((f, i) => (
                <li key={i}>{f.statement}</li>
              ))}
            </ul>
          )}
          <div className="next">
            <small>Recommended next step</small>
            <strong>{ex.next_step.action}</strong>
            <p className="muted small">{ex.next_step.rationale}</p>
          </div>
          {ex.uncertainty.length > 0 && (
            <details>
              <summary>Uncertainty</summary>
              <ul className="plain">
                {ex.uncertainty.map((u, i) => (
                  <li key={i}>{u}</li>
                ))}
              </ul>
            </details>
          )}
          {result?.source !== "llm" && result?.fallback_reason && (
            <p className="muted tiny">{REASONS[result.fallback_reason] ?? result.fallback_reason} Showing the deterministic explanation built directly from graph evidence.</p>
          )}
          {result && (
            <>
              <button className="link" onClick={() => setShowTrace((v) => !v)}>
                {showTrace ? "Hide" : "Show"} tool trace ({result.usage.tool_calls} calls, {result.usage.turns} turns)
              </button>
              {showTrace && (
                <ul className="trace">
                  {result.tool_trace.map((t) => (
                    <li key={t.seq} className={t.ok ? "" : "bad"}>
                      <b>{t.tool}</b> <code>{t.input}</code> <span className="muted tiny">{t.duration_ms}ms</span>
                    </li>
                  ))}
                  {result.tool_trace.length === 0 && <li className="muted small">No tools were called.</li>}
                </ul>
              )}
            </>
          )}
          <button className="btn btn-ghost wide" onClick={onRun}>
            Run again
          </button>
        </div>
      )}
    </section>
  );
}
