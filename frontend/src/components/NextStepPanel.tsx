import { useState, type ReactNode } from "react";
import type { ActionPlan, AgentResult, Meta } from "../types";
import Hint from "./Hint";

interface Props {
  meta: Meta | null;
  result: AgentResult | null;
  busy: boolean;
  /** An agent run for a different claim is still in flight. */
  blocked?: boolean;
  error: string | null;
  onRun: () => void;
  /** Deterministic verification plan derived from the graph (no model involved). */
  plan?: ActionPlan | null;
  /** The investigator's decision box, shown under the report. */
  children?: ReactNode;
}

function Actions({ plan }: { plan: ActionPlan }) {
  return (
    <div className="actions-block">
      <div className="actions-head">
        <h3>Recommended verifications</h3>
        <span className="chip small-chip" title="Derived deterministically from the graph evidence; the language model is not involved">
          From the graph
        </span>
      </div>
      <ol className="actions" aria-label="Recommended verifications">
        {plan.actions.map((a, i) => (
          <li key={i}>
            <details>
              <summary>
                <span className="a-title">{a.title}</span>
                <span className="a-badges">
                  {a.simulated && <span className="chip chip-warn small-chip">Simulated</span>}
                  {a.requires_human_approval && <span className="chip small-chip">Needs human approval</span>}
                </span>
              </summary>
              <p className="muted small">{a.rationale}</p>
              {a.evidence_path && (
                <p className="a-path">
                  <small>Evidence path</small>
                  <code>{a.evidence_path}</code>
                </p>
              )}
              {!!a.claims?.length && <p className="muted tiny">Claims: {a.claims.join(", ")}</p>}
              {!!a.entities?.length && <p className="muted tiny">Identifiers: {a.entities.join(", ")}</p>}
            </details>
          </li>
        ))}
      </ol>
      <p className="muted tiny">{plan.note}</p>
    </div>
  );
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
  tool_channel_error: "The connection to the MCP tool server failed.",
};

const MCP_HINT =
  "MCP (Model Context Protocol) is the standard way AI agents find and call tools. This agent does not have its tools built in: each run connects to the ClaimGraph MCP server, asks it which tools exist (tools/list) and sends every call through it (tools/call). The server checks each call (allowed tools, scope, limits) before it touches FalkorDB. Claude Code, Claude Desktop or Cursor can connect to the same tools with cmd/mcp.";

export default function NextStepPanel({ meta, result, busy, blocked, error, onRun, plan, children }: Props) {
  const [showTrace, setShowTrace] = useState(false);
  const ex = result?.explanation;
  const b = meta?.agent_budget;

  return (
    <section className="card">
      <header className="card-head">
        <h2>Next verification step</h2>
        {result && (
          <span className="head-chips">
            {result.tool_channel && (
              <span className="chip chip-ai" data-testid="mcp-badge" title="The agent found and called its tools through the ClaimGraph MCP server">
                MCP
              </span>
            )}
            <span className={`chip ${result.source === "llm" ? "chip-ai" : ""}`}>
              {result.source === "llm" ? `AI · ${result.model}` : "Deterministic"}
            </span>
          </span>
        )}
      </header>

      {plan && plan.actions.length > 0 && <Actions plan={plan} />}

      {!result && !busy && (
        <>
          <p className="muted small">
            The investigation agent reads the graph through eight fixed tools, then explains the findings and proposes what to verify next.
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
      {busy && (
        <div className="pulse-line">
          {meta?.llm_enabled
            ? "Agent is reading the graph through its MCP tools… this can take up to a minute."
            : "Agent is reading the graph… this can take up to a minute."}
        </div>
      )}
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
          {result?.tool_channel && (
            <p className="small mcp-line" data-testid="mcp-line">
              <span className="chip chip-ai small-chip">MCP</span> The agent found {result.tool_channel.discovered.length} tools on the{" "}
              <code>{result.tool_channel.server}</code> MCP server (tools/list) and sent {result.tool_channel.calls}{" "}
              {result.tool_channel.calls === 1 ? "call" : "calls"} through it (tools/call). <Hint text={MCP_HINT} />
            </p>
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
      {children}
    </section>
  );
}
