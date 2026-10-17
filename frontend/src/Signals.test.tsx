import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import { fnolResult, installFakeApi, reportA, summary } from "./test/fakeApi";
import type { ActionPlan, Report, RingAlert } from "./types";

// Cytoscape needs a real canvas; both canvases are covered by the browser e2e tests.
vi.mock("./components/GraphPanel", () => ({
  default: (p: { graph: { target_claim: string } | null }) => <div data-testid="graph" data-target={p.graph?.target_claim ?? ""} />,
}));
vi.mock("./components/RingGraph", () => ({
  default: (p: { ring: { target_claim: string } }) => <div data-testid="ring-graph" data-target={p.ring.target_claim} />,
}));

const plan = (id: string): ActionPlan => ({
  target_claim: id,
  note: "Recommended verification for a human investigator; it is not a decision about the claim.",
  actions: [
    {
      kind: "hold_linked_claims_for_review", priority: 1, simulated: true, requires_human_approval: true,
      title: "Hold the 2 open claims of RING-CLM-1 for joint review (simulated)", rationale: "The claim belongs to RING-CLM-1.",
      claims: ["CLM-2", "CLM-7"],
    },
    {
      kind: "verify_payout_account_holder", priority: 2, requires_human_approval: true,
      title: "Verify who holds payout account PAY-01", rationale: "Two claimants are paid into one account.",
      claims: ["CLM-2"], entities: ["PAY-01"], evidence_path: "CLM-1 -[PAID_TO]-> PayoutDestination(PAY-01) <-[PAID_TO]- CLM-2",
    },
  ],
});

const withPlan = (id: string): Report => {
  const r = reportA(id);
  return {
    ...r,
    recommended_actions: plan(id),
    investigation: {
      ...r.investigation, outcome: "connected",
      traversal_stats: { engine: "falkordb_in_database", queries: 4, paths_returned: 37, duration_ms: 271 },
    },
  };
};

async function ready(api: ReturnType<typeof installFakeApi>) {
  render(<App />);
  await screen.findByText("High Priority");
  await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-1"));
  return api;
}

describe("recommended verifications (deterministic, from the graph)", () => {
  it("are shown with the investigation, before and without any agent run", async () => {
    const api = await ready(installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => withPlan(p.id) }));
    const list = within(screen.getByRole("list", { name: "Recommended verifications" }));
    expect(list.getAllByRole("listitem")).toHaveLength(2);
    expect(list.getByText(/Hold the 2 open claims of RING-CLM-1/)).toBeInTheDocument();
    expect(list.getByText("Simulated")).toBeInTheDocument();
    expect(list.getAllByText("Needs human approval")).toHaveLength(2);
    expect(screen.getByText(/it is not a decision about the claim/)).toBeInTheDocument();
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("cite the exact evidence path when opened", async () => {
    await ready(installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => withPlan(p.id) }));
    await userEvent.click(screen.getByText("Verify who holds payout account PAY-01"));
    expect(screen.getByText("CLM-1 -[PAID_TO]-> PayoutDestination(PAY-01) <-[PAID_TO]- CLM-2")).toBeVisible();
    expect(screen.getByText("Identifiers: PAY-01")).toBeVisible();
  });

  it("stay on screen during a counterfactual (they belong to the full evidence)", async () => {
    const api = await ready(installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => withPlan(p.id) }));
    await userEvent.click(screen.getByRole("button", { name: "Repair shop" }));
    await waitFor(() => expect(api.count("POST", "/counterfactual")).toBe(1));
    expect(await screen.findByText(/falls by 8/)).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Recommended verifications" })).toBeInTheDocument();
  });

  it("are absent when the API sends no plan", async () => {
    await ready(installFakeApi());
    expect(screen.queryByRole("list", { name: "Recommended verifications" })).not.toBeInTheDocument();
  });
});

describe("evidence panel: FalkorDB timing and explicit outcomes", () => {
  it("shows how the traversal ran", async () => {
    await ready(installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => withPlan(p.id) }));
    expect(screen.getByText(/Traversal ran inside FalkorDB · 4 queries · 37 paths examined · 271 ms/)).toBeInTheDocument();
  });

  it("states 'no supporting connection' as a result, and never as a clearance", async () => {
    const none = (id: string): Report => {
      const r = reportA(id);
      return { ...r, score: 0, band: "Low", factors: [], connected_claims: 0,
        investigation: { ...r.investigation, related_claims: [], outcome: "no_supporting_connection", message: "No supporting connection to other claims was found in the graph." } };
    };
    installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => none(p.id) });
    render(<App />);
    expect(await screen.findByRole("status")).toHaveTextContent("No supporting connection found in the graph");
    expect(screen.getByRole("status")).toHaveTextContent(/not a clearance/);
  });

  it("lists shared infrastructure with exact counts instead of treating its users as connections", async () => {
    const infra = (id: string): Report => {
      const r = reportA(id);
      return { ...r, score: 8, band: "Low", connected_claims: 0,
        investigation: { ...r.investigation, related_claims: [], outcome: "infrastructure_only", truncated: true,
          infrastructure_links: [{ category: "repair_shop", entity_type: "RepairShop", entity_id: "SHOP-0000", rel: "REPAIRED_AT", other_claims: 1493 }] } };
    };
    installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => infra(p.id) });
    render(<App />);
    expect(await screen.findByText("Linked only through shared infrastructure")).toBeInTheDocument();
    expect(screen.getByText("SHOP-0000")).toBeInTheDocument();
    expect(screen.getByText(/used by 1,493 other claims/)).toBeInTheDocument();
    expect(screen.getByText(/some evidence may be missing/)).toBeInTheDocument();
  });

  it("lists the claimant's own other claims as context, with their recorded decision, never as connections", async () => {
    const own = (id: string): Report => {
      const r = reportA(id);
      return { ...r,
        investigation: { ...r.investigation, outcome: "connected",
          same_claimant_claims: { claimant_id: "DRV-010128", total: 3,
            claims: [{ claim_id: "CLM-105247", filing_date: "2025-05-09", decision: "confirmed_fraud" }, { claim_id: "CLM-100001", filing_date: "2024-01-02" }] } } };
    };
    installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => own(p.id) });
    render(<App />);
    const block = await screen.findByTestId("own-claims");
    expect(within(block).getByText(/This claimant's other claims/)).toBeInTheDocument();
    expect(within(block).getByText("CLM-105247")).toBeInTheDocument();
    expect(within(block).getByText(/Confirmed Fraud/)).toBeInTheDocument();
    expect(within(block).getByText(/never counted as links between claims/)).toBeInTheDocument();
    expect(within(block).getByText(/DRV-010128/)).toBeInTheDocument();
    expect(within(block).getByText("Showing the newest 2 of 3.")).toBeInTheDocument();
    // Not in the list of connected claims.
    expect(document.querySelectorAll(".ev-item strong")).not.toHaveLength(0);
    for (const el of document.querySelectorAll(".ev-item strong")) expect(el.textContent).not.toBe("CLM-105247");
  });
});

describe("FNOL ring alert (algo.WCC at intake)", () => {
  const alert = (id: string): RingAlert => ({
    claim_id: id, ring_id: "RING-CLM-1", size: 4, confirmed_fraud_cases: 1, level: "review_recommended",
    traits: { claimants: 4, addresses: 4, max_claims_in_90_days: 4, claims_on_new_policies: 0, indicators: ["spans_households", "confirmed_case"] },
    message: `Claim ${id} joins RING-CLM-1: 4 claims by 4 claimants linked by strong shared identifiers. Investigator review is recommended; this is a lead for verification, not a finding of fraud.`,
  });

  it("tells the intake user the clean-looking claim joins a ring, and opens the ring view on request", async () => {
    const api = installFakeApi();
    api.handlers["POST /api/fnol"] = (c) => {
      api.setClaims([summary(c.body.claim_id), summary("CLM-1"), summary("CLM-2")]);
      return { ...fnolResult(c.body.claim_id), ring_alert: alert(c.body.claim_id) };
    };
    await ready(api);
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    expect(await screen.findByText("Joins RING-CLM-1 · review recommended")).toBeInTheDocument();
    expect(screen.getByText(/Ordinary policy checks passed/)).toBeInTheDocument(); // looks clean on its own...
    expect(screen.getByText(/not a finding of fraud/)).toBeInTheDocument();
    const id = api.where("POST", "/api/fnol")[0].body.claim_id;
    await userEvent.click(screen.getByRole("button", { name: "Open ring view (4 claims)" }));
    await waitFor(() => expect(screen.getByTestId("ring-graph")).toHaveAttribute("data-target", id));
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("shows nothing for a claim that joins no ring", async () => {
    await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    await screen.findByText(/Ordinary policy checks passed/);
    expect(screen.queryByText(/Open ring view/)).not.toBeInTheDocument();
  });
});
