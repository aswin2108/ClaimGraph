import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { api, ApiError } from "../api";
import type { FNOL, FNOLResult } from "../types";
import { money, POLICY_CHECK_LABEL as CHECK_LABEL } from "../ui";
import { newClaimId, type Preset } from "../presets";

interface Props {
  presets: Preset[];
  onSubmitted: (id: string) => void;
  onOpenRing: (claimId: string) => void;
}

const OPTIONAL: (keyof FNOL)[] = ["incident_location", "loss_type", "tow_provider_id", "contact_type", "contact_value", "address_id", "device_id", "ip_address"];

/** Drops empty optional fields so the API never receives "" for them. */
function clean(f: FNOL): FNOL {
  const out = { ...f };
  for (const k of OPTIONAL) if (out[k] === "") delete out[k];
  if (!out.device_id) delete out.device_is_public;
  if (!out.contact_value) delete out.contact_type;
  return out;
}

function Field({ label, children, wide }: { label: string; children: React.ReactNode; wide?: boolean }) {
  return (
    <label className={`ff ${wide ? "wide" : ""}`}>
      <span>{label}</span>
      {children}
    </label>
  );
}

/** Every FNOL field, editable. Changes stay in the draft until the claim is submitted. */
function EditDialog({ draft, setDraft, onClose, onRestore }: { draft: FNOL; setDraft: (f: FNOL) => void; onClose: () => void; onRestore: () => void }) {
  const first = useRef<HTMLInputElement>(null);
  const close = useRef(onClose);
  close.current = onClose;
  // Once, when the dialog opens: focus the first field and close on Escape. (Depending on onClose would
  // re-run this on every keystroke, because the parent re-renders, and pull the focus back to the first field.)
  useEffect(() => {
    first.current?.focus();
    const esc = (e: KeyboardEvent) => e.key === "Escape" && close.current();
    window.addEventListener("keydown", esc);
    return () => window.removeEventListener("keydown", esc);
  }, []);
  const text = (k: keyof FNOL, ref?: React.Ref<HTMLInputElement>) => (
    <input ref={ref} value={(draft[k] as string | undefined) ?? ""} onChange={(e) => setDraft({ ...draft, [k]: e.target.value })} />
  );
  const date = (k: keyof FNOL) => <input type="date" value={(draft[k] as string) ?? ""} onChange={(e) => setDraft({ ...draft, [k]: e.target.value })} />;
  const select = (k: keyof FNOL, options: string[]) => (
    <select value={(draft[k] as string | undefined) ?? ""} onChange={(e) => setDraft({ ...draft, [k]: e.target.value })}>
      {options.map((o) => (
        <option key={o} value={o}>
          {o || "—"}
        </option>
      ))}
    </select>
  );
  // Rendered into <body>: the side column is position: sticky, which would trap the overlay under the graph canvas.
  return createPortal(
    <div className="modal-back" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal" role="dialog" aria-modal="true" aria-label="Edit claim fields">
        <header className="card-head">
          <h2>Claim fields (synthetic)</h2>
          <button className="link" onClick={onRestore}>
            Restore scenario values
          </button>
        </header>
        <div className="form-grid">
          <h3>Claim</h3>
          <Field label="Claim id">{text("claim_id", first)}</Field>
          <Field label="Amount (USD)">
            <input type="number" min={1} value={draft.amount} onChange={(e) => setDraft({ ...draft, amount: Number(e.target.value) })} />
          </Field>
          <Field label="Incident date">{date("incident_date")}</Field>
          <Field label="Filing date">{date("filing_date")}</Field>
          <Field label="Loss type">{select("loss_type", ["", "collision", "theft", "glass", "vandalism", "weather"])}</Field>
          <Field label="Incident location">{text("incident_location")}</Field>

          <h3>Claimant and policy</h3>
          <Field label="Claimant id">{text("claimant_id")}</Field>
          <Field label="Claimant name">{text("claimant_name")}</Field>
          <Field label="Policy id">{text("policy_id")}</Field>
          <Field label="Policy status">{select("policy_status", ["active", "lapsed", "cancelled"])}</Field>
          <Field label="Policy start">{date("policy_start")}</Field>
          <Field label="Policy end">{date("policy_end")}</Field>
          <Field label="Vehicle VIN (17 letters/digits)" wide>
            {text("vin")}
          </Field>

          <h3>Payment and repair</h3>
          <Field label="Payout account">{text("payout_account_id")}</Field>
          <Field label="Payout type">{select("payout_type", ["claimant", "vendor_direct_settlement"])}</Field>
          <Field label="Repair shop">{text("repair_shop_id")}</Field>
          <Field label="Tow provider (optional)">{text("tow_provider_id")}</Field>

          <h3>Contact and digital (optional)</h3>
          <Field label="Contact type">{select("contact_type", ["", "phone", "email"])}</Field>
          <Field label="Phone / email">{text("contact_value")}</Field>
          <Field label="Address id">{text("address_id")}</Field>
          <Field label="IP address">{text("ip_address")}</Field>
          <Field label="Device id">{text("device_id")}</Field>
          <label className="ff check">
            <input type="checkbox" checked={!!draft.device_is_public} onChange={(e) => setDraft({ ...draft, device_is_public: e.target.checked })} />
            <span>Public device (kiosk)</span>
          </label>
        </div>
        <footer className="modal-foot">
          <span className="muted tiny">Synthetic data only. Never enter real personal or banking details.</span>
          <button className="btn btn-primary" onClick={onClose}>
            Done
          </button>
        </footer>
      </div>
    </div>,
    document.body,
  );
}

export default function IntakeCard({ presets, onSubmitted, onOpenRing }: Props) {
  const [presetKey, setPresetKey] = useState(presets[0].key);
  const preset = presets.find((p) => p.key === presetKey) ?? presets[0];
  const [draft, setDraft] = useState<FNOL>(() => preset.build(newClaimId()));
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<FNOLResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const choose = (p: Preset) => {
    setPresetKey(p.key);
    setDraft(p.build(newClaimId()));
    setError(null);
  };

  // The preset set changes once the API says which graph it serves (meta arrives after the first render).
  const shownSet = useRef(presets);
  useEffect(() => {
    if (shownSet.current !== presets) {
      shownSet.current = presets;
      choose(presets[0]);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [presets]);

  const submit = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const res = await api.submitFNOL(clean(draft));
      setResult(res);
      if (!preset.fixedId) setDraft(preset.build(newClaimId())); // the next submission is a new claim
      onSubmitted(res.claim_id);
    } catch (e) {
      const err = e as ApiError;
      setError(err.problems?.length ? err.problems.join("; ") : err.message);
    } finally {
      setBusy(false);
    }
  };

  const alert = result?.ring_alert;
  const rows: [string, string][] = [
    ["Claim", `${draft.claim_id} · ${draft.loss_type ?? "claim"} · ${money(draft.amount || 0)}`],
    ["Incident", `${draft.incident_date}${draft.incident_location ? ` · ${draft.incident_location}` : ""}`],
    ["Claimant", `${draft.claimant_name} (${draft.claimant_id})`],
    ["Policy", `${draft.policy_id} · ${draft.policy_status} · ${draft.policy_start} → ${draft.policy_end}`],
    ["Payout", `${draft.payout_account_id}${draft.payout_type === "vendor_direct_settlement" ? " (vendor settlement)" : ""}`],
    ["Repair shop", draft.repair_shop_id],
  ];
  if (draft.contact_value) rows.push([draft.contact_type === "email" ? "Email" : "Phone", draft.contact_value]);
  if (draft.address_id) rows.push(["Address", draft.address_id]);
  if (draft.device_id) rows.push(["Device", `${draft.device_id}${draft.device_is_public ? " (public)" : ""}`]);
  if (draft.ip_address) rows.push(["IP", draft.ip_address]);

  return (
    <section className="card intake">
      <header className="card-head">
        <h2>New claim (FNOL)</h2>
        <button className="link" onClick={() => setEditing(true)}>
          Edit fields
        </button>
      </header>
      <div className="presets" role="group" aria-label="Scenario">
        {presets.map((p) => (
          <button key={p.key} className={`preset ${p.key === preset.key ? "on" : ""}`} aria-pressed={p.key === preset.key} onClick={() => choose(p)}>
            {p.title}
          </button>
        ))}
      </div>
      <p className="muted small">{preset.blurb}</p>
      <dl className="fnol-sum" aria-label="Claim to submit">
        {rows.map(([k, v]) => (
          <div key={k}>
            <dt>{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
      <button className="btn btn-primary wide" onClick={submit} disabled={busy}>
        {busy ? "Submitting…" : "Submit synthetic FNOL"}
      </button>

      {error && <p className="note bad">{error}</p>}
      {result && (
        <div className="intake-result" role="status">
          <p className="note ok">
            {result.created ? "Received" : "Updated"} {result.claim_id}.{" "}
            {result.policy_check_status === "passed" ? "Ordinary policy checks passed." : "Ordinary policy checks flagged a problem."}
          </p>
          <ul className="checks" aria-label="Policy checks">
            {result.policy_checks.map((c) => (
              <li key={c.name} className={c.passed ? "ok" : "bad"}>
                <span aria-hidden>{c.passed ? "✓" : "✗"}</span> {CHECK_LABEL[c.name] ?? c.name.replaceAll("_", " ")}
              </li>
            ))}
          </ul>
          {!result.created && <p className="muted tiny">This claim id already existed, so it was updated (submissions are idempotent).</p>}
        </div>
      )}
      {alert && (
        <div className={`ring-alert ${alert.level === "review_recommended" ? "review" : ""}`} role="status">
          <strong>{alert.level === "review_recommended" ? `Joins ${alert.ring_id} · review recommended` : `Part of ${alert.ring_id}`}</strong>
          <p className="small">{alert.message}</p>
          <button className="link" onClick={() => onOpenRing(alert.claim_id)}>
            Open ring view ({alert.size} claims)
          </button>
        </div>
      )}

      {editing && (
        <EditDialog draft={draft} setDraft={setDraft} onClose={() => setEditing(false)} onRestore={() => setDraft(preset.build(draft.claim_id))} />
      )}
    </section>
  );
}
