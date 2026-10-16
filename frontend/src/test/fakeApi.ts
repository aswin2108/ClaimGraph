import { vi } from "vitest";
import type {
  AgentResult, AlgoLink, ClaimDetail, ClaimSummary, Counterfactual, DispositionCase, Factor, FNOLResult, GraphView, Meta, Report, Ring,
  RingList, StrongestLink,
} from "../types";

/** One request that reached the (fake) network. */
export interface Call {
  method: string;
  path: string; // without query string
  query: URLSearchParams;
  body: any;
}

export type Handler = (call: Call, params: Record<string, string>) => unknown | Promise<unknown>;

export class HttpError {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown) {
    this.status = status;
    this.body = body;
  }
}

// ---------- fixtures (typed, so they break at compile time if the API contract types change) ----------

export const meta: Meta = {
  counterfactual_categories: ["payout", "device", "contact", "address", "repair_shop", "public_ip"],
  dispositions: ["confirmed_fraud", "not_substantiated", "needs_info"],
  max_hops: 3,
  disclaimer: "This is an investigation-priority score for human review. It is not a fraud determination.",
  llm_enabled: true,
  agent_budget: { llm_runs_last_hour: 0, max_llm_runs_per_hour: 30, llm_tokens_today: 0, daily_token_limit: 1_000_000 },
};

export const summary = (id: string, extra: Partial<ClaimSummary> = {}): ClaimSummary => ({
  claim_id: id, filing_date: "2026-09-09", incident_date: "2026-09-08", amount: 4200, status: "open", policy_check_status: "passed", ...extra,
});

export const detail = (id: string): ClaimDetail => ({ ...summary(id), loss_type: "collision" });

const factor = (name: string, points: number): Factor => ({ name, points, reason: name });

export const report = (id: string, score: number, band: string, factors: Factor[], related: string[] = ["CLM-2"]): Report => ({
  target_claim: id, score, band, band_meaning: "meaning", connected_claims: related.length, factors, prior_cases: [], uncertainty: ["A joint account could explain this."],
  disclaimer: meta.disclaimer,
  investigation: {
    target_claim: id, max_hops: 3, truncated: false,
    related_claims: related.map((r) => ({
      claim_id: r, hops: 1, filing_date: "2026-09-07",
      paths: [{ hops: 1, description: `${id} -[PAID_TO]-> PayoutDestination(PAY-01) <-[PAID_TO]- ${r}`, steps: [{
        from_claim: id, to_claim: r, entity_type: "PayoutDestination", entity_id: "PAY-01", entity_subtype: "claimant", from_rel: "PAID_TO", to_rel: "PAID_TO",
      }] }],
    })),
  },
});

export const FACTORS_A = [factor("shared_claimant_payout", 25), factor("shared_repair_shop", 8)];
export const reportA = (id: string) => report(id, 88, "High Priority", FACTORS_A);
export const reportNoShop = (id: string) => report(id, 80, "High Priority", [factor("shared_claimant_payout", 25)]);

export const graph = (id: string, exclude?: string): GraphView => ({
  target_claim: id, excluded_category: exclude, truncated: false,
  nodes: [{ id: `claim:${id}`, kind: "claim", label: id, target: true, hops: 0 }],
  edges: [],
});

export const counterfactual = (id: string, category: string): Counterfactual => ({
  excluded_category: category, original: reportA(id), counterfactual: reportNoShop(id), delta: -8,
  removed_factors: [{ name: "shared_repair_shop", original_points: 8, counterfactual_points: 0 }],
  still_justifies_investigation: true, explanation: "Without the repair shop the score falls by 8.",
});

export const agentResult = (id: string, source = "llm"): AgentResult => ({
  report: reportA(id), source, model: "fake-model", fallback_reason: source === "llm" ? undefined : "llm_disabled",
  explanation: {
    summary: `Claim ${id} shares a payout destination with CLM-2.`,
    key_findings: [{ statement: "Payout PAY-01 is shared.", claim_ids: ["CLM-2"], entities: ["PAY-01"] }],
    uncertainty: ["A joint account could explain this."],
    next_step: { action: "Verify the account holder of PAY-01.", rationale: "Strongest signal.", claim_ids: ["CLM-2"], entities: ["PAY-01"] },
  },
  tool_trace: [{ seq: 1, tool: "GetClaim", input: "{}", ok: true, duration_ms: 3 }],
  usage: { turns: 2, tool_calls: 1, input_tokens: 10, output_tokens: 5 },
});

export const dispositionCase = (id: string, disposition: string): DispositionCase => ({
  case_id: `CASE-${id}`, claim_id: id, disposition, recorded_at: "2026-10-15T10:00:00Z", created: true,
});

export const fnolResult = (id: string, failed: string[] = []): FNOLResult => ({
  claim_id: id, created: true, policy_check_status: failed.length ? "failed" : "passed",
  policy_checks: ["policy_active", "incident_within_coverage_period", "amount_within_limit"].map((name) => ({ name, passed: !failed.includes(name) })),
});

const algoLink = (fromType: string, from: string, rel: string, toType: string, to: string, cost: number): AlgoLink => ({
  from: { type: fromType, id: from }, rel, to: { type: toType, id: to }, evidence_cost: cost,
});

/** CLM-1's ring: CLM-2 shares its payout account (1 hop); CLM-7 is reached only through CLM-2's device. */
export const ring = (id: string): Ring => ({
  ring_id: "RING-CLM-1", target_claim: id, algorithm: "algo.WCC + algo.MSF", size: 3, oversized: false,
  members: [
    { claim_id: "CLM-1", filing_date: "2026-09-09", amount: 4200 },
    { claim_id: "CLM-2", filing_date: "2026-09-07", amount: 3900, recorded_dispositions: ["confirmed_fraud"] },
    { claim_id: "CLM-7", filing_date: "2026-08-30", amount: 5100 },
  ],
  shared_entities: [
    { entity_type: "PayoutDestination", entity_id: "PAY-01", entity_subtype: "claimant", member_claims: 3 },
    { entity_type: "Device", entity_id: "DEV-9", member_claims: 2 },
  ],
  hub_entity: { entity_type: "PayoutDestination", entity_id: "PAY-01", entity_subtype: "claimant", member_claims: 3 },
  first_filed: "2026-08-30", last_filed: "2026-09-09", filing_span_days: 10, total_amount: 13200,
  confirmed_fraud_cases: 1, other_recorded_cases: 0,
  backbone: [
    algoLink("Claim", id, "PAID_TO", "PayoutDestination", "PAY-01", 4),
    algoLink("Claim", "CLM-2", "PAID_TO", "PayoutDestination", "PAY-01", 4),
    algoLink("Claim", "CLM-2", "USED_DEVICE", "Device", "DEV-9", 5),
    algoLink("Claim", "CLM-7", "USED_DEVICE", "Device", "DEV-9", 5),
  ],
  backbone_cost: 18, truncated: false, note: "A ring is a group of claims connected through strong shared identifiers.",
  algorithm_results_cached: true, duration_ms: 6,
  traits: { claimants: 3, addresses: 3, max_claims_in_90_days: 3, claims_on_new_policies: 0, indicators: ["spans_households", "confirmed_case"] },
});

export const loneRing = (id: string): Ring => ({
  ...ring(id), ring_id: undefined, size: 1, members: [{ claim_id: id, amount: 0 }], shared_entities: [], hub_entity: undefined,
  backbone: [], backbone_cost: 0, filing_span_days: 0, total_amount: 0, confirmed_fraud_cases: 0,
  message: "The claim shares no strong identifier with any other claim, so it belongs to no ring.",
  traits: { claimants: 0, addresses: 0, max_claims_in_90_days: 0, claims_on_new_policies: 0, indicators: null as unknown as string[] },
});

export const strongestLink = (from: string, to: string): StrongestLink => ({
  from_claim: from, to_claim: to, algorithm: "algo.SPpaths", found: true, total_evidence_cost: 18,
  claims_on_path: [from, "CLM-2", to],
  links: [
    algoLink("Claim", from, "PAID_TO", "PayoutDestination", "PAY-01", 4),
    algoLink("PayoutDestination", "PAY-01", "PAID_TO", "Claim", "CLM-2", 4),
    algoLink("Claim", "CLM-2", "USED_DEVICE", "Device", "DEV-9", 5),
    algoLink("Device", "DEV-9", "USED_DEVICE", "Claim", to, 5),
  ],
  evidence_used: ["PayoutDestination(PAY-01)", "Device(DEV-9)"],
  description: `${from} - PayoutDestination(PAY-01) - CLM-2 - Device(DEV-9) - ${to}`, note: "", duration_ms: 4,
});

export const ringList = (): RingList => ({
  algorithm: "algo.WCC", min_size: 3, total_rings: 2, truncated: false, ordering: "indicators first", note: "", duration_ms: 12,
  rings: [
    { ring_id: "RING-CLM-1", size: 3, claims: ["CLM-1", "CLM-2", "CLM-7"], filing_span_days: 10, total_amount: 13200, confirmed_fraud_cases: 1,
      claims_without_recorded_outcome: 2, oversized: false, claimants: 3, addresses: 3, max_claims_in_90_days: 3, claims_on_new_policies: 0,
      indicators: ["spans_households", "confirmed_case"] },
    { ring_id: "RING-CLM-40", size: 4, claims: ["CLM-40", "CLM-41", "CLM-42", "CLM-43"], filing_span_days: 300, total_amount: 9000,
      confirmed_fraud_cases: 0, claims_without_recorded_outcome: 4, oversized: false, claimants: 2, addresses: 1, max_claims_in_90_days: 1,
      claims_on_new_policies: 0, indicators: [] },
  ],
});

// ---------- the fake network ----------

export function installFakeApi(overrides: Record<string, Handler> = {}) {
  const calls: Call[] = [];
  let claims: ClaimSummary[] = [summary("CLM-1"), summary("CLM-2", { filing_date: "2026-09-07" })];

  const defaults: Record<string, Handler> = {
    "GET /api/meta": () => meta,
    "GET /api/claims": (c) => {
      const q = (c.query.get("q") ?? "").toUpperCase();
      const offset = Number(c.query.get("offset") ?? 0);
      const limit = Number(c.query.get("limit") ?? 100);
      const hits = claims.filter((x) => !q || x.claim_id.includes(q));
      return { claims: hits.slice(offset, offset + limit), total: hits.length, offset, limit, q: q || undefined };
    },
    "GET /api/claims/:id": (_c, p) => detail(p.id),
    "GET /api/claims/:id/investigation": (_c, p) => reportA(p.id),
    "GET /api/claims/:id/graph": (c, p) => graph(p.id, c.query.get("exclude") ?? undefined),
    "POST /api/claims/:id/counterfactual": (c, p) => counterfactual(p.id, c.body.exclude),
    "POST /api/claims/:id/disposition": (c, p) => dispositionCase(p.id, c.body.disposition),
    "POST /api/claims/:id/agent": (_c, p) => agentResult(p.id),
    "POST /api/fnol": (c) => fnolResult(c.body.claim_id),
    "GET /api/claims/:id/ring": (_c, p) => ring(p.id),
    "GET /api/claims/:id/link/:other": (_c, p) => strongestLink(p.id, p.other),
    "GET /api/rings": () => ringList(),
  };
  const handlers = { ...defaults, ...overrides };

  const match = (method: string, path: string): [Handler, Record<string, string>] | null => {
    for (const [key, h] of Object.entries(handlers)) {
      const [m, pattern] = key.split(" ");
      if (m !== method) continue;
      const names: string[] = [];
      const re = new RegExp("^" + pattern.replace(/:(\w+)/g, (_x, n) => (names.push(n), "([^/]+)")) + "$");
      const hit = re.exec(path);
      if (hit) return [h, Object.fromEntries(names.map((n, i) => [n, decodeURIComponent(hit[i + 1])]))];
    }
    return null;
  };

  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://localhost");
    const method = (init?.method ?? "GET").toUpperCase();
    const call: Call = { method, path: url.pathname, query: url.searchParams, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    const m = match(method, url.pathname);
    if (!m) return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    try {
      return new Response(JSON.stringify(await m[0](call, m[1])), { status: 200, headers: { "Content-Type": "application/json" } });
    } catch (e) {
      if (e instanceof HttpError) return new Response(JSON.stringify(e.body), { status: e.status });
      throw e;
    }
  });
  vi.stubGlobal("fetch", fetchMock);

  return {
    calls,
    handlers,
    setClaims: (c: ClaimSummary[]) => (claims = c),
    /** Requests matching "METHOD path-regexp-or-suffix". */
    count: (method: string, pathPart: string | RegExp) =>
      calls.filter((c) => c.method === method && (typeof pathPart === "string" ? c.path.includes(pathPart) : pathPart.test(c.path))).length,
    where: (method: string, pathPart: string) => calls.filter((c) => c.method === method && c.path.includes(pathPart)),
  };
}

/** A promise you resolve by hand, to hold a request open. */
export function deferred<T = void>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}
