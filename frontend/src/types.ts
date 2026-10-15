export interface Meta {
  counterfactual_categories: string[];
  dispositions: string[];
  max_hops: number;
  disclaimer: string;
  llm_enabled: boolean;
  agent_budget: { llm_runs_last_hour: number; max_llm_runs_per_hour: number; llm_tokens_today: number; daily_token_limit: number };
}

export interface ClaimSummary {
  claim_id: string;
  filing_date: string;
  incident_date: string;
  amount: number;
  status: string;
  policy_check_status: string;
  disposition?: string;
}

export interface Entity {
  id: string;
  name?: string;
  type?: string;
  public?: boolean;
  status?: string;
  start?: string;
  end?: string;
}

export interface ClaimDetail extends ClaimSummary {
  loss_type?: string;
  incident_location?: string;
  claimant?: Entity;
  policy?: Entity;
  vehicle?: Entity;
  repair_shop?: Entity;
  payout_destination?: Entity;
  tow_provider?: Entity;
  device?: Entity;
  ip_address?: Entity;
}

export interface Step {
  from_claim: string;
  to_claim: string;
  from_claimant?: string;
  to_claimant?: string;
  entity_type: string;
  entity_id: string;
  entity_subtype?: string;
  entity_public?: boolean;
  from_rel: string;
  to_rel: string;
  from_evidence?: string;
  to_evidence?: string;
  from_recorded_at?: string;
  to_recorded_at?: string;
}
export interface Path {
  hops: number;
  steps: Step[];
  description: string;
}
export interface RelatedClaim {
  claim_id: string;
  hops: number;
  filing_date?: string;
  incident_date?: string;
  paths: Path[];
}
export interface Investigation {
  target_claim: string;
  target_filing_date?: string;
  max_hops: number;
  related_claims: RelatedClaim[];
  truncated: boolean;
  message?: string;
}
export interface Factor {
  name: string;
  points: number;
  reason: string;
  claims?: string[];
  entities?: string[];
  evidence_ids?: string[];
}
export interface PriorCase {
  case_id: string;
  claim_id: string;
  disposition: string;
  notes?: string;
  timestamp?: string;
}
export interface Report {
  target_claim: string;
  score: number;
  band: string;
  band_meaning: string;
  connected_claims: number;
  factors: Factor[];
  prior_cases: PriorCase[];
  uncertainty: string[];
  excluded_category?: string;
  investigation: Investigation;
  disclaimer: string;
}
export interface FactorChange {
  name: string;
  original_points: number;
  counterfactual_points: number;
}
export interface Counterfactual {
  excluded_category: string;
  original: Report;
  counterfactual: Report;
  delta: number;
  removed_factors: FactorChange[];
  still_justifies_investigation: boolean;
  explanation: string;
}

export interface GraphNode {
  id: string;
  kind: "claim" | "claimant" | "entity";
  label: string;
  type?: string;
  category?: string;
  target?: boolean;
  hops?: number;
  context?: boolean;
}
export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  rel: string;
  category?: string;
  evidence_id?: string;
  recorded_at?: string;
  context?: boolean;
}
export interface GraphView {
  target_claim: string;
  excluded_category?: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
  truncated: boolean;
}

export interface Finding {
  statement: string;
  claim_ids: string[];
  entities: string[];
}
export interface NextStep {
  action: string;
  rationale: string;
  claim_ids: string[];
  entities: string[];
}
export interface Explanation {
  summary: string;
  key_findings: Finding[];
  uncertainty: string[];
  next_step: NextStep;
}
export interface TraceEntry {
  seq: number;
  tool: string;
  input: string;
  ok: boolean;
  error?: string;
  duration_ms: number;
}
export interface AgentResult {
  report: Report;
  explanation: Explanation;
  source: string;
  fallback_reason?: string;
  model?: string;
  tool_trace: TraceEntry[];
  usage: { turns: number; tool_calls: number; input_tokens: number; output_tokens: number };
}

export interface DispositionCase {
  case_id: string;
  claim_id: string;
  disposition: string;
  notes?: string;
  recorded_at: string;
  created: boolean;
  previous_disposition?: string;
}

export interface FNOLResult {
  claim_id: string;
  created: boolean;
  policy_checks: { name: string; passed: boolean }[];
  policy_check_status: string;
}

export interface FNOL {
  claim_id: string;
  claimant_id: string;
  claimant_name: string;
  policy_id: string;
  policy_start: string;
  policy_end: string;
  policy_status: string;
  vin: string;
  incident_date: string;
  filing_date: string;
  incident_location?: string;
  loss_type?: string;
  amount: number;
  payout_account_id: string;
  payout_type: string;
  repair_shop_id: string;
  tow_provider_id?: string;
  contact_type?: string;
  contact_value?: string;
  address_id?: string;
  device_id?: string;
  device_is_public?: boolean;
  ip_address?: string;
}
