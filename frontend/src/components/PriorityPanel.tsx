import type { Report } from "../types";
import { BAND_COLOR, FACTOR_LABEL } from "../ui";

interface Props {
  report: Report | null;
  /** Score before a counterfactual exclusion, when one is active. */
  originalScore?: number;
  excludedLabel?: string;
  loading: boolean;
}

const ARC = Math.PI * 70;

export default function PriorityPanel({ report, originalScore, excludedLabel, loading }: Props) {
  const score = report?.score ?? 0;
  const color = BAND_COLOR[report?.band ?? "Low"] ?? "#64748b";
  const delta = originalScore !== undefined ? score - originalScore : 0;
  const maxPts = Math.max(25, ...(report?.factors.map((f) => f.points) ?? [0]));

  return (
    <section className="card">
      <header className="card-head">
        <h2>Investigation priority</h2>
        {report && (
          <span className="band" style={{ background: `${color}1a`, color }}>
            {report.band}
          </span>
        )}
      </header>

      <div className={`gauge ${loading ? "faded" : ""}`}>
        <svg viewBox="0 0 160 96" role="img" aria-label={`Priority score ${score} out of 100`}>
          <path d="M 10 82 A 70 70 0 0 1 150 82" className="g-track" />
          <path
            d="M 10 82 A 70 70 0 0 1 150 82"
            className="g-fill"
            style={{ stroke: color, strokeDasharray: ARC, strokeDashoffset: ARC * (1 - score / 100) }}
          />
          {[30, 60].map((t) => {
            const a = Math.PI * (1 - t / 100);
            const x1 = 80 + 62 * Math.cos(a);
            const y1 = 82 - 62 * Math.sin(a);
            const x2 = 80 + 78 * Math.cos(a);
            const y2 = 82 - 78 * Math.sin(a);
            return <line key={t} x1={x1} y1={y1} x2={x2} y2={y2} className="g-tick" />;
          })}
          <text x="80" y="70" textAnchor="middle" className="g-score">
            {score}
          </text>
          <text x="80" y="88" textAnchor="middle" className="g-sub">
            of 100
          </text>
        </svg>
        {originalScore !== undefined && (
          <div className="cf-delta">
            Ignoring {excludedLabel}: <s>{originalScore}</s> → <strong>{score}</strong>{" "}
            <span className={delta < 0 ? "neg" : ""}>({delta > 0 ? "+" : ""}{delta})</span>
          </div>
        )}
      </div>

      {report && <p className="muted small center">{report.band_meaning}</p>}

      <ul className="factors">
        {report?.factors.map((f) => (
          <li key={f.name} title={f.reason}>
            <div className="f-row">
              <span>{FACTOR_LABEL[f.name] ?? f.name}</span>
              <strong>+{f.points}</strong>
            </div>
            <div className="f-bar">
              <i style={{ width: `${(f.points / maxPts) * 100}%`, background: color }} />
            </div>
          </li>
        ))}
        {report && report.factors.length === 0 && <li className="muted small">No scoring signals found for this claim.</li>}
      </ul>

      <p className="disclaimer">{report?.disclaimer ?? "This is an investigation-priority score, not a fraud determination."}</p>
    </section>
  );
}
