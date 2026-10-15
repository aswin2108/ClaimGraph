import type { Counterfactual, Report } from "../types";
import { CATEGORY_COLOR, CATEGORY_LABEL, FACTOR_LABEL } from "../ui";

interface Props {
  categories: string[];
  report: Report | null;
  excluded: string | null;
  result: Counterfactual | null;
  busy: boolean;
  onToggle: (category: string) => void;
}

// Which score factor each excludable category produces (to show which signals exist).
const FACTOR_OF: Record<string, string> = {
  payout: "shared_claimant_payout",
  device: "shared_non_public_device",
  contact: "shared_contact",
  address: "shared_address",
  repair_shop: "shared_repair_shop",
  public_ip: "shared_public_ip",
};

export default function CounterfactualPanel({ categories, report, excluded, result, busy, onToggle }: Props) {
  const present = new Set(report?.factors.map((f) => f.name));
  return (
    <section className="card">
      <header className="card-head">
        <h2>Challenge the finding</h2>
        {excluded && (
          <button className="link" onClick={() => onToggle(excluded)}>
            Reset
          </button>
        )}
      </header>
      <p className="muted small">What if we ignore one kind of evidence? The graph is never modified; the score is recomputed read-only.</p>

      <div className="toggles">
        {categories.map((c) => {
          const has = present.has(FACTOR_OF[c]);
          const on = excluded === c;
          return (
            <button
              key={c}
              className={`toggle ${on ? "on" : ""}`}
              disabled={busy || (!has && !on)}
              onClick={() => onToggle(c)}
              title={has ? `Ignore ${CATEGORY_LABEL[c]}` : "No evidence of this kind for the claim"}
              aria-pressed={on}
            >
              <i style={{ background: CATEGORY_COLOR[c] }} />
              {CATEGORY_LABEL[c] ?? c}
            </button>
          );
        })}
      </div>

      {result && (
        <div className="cf-result">
          <div className="cf-line">
            <span>
              {result.original.score} <em>{result.original.band}</em>
            </span>
            <span className="arrow">→</span>
            <span>
              <strong>{result.counterfactual.score}</strong> <em>{result.counterfactual.band}</em>
            </span>
          </div>
          <ul className="removed">
            {result.removed_factors.map((f) => (
              <li key={f.name}>
                <span>{FACTOR_LABEL[f.name] ?? f.name}</span>
                <span className="neg">
                  {f.original_points} → {f.counterfactual_points}
                </span>
              </li>
            ))}
          </ul>
          <p className="small">{result.explanation}</p>
          <p className={`verdict ${result.still_justifies_investigation ? "yes" : "no"}`}>
            {result.still_justifies_investigation
              ? "The remaining evidence still justifies a human investigation."
              : "Without this evidence, the claim no longer stands out for prioritisation."}
          </p>
        </div>
      )}
    </section>
  );
}
