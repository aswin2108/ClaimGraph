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
  connected_total?: number; // every connected claim; related_claims lists at most 50
  truncated: boolean; // a limit may have hidden evidence
  paths_trimmed?: boolean; // only example paths are shown for some claims (no evidence lost)
  message?: string;
  outcome?: "connected" | "infrastructure_only" | "no_supporting_connection";
  traversal_stats?: TraversalStats;
  infrastructure_links?: InfrastructureLink[];
}
export interface TraversalStats {
  engine: string;
  queries: number;
  paths_returned: number;
  duration_ms: number;
}
export interface InfrastructureLink {
  category: string;
  entity_type: string;
  entity_id: string;
  entity_subtype?: string;
  entity_public?: boolean;
  rel: string;
  other_claims: number;
  evidence_id?: string;
  recorded_at?: string;
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
  recommended_actions?: ActionPlan; // only on GET /investigation, not on counterfactual reports
  ring_alert?: RingAlert;
}
export interface RecommendedAction {
  kind: string;
  priority: number;
  title: string;
  rationale: string;
  claims?: string[];
  entities?: string[];
  evidence_path?: string;
  simulated?: boolean;
  requires_human_approval: boolean;
}
export interface ActionPlan {
  target_claim: string;
  actions: RecommendedAction[];
  note: string;
}
export interface RingAlert {
  claim_id: string;
  ring_id: string;
  size: number;
  confirmed_fraud_cases: number;
  traits: RingTraits;
  level: "review_recommended" | "context";
  message: string;
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
  ring_alert?: RingAlert;
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

// ---------- graph algorithms (context only; never part of the score) ----------

export interface AlgoNode {
  type: string; // domain label: Claim, Claimant, PayoutDestination, Device, ContactPoint, Address
  id: string;
}
export interface AlgoLink {
  from: AlgoNode;
  rel: string;
  to: AlgoNode;
  evidence_cost: number;
}
export interface RingMember {
  claim_id: string;
  filing_date?: string;
  amount: number;
  recorded_dispositions?: string[];
}
export interface RingEntity {
  entity_type: string;
  entity_id: string;
  entity_subtype?: string;
  member_claims: number;
}
export interface RingTraits {
  claimants: number;
  addresses: number;
  max_claims_in_90_days: number;
  claims_on_new_policies: number;
  indicators: string[];
}
export interface Ring {
  ring_id?: string;
  target_claim: string;
  algorithm: string;
  size: number;
  oversized: boolean;
  members: RingMember[];
  shared_entities: RingEntity[];
  hub_entity?: RingEntity;
  first_filed?: string;
  last_filed?: string;
  filing_span_days: number;
  total_amount: number;
  confirmed_fraud_cases: number;
  other_recorded_cases: number;
  backbone: AlgoLink[];
  backbone_cost: number;
  truncated: boolean;
  message?: string;
  note: string;
  algorithm_results_cached: boolean;
  traits: RingTraits;
  duration_ms: number;
}
export interface StrongestLink {
  from_claim: string;
  to_claim: string;
  algorithm: string;
  found: boolean;
  total_evidence_cost?: number;
  claims_on_path?: string[];
  links: AlgoLink[] | null;
  evidence_used?: string[];
  description: string;
  note: string;
  duration_ms: number;
}
export interface RingSummary extends RingTraits {
  ring_id: string;
  size: number;
  claims: string[]; // up to 10 members, sorted
  first_filed?: string;
  last_filed?: string;
  filing_span_days: number;
  total_amount: number;
  confirmed_fraud_cases: number;
  claims_without_recorded_outcome: number;
  oversized: boolean;
}
export interface RingList {
  algorithm: string;
  min_size: number;
  total_rings: number;
  truncated: boolean;
  rings: RingSummary[];
  ordering: string;
  note: string;
  duration_ms: number;
}
