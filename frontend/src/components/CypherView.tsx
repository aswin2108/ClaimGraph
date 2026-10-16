import { useState } from "react";
import type { ExecutedQuery } from "../types";

interface Props {
  queries: ExecutedQuery[];
  /** What the queries produced, e.g. "this traversal" or "this ring". */
  what: string;
}

const MAX_LIST = 12;

/** Parameter values as sent, with long id lists shortened for display. */
function showParam(v: unknown): string {
  if (Array.isArray(v) && v.length > MAX_LIST) {
    return JSON.stringify(v.slice(0, MAX_LIST)).replace(/\]$/, `, … ${v.length - MAX_LIST} more]`);
  }
  return JSON.stringify(v);
}

/**
 * "Show Cypher": the exact queries the backend sent to FalkorDB for a result. The query text
 * is a fixed template in Go; every value (claim ids, limits, labels) is a bound parameter.
 */
export default function CypherView({ queries, what }: Props) {
  const [open, setOpen] = useState(false);
  if (!queries.length) return null;
  return (
    <div className="cypher-view">
      <button className="link" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        {open ? "Hide Cypher" : `Show Cypher (${queries.length} quer${queries.length === 1 ? "y" : "ies"})`}
      </button>
      {open && (
        <div className="cypher-list" aria-label={`Cypher behind ${what}`}>
          <p className="muted tiny">
            The queries sent to FalkorDB for {what}. Each is a fixed, parameterized template in Go: values such as claim ids
            are bound as parameters ($id, $claims, …), never pasted into the query, and the AI model never writes Cypher.
          </p>
          {queries.map((q, i) => (
            <div key={i} className="cypher-item">
              <div className="cypher-head">
                <strong>{q.purpose}</strong>
                <span className="muted tiny">
                  {q.cached
                    ? "answered from the cached whole-book run"
                    : `${q.results >= 0 ? `${q.results} result${q.results === 1 ? "" : "s"} · ` : ""}${q.duration_ms} ms`}
                  {q.timed_out ? " · stopped at the time limit" : ""}
                </span>
              </div>
              <pre className="cypher">{q.cypher}</pre>
              {q.params && Object.keys(q.params).length > 0 && (
                <ul className="cypher-params">
                  {Object.entries(q.params).map(([k, v]) => (
                    <li key={k}>
                      <code>${k}</code> = <code>{showParam(v)}</code>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
