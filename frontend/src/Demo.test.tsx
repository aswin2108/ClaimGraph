import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import { HttpError, installFakeApi, meta, reportA, summary } from "./test/fakeApi";
import type { Report } from "./types";

vi.mock("./components/GraphPanel", () => ({
  default: (p: { graph: { target_claim: string } | null }) => <div data-testid="graph" data-target={p.graph?.target_claim ?? ""} />,
}));
vi.mock("./components/RingGraph", () => ({ default: () => <div data-testid="ring-graph" /> }));

const scaleMeta = { ...meta, graph: "claimgraph_scale", demo_reset_available: true };
const stats = { nodes: 122455, relationships: 175527, labels: {}, relationship_types: {}, evidence_version: 3, analytics_overlay_ready: true, duration_ms: 4 };
const resetResult = { graph: "claimgraph_scale", restored_from: "claimgraph_scale__pristine", claims: 20000, duration_ms: 1520 };

async function ready(api: ReturnType<typeof installFakeApi>) {
  render(<App />);
  await screen.findByText("High Priority");
  await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-1"));
  return api;
}

describe("Reset demo", () => {
  it("is hidden unless the server has a snapshot to restore", async () => {
    await ready(installFakeApi());
    expect(screen.queryByRole("button", { name: "Reset demo" })).not.toBeInTheDocument();
  });

  it("needs two clicks, sends exactly one request, then starts over from the restored graph", async () => {
    const api = installFakeApi({ "GET /api/meta": () => scaleMeta, "POST /api/demo/reset": () => resetResult });
    await ready(api);
    await userEvent.click(await screen.findByRole("button", { name: "Reset demo" }));
    expect(api.count("POST", "/api/demo/reset")).toBe(0); // the first click only arms it
    api.setClaims([summary("CLM-2")]); // the restored graph no longer has CLM-1
    await userEvent.click(screen.getByRole("button", { name: "Click again to reset" }));

    expect(await screen.findByText(/Demo reset: the graph is back to its starting state \(20,000 claims/)).toBeInTheDocument();
    expect(api.count("POST", "/api/demo/reset")).toBe(1);
    expect(api.where("POST", "/api/demo/reset")[0].body).toEqual({ confirm: "reset" });
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("re-investigates when the first claim is still the selected one", async () => {
    const api = installFakeApi({ "GET /api/meta": () => scaleMeta, "POST /api/demo/reset": () => resetResult });
    await ready(api);
    const before = api.where("GET", "/api/claims/CLM-1/investigation").length;
    await userEvent.click(await screen.findByRole("button", { name: "Reset demo" }));
    await userEvent.click(screen.getByRole("button", { name: "Click again to reset" }));
    await screen.findByText(/Demo reset/);
    await waitFor(() => expect(api.where("GET", "/api/claims/CLM-1/investigation").length).toBe(before + 1));
  });

  it("shows the server's refusal and changes nothing", async () => {
    const api = installFakeApi({
      "GET /api/meta": () => scaleMeta,
      "POST /api/demo/reset": () => {
        throw new HttpError(429, { error: "the demo was just reset; try again in a few seconds" });
      },
    });
    await ready(api);
    await userEvent.click(await screen.findByRole("button", { name: "Reset demo" }));
    await userEvent.click(screen.getByRole("button", { name: "Click again to reset" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the demo was just reset");
    expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-1");
  });
});

describe("intake on the 20,000-claim book", () => {
  it("offers the showcase claim, shows its fields, and submits exactly that claim every time", async () => {
    const api = await ready(installFakeApi({ "GET /api/meta": () => scaleMeta }));
    expect(await screen.findByRole("button", { name: "Showcase: routine-looking claim" })).toHaveAttribute("aria-pressed", "true");
    const sum = within(screen.getByLabelText("Claim to submit"));
    expect(sum.getByText(/CLM-120001 · collision · \$12,764/)).toBeInTheDocument();
    expect(sum.getByText("ACCT-0000014")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    await screen.findByText(/Received CLM-120001/);
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    await waitFor(() => expect(api.count("POST", "/api/fnol")).toBe(2));
    expect(api.where("POST", "/api/fnol").map((c) => c.body.claim_id)).toEqual(["CLM-120001", "CLM-120001"]);
    expect(api.where("POST", "/api/fnol")[0].body.payout_account_id).toBe("ACCT-0000014");
  });

  it("control scenarios use new 7-digit claim ids that cannot overwrite a seeded claim", async () => {
    const api = await ready(installFakeApi({ "GET /api/meta": () => scaleMeta }));
    await userEvent.click(await screen.findByRole("button", { name: "Busiest repair shop only" }));
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    await waitFor(() => expect(api.count("POST", "/api/fnol")).toBe(1));
    const body = api.where("POST", "/api/fnol")[0].body;
    expect(body.claim_id).toMatch(/^CLM-9\d{6}$/);
    expect(body.repair_shop_id).toBe("SHOP-0000");
  });

  it("every field can be edited before submitting, and empty optional fields are not sent", async () => {
    const api = await ready(installFakeApi({ "GET /api/meta": () => scaleMeta }));
    await screen.findByRole("button", { name: "Showcase: routine-looking claim" });
    await userEvent.click(screen.getByRole("button", { name: "Edit fields" }));
    const dialog = within(screen.getByRole("dialog", { name: "Edit claim fields" }));
    const amount = dialog.getByLabelText("Amount (USD)");
    await userEvent.clear(amount);
    await userEvent.type(amount, "60000");
    await userEvent.clear(dialog.getByLabelText("IP address"));
    await userEvent.click(dialog.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(within(screen.getByLabelText("Claim to submit")).getByText(/\$60,000/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    await waitFor(() => expect(api.count("POST", "/api/fnol")).toBe(1));
    const body = api.where("POST", "/api/fnol")[0].body;
    expect(body.amount).toBe(60000);
    expect(body.claim_id).toBe("CLM-120001"); // typing stayed in the field being edited
    expect("ip_address" in body).toBe(false);
    expect(body.device_id).toBe("DEV-0000011");
  });

  it("Escape closes the dialog without submitting", async () => {
    const api = await ready(installFakeApi({ "GET /api/meta": () => scaleMeta }));
    await userEvent.click(await screen.findByRole("button", { name: "Edit fields" }));
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.count("POST", "/api/fnol")).toBe(0);
  });
});

describe("graph size next to the traversal timing (S2)", () => {
  it("names the size of the graph the traversal ran on", async () => {
    const withStats = (id: string): Report => {
      const r = reportA(id);
      return { ...r, investigation: { ...r.investigation, traversal_stats: { engine: "falkordb_in_database", queries: 4, paths_returned: 3, duration_ms: 347 } } };
    };
    await ready(installFakeApi({ "GET /api/graph/stats": () => stats, "GET /api/claims/:id/investigation": (_c, p) => withStats(p.id) }));
    expect(
      await screen.findByText(/Traversal ran inside FalkorDB on a graph of 122,455 nodes \/ 175,527 relationships · 4 queries · 3 paths examined · 347 ms/),
    ).toBeInTheDocument();
  });
});

describe("claim queue: search and paging over the whole book", () => {
  const many = Array.from({ length: 130 }, (_, i) => summary(`CLM-${String(100000 + i)}`, { filing_date: `2026-${String(1 + (i % 9)).padStart(2, "0")}-01` }));

  it("loads one page, shows the total, and 'Load more' appends the next page once", async () => {
    const api = installFakeApi();
    api.setClaims(many);
    render(<App />);
    const list = await screen.findByRole("list", { name: "Claim queue" });
    await waitFor(() => expect(within(list).getAllByRole("button", { name: /^CLM-/ })).toHaveLength(100));
    expect(screen.getByRole("tab", { name: "Claims · 130" })).toBeInTheDocument();
    expect(screen.getByText("Showing 100 of 130, newest first")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(within(list).getAllByRole("button", { name: /^CLM-/ })).toHaveLength(130));
    expect(api.where("GET", "/api/claims").filter((c) => c.path === "/api/claims").map((c) => c.query.get("offset"))).toEqual(["0", "100"]);
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("searching by claim id asks the API once the typing pauses, and shows the matches", async () => {
    const api = installFakeApi();
    api.setClaims(many);
    render(<App />);
    await screen.findByRole("tab", { name: "Claims · 130" });
    await userEvent.type(screen.getByRole("searchbox", { name: "Search claims by id" }), "10012");
    const list = screen.getByRole("list", { name: "Claim queue" });
    await waitFor(() => expect(within(list).getAllByRole("button", { name: /^CLM-/ })).toHaveLength(10));
    expect(screen.getByRole("tab", { name: "Claims · 10" })).toBeInTheDocument();
    const searches = api.where("GET", "/api/claims").filter((c) => c.path === "/api/claims" && c.query.get("q"));
    expect(searches.at(-1)!.query.get("q")).toBe("10012");
    expect(searches.length).toBeLessThanOrEqual(2); // debounced, not one request per keystroke
    await userEvent.click(within(list).getByRole("button", { name: /CLM-100125/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-100125"));
  });

  it("a search with no match says so", async () => {
    installFakeApi();
    render(<App />);
    await screen.findByText("High Priority");
    await userEvent.type(screen.getByRole("searchbox", { name: "Search claims by id" }), "ZZZ");
    expect(await screen.findByText('No claim id contains "ZZZ".')).toBeInTheDocument();
  });
});

describe("returning to a decided claim", () => {
  it("shows the earlier decision with its date and notes, and the new one after recording again", async () => {
    let decision: { case_id: string; disposition: string; notes?: string; recorded_at: string; source: string } | undefined = {
      case_id: "CASE-CLM-1", disposition: "needs_info", notes: "waiting for the bank", recorded_at: "2026-10-15T09:30:00Z", source: "investigator_simulated",
    };
    const api = installFakeApi({
      "GET /api/claims/:id": (_c, p) => ({ ...summary(p.id), disposition: decision?.disposition, recorded_decision: p.id === "CLM-1" ? decision : undefined }),
      "POST /api/claims/:id/disposition": (c, p) => {
        decision = { case_id: `CASE-${p.id}`, disposition: c.body.disposition, notes: c.body.notes, recorded_at: "2026-10-16T11:00:00Z", source: "investigator_simulated" };
        return { ...decision, claim_id: p.id, created: false, previous_disposition: "needs_info" };
      },
    });
    await ready(api);
    const box = await screen.findByLabelText("Recorded decision");
    expect(box).toHaveTextContent("Needs More Information");
    expect(box).toHaveTextContent("15 Oct 2026");
    expect(box).toHaveTextContent("“waiting for the bank”");

    await userEvent.type(screen.getByLabelText("Disposition notes"), "holder confirmed");
    await userEvent.click(screen.getByRole("button", { name: "Not Substantiated" }));
    await waitFor(() => expect(screen.getByLabelText("Recorded decision")).toHaveTextContent("Not Substantiated"));
    expect(screen.getByLabelText("Recorded decision")).toHaveTextContent("“holder confirmed”");
  });

  it("shows no decision box for a claim nobody has decided", async () => {
    await ready(installFakeApi());
    expect(screen.queryByLabelText("Recorded decision")).not.toBeInTheDocument();
  });
});
