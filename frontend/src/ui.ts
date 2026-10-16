// Shared labels and colours so every panel describes evidence the same way.

export const CATEGORY_LABEL: Record<string, string> = {
  payout: "Payout destination",
  device: "Device",
  contact: "Phone / email",
  address: "Address",
  repair_shop: "Repair shop",
  public_ip: "Public IP",
  vendor_payout: "Vendor settlement account",
  public_device: "Public device",
  private_ip: "Private network IP",
  tow_provider: "Tow provider",
};

export const CATEGORY_COLOR: Record<string, string> = {
  payout: "#e11d48",
  device: "#7c3aed",
  contact: "#0284c7",
  address: "#d97706",
  repair_shop: "#0d9488",
  public_ip: "#64748b",
  vendor_payout: "#94a3b8",
  public_device: "#94a3b8",
  private_ip: "#94a3b8",
  tow_provider: "#94a3b8",
};

export const FACTOR_LABEL: Record<string, string> = {
  shared_claimant_payout: "Shared payout destination",
  shared_non_public_device: "Shared device",
  shared_contact: "Shared phone / email",
  shared_address: "Shared address",
  shared_repair_shop: "Shared repair shop",
  shared_public_ip: "Shared public IP",
  connected_claims_breadth: "Connected-claim breadth",
  temporal_proximity: "Filed within 7 days",
  confirmed_fraud_history: "Confirmed outcome on a related claim",
  multi_signal_reinforcement: "Independent signals reinforce each other",
};

export const ENTITY_LABEL: Record<string, string> = {
  PayoutDestination: "Payout",
  Device: "Device",
  ContactPoint: "Contact",
  Address: "Address",
  RepairShop: "Repair shop",
  IPAddress: "IP",
  TowProvider: "Tow",
};

export const DISPOSITION_LABEL: Record<string, string> = {
  confirmed_fraud: "Confirmed Fraud",
  not_substantiated: "Not Substantiated",
  needs_info: "Needs More Information",
};

export const BAND_COLOR: Record<string, string> = {
  Low: "#16a34a",
  Review: "#d97706",
  "High Priority": "#dc2626",
};

export const money = (n: number) => n.toLocaleString("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 0 });

export const reducedMotion = () =>
  typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

/** Evidence category of a node type on a graph-algorithm result (the strong-evidence subgraph only). */
export const ALGO_CATEGORY: Record<string, string> = {
  PayoutDestination: "payout",
  Device: "device",
  ContactPoint: "contact",
  Address: "address",
};

export const RING_INDICATOR_LABEL: Record<string, string> = {
  spans_households: "Spans households",
  filing_burst: "Filing burst",
  new_policies: "New policies",
  confirmed_case: "Confirmed case on a member",
};
