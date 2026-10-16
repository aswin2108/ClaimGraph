import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import { deferred, HttpError, installFakeApi, loneRing, reportA, strongestLink } from "./test/fakeApi";

// Cytoscape needs a real canvas; both canvases are covered by the browser e2e tests.
vi.mock("./components/GraphPanel", () => ({
  default: (p: { graph: { target_claim: string } | null; ringSize?: number; onView: (v: string) => void; onHops: (n: number) => void }) => (
    <div data-testid="graph" data-target={p.graph?.target_claim ?? ""} data-ring-size={p.ringSize ?? ""}>
      <button onClick={() => p.onView("ring")}>open-ring</button>
      <button onClick={() => p.onHops(1)}>set-hops-1</button>
    </div>
  ),
}));
vi.mock("./components/RingGraph", () => ({
  default: (p: { ring: { target_claim: string; backbone: unknown[] }; link: { to_claim: string } | null; onPick: (id: string | null) => void }) => (
    <div data-testid="ring-graph" data-target={p.ring.target_claim} data-edges={p.ring.backbone.length} data-link={p.link?.to_claim ?? ""}>
      <button onClick={() => p.onPick("CLM-7")}>canvas-pick-CLM-7</button>
    </div>
  ),
}));

const queue = () => within(screen.getByRole("list", { name: "Claim queue" }));

async function ready(api: ReturnType<typeof installFakeApi>) {
  render(<App />);
  await screen.findByText("High Priority");
  await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-ring-size", "3"));
  return api;
}

async function openRing() {
  await userEvent.click(screen.getByText("open-ring"));
  return screen.findByTestId("ring-graph");
}

describe("claim ring (algo.WCC + algo.MSF)", () => {
  it("fetches the ring once per selected claim, never for hop changes or exclusions, and never calls the agent", async () => {
    const api = await ready(installFakeApi());
    expect(api.count("GET", /\/ring$/)).toBe(1);

    await userEvent.click(screen.getByText("set-hops-1"));
    await waitFor(() => expect(api.where("GET", "/investigation").some((c) => c.query.get("hops") === "1")).toBe(true));
    await userEvent.click(screen.getByRole("button", { name: "Repair shop" }));
    await waitFor(() => expect(api.count("POST", "/counterfactual")).toBe(1));
    expect(api.count("GET", /\/ring$/)).toBe(1);

    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(api.where("GET", "/api/claims/CLM-2/ring")).toHaveLength(1));
    expect(api.count("GET", /\/ring$/)).toBe(2);
    expect(api.count("GET", "/link/")).toBe(0);
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("shows the ring's facts, indicators, hub and which members only the ring reveals", async () => {
    await ready(installFakeApi());
    const canvas = await openRing();
    expect(canvas).toHaveAttribute("data-edges", "4");
    expect(screen.getByRole("tab", { name: "Ring · 3 claims" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("RING-CLM-1")).toBeInTheDocument();
    expect(screen.getByText("Spans households: 3 addresses")).toBeInTheDocument();
    expect(screen.getByText("Confirmed case on a member: 1 case")).toBeInTheDocument();
    expect(screen.getByText("hub")).toBeInTheDocument();
    expect(screen.getByText("algo.WCC + algo.MSF · 6 ms")).toBeInTheDocument();
    // CLM-2 is in the traversal (1 hop); CLM-7 is found only by the ring
    expect(screen.getByRole("button", { name: "CLM-2, 1 hop, Confirmed Fraud" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "CLM-7, ring only" })).toBeInTheDocument();
    expect(screen.getByText("1 member not reached by the ≤ 3-hop traversal")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "CLM-1, target" })).toBeDisabled();
    expect(screen.getByText(/never change the priority score/)).toBeInTheDocument();
  });

  it("clicking a member fires one strongest-link request and draws the chain; clicking it again clears without a request", async () => {
    const api = await ready(installFakeApi());
    await openRing();
    const member = screen.getByRole("button", { name: "CLM-7, ring only" });
    await userEvent.click(member);
    expect(await screen.findByText("Strongest link to CLM-7")).toBeInTheDocument();
    expect(api.where("GET", "/api/claims/CLM-1/link/CLM-7")).toHaveLength(1);
    expect(screen.getByText("evidence cost 18 (lower is stronger)")).toBeInTheDocument();
    expect(screen.getByTestId("ring-graph")).toHaveAttribute("data-link", "CLM-7");

    await userEvent.click(member);
    expect(screen.queryByText("Strongest link to CLM-7")).not.toBeInTheDocument();
    expect(screen.getByTestId("ring-graph")).toHaveAttribute("data-link", "");
    expect(api.count("GET", "/link/")).toBe(1);

    await userEvent.click(screen.getByText("canvas-pick-CLM-7")); // a click on the canvas node does the same
    await screen.findByText("Strongest link to CLM-7");
    expect(api.count("GET", "/link/")).toBe(2);
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("a late strongest-link answer for a member the investigator left is ignored", async () => {
    const slow = deferred();
    await ready(
      installFakeApi({
        "GET /api/claims/:id/link/:other": async (_c, p) => {
          if (p.other === "CLM-7") await slow.promise;
          return strongestLink(p.id, p.other);
        },
      }),
    );
    await openRing();
    await userEvent.click(screen.getByRole("button", { name: "CLM-7, ring only" }));
    await userEvent.click(screen.getByRole("button", { name: /^CLM-2, 1 hop/ }));
    expect(await screen.findByText("Strongest link to CLM-2")).toBeInTheDocument();
    slow.resolve();
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("Strongest link to CLM-2")).toBeInTheDocument();
    expect(screen.queryByText("Strongest link to CLM-7")).not.toBeInTheDocument();
  });

  it("a claim with no strong shared identifier says so instead of drawing a ring", async () => {
    installFakeApi({ "GET /api/claims/:id/ring": (_c, p) => loneRing(p.id) });
    render(<App />);
    await screen.findByText("High Priority");
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-ring-size", "1"));
    await userEvent.click(screen.getByText("open-ring"));
    expect(screen.getByRole("tab", { name: "Ring · none" })).toBeInTheDocument();
    expect(screen.getByText(/belongs to no ring/)).toBeInTheDocument();
    expect(screen.queryByTestId("ring-graph")).not.toBeInTheDocument();
  });

  it("an algorithm failure stays inside the ring view and never blocks the investigation", async () => {
    const api = installFakeApi({
      "GET /api/claims/:id/ring": () => {
        throw new HttpError(503, { error: "graph algorithms are not ready" });
      },
    });
    render(<App />);
    expect(await screen.findByText("High Priority")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await waitFor(() => expect(api.count("GET", /\/ring$/)).toBe(1));
    await userEvent.click(screen.getByText("open-ring"));
    expect(await screen.findByText("graph algorithms are not ready")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: /Traversal/ }));
    expect(screen.getByTestId("graph")).toBeInTheDocument();
  });

  it("recording a disposition refreshes the ring (its recorded cases changed)", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Not Substantiated" }));
    await waitFor(() => expect(api.count("POST", "/disposition")).toBe(1));
    await waitFor(() => expect(api.count("GET", /\/ring$/)).toBe(2));
  });

  it("does not mark members 'ring only' when the traversal list was cut by a limit", async () => {
    const cut = (id: string) => {
      const r = reportA(id);
      return { ...r, investigation: { ...r.investigation, connected_total: 60 } };
    };
    await ready(installFakeApi({ "GET /api/claims/:id/investigation": (_c, p) => cut(p.id) }));
    await openRing();
    expect(screen.queryByText(/ring only/)).not.toBeInTheDocument();
    expect(screen.queryByText(/not reached by/)).not.toBeInTheDocument();
  });
});
