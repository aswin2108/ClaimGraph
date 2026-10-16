import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import {
  agentResult, closure as closureOf, counterfactual, deferred, dispositionCase, fnolResult, graph, HttpError, installFakeApi, reportA, summary,
} from "./test/fakeApi";

// Cytoscape needs a real canvas; the graph panel is covered by the browser e2e tests.
vi.mock("./components/GraphPanel", () => ({
  default: (p: { graph: { target_claim: string } | null; excluded: string | null; hops: number; onHops: (n: number) => void }) => (
    <div data-testid="graph" data-target={p.graph?.target_claim ?? ""} data-excluded={p.excluded ?? ""} data-hops={p.hops}>
      <button onClick={() => p.onHops(2)}>set-hops-2</button>
    </div>
  ),
}));

const queue = () => within(screen.getByRole("list", { name: "Claim queue" }));

async function ready(api: ReturnType<typeof installFakeApi>) {
  render(<App />);
  await screen.findByText("High Priority");
  await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-1"));
  return api;
}

describe("loading and navigation", () => {
  it("loads the first claim without ever calling the agent", async () => {
    const api = await ready(installFakeApi());
    expect(api.count("GET", "/api/meta")).toBe(1);
    expect(api.count("GET", /\/investigation$/)).toBe(1);
    expect(api.count("GET", /\/graph$/)).toBe(1);
    expect(api.count("POST", "/agent")).toBe(0); // the LLM must only ever run on an explicit click
    expect(screen.getByText("88")).toBeInTheDocument();
  });

  it("selecting another claim re-investigates it, still without the agent", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    expect(api.where("GET", "/api/claims/CLM-2/investigation")).toHaveLength(1);
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("changing max hops refetches investigation and graph with the new hop count", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(screen.getByText("set-hops-2"));
    await waitFor(() => expect(api.where("GET", "/investigation").some((c) => c.query.get("hops") === "2")).toBe(true));
    expect(api.where("GET", "/graph").some((c) => c.query.get("hops") === "2")).toBe(true);
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("ignores a slow response for a claim the investigator has already left", async () => {
    const slow = deferred();
    const api = installFakeApi({
      "GET /api/claims/:id/investigation": async (_c, p) => {
        if (p.id === "CLM-1") await slow.promise;
        return reportA(p.id);
      },
    });
    render(<App />);
    await screen.findByText("CLM-2", { selector: "strong" });
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    slow.resolve(); // CLM-1's answer arrives late
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2");
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("shows a clear banner when the API is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("down"))));
    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/Cannot reach the ClaimGraph API/);
  });
});

describe("Run investigation agent (the only button that can spend LLM budget)", () => {
  it("fires exactly one request per click, and rapid double-clicks fire one", async () => {
    const hold = deferred();
    const api = await ready(
      installFakeApi({ "POST /api/claims/:id/agent": async (_c, p) => (await hold.promise, agentResult(p.id)) }),
    );
    const btn = screen.getByRole("button", { name: "Run investigation agent" });
    await userEvent.dblClick(btn);
    await userEvent.click(btn);
    expect(api.count("POST", "/agent")).toBe(1);
    expect(screen.getByText(/Agent is reading the graph through its MCP tools/)).toBeInTheDocument();
    hold.resolve();
    expect(await screen.findByText(/shares a payout destination with CLM-2/)).toBeInTheDocument();
    expect(api.count("POST", "/agent")).toBe(1);
    expect(api.where("POST", "/agent")[0].body).toEqual({ hops: 3 });
    // The tools were reached over MCP, and the panel says so.
    expect(screen.getByTestId("mcp-line")).toHaveTextContent(/found 2 tools on the claimgraph MCP server \(tools\/list\) and sent 1 call/);
    expect(screen.getByTestId("mcp-badge")).toHaveTextContent("MCP");
  });

  it("'Run again' sends one more request, no more", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    await userEvent.click(await screen.findByRole("button", { name: "Run again" }));
    await waitFor(() => expect(api.count("POST", "/agent")).toBe(2));
    await new Promise((r) => setTimeout(r, 50));
    expect(api.count("POST", "/agent")).toBe(2);
  });

  it("labels deterministic fallback results honestly and explains why", async () => {
    await ready(installFakeApi({ "POST /api/claims/:id/agent": (_c, p) => agentResult(p.id, "deterministic_fallback") }));
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    expect(await screen.findByText("Deterministic")).toBeInTheDocument();
    expect(screen.getByText(/language model is turned off/i)).toBeInTheDocument();
    expect(screen.queryByTestId("mcp-line")).toBeNull(); // no model call, no MCP claim
    expect(screen.queryByTestId("mcp-badge")).toBeNull();
  });

  it("shows an error and allows a retry when the request fails", async () => {
    let fail = true;
    const api = await ready(
      installFakeApi({
        "POST /api/claims/:id/agent": (_c, p) => {
          if (fail) throw new HttpError(429, { error: "agent is busy" });
          return agentResult(p.id);
        },
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    expect(await screen.findByText("agent is busy")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    expect(await screen.findByText(/shares a payout destination/)).toBeInTheDocument();
    expect(api.count("POST", "/agent")).toBe(2);
  });

  it("blocks a second run while another claim's run is still in flight", async () => {
    const hold = deferred();
    const api = await ready(
      installFakeApi({ "POST /api/claims/:id/agent": async (_c, p) => (await hold.promise, agentResult(p.id)) }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    const btn = screen.getByRole("button", { name: "Run investigation agent" });
    expect(btn).toBeDisabled();
    await userEvent.click(btn);
    expect(api.count("POST", "/agent")).toBe(1);
    hold.resolve();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run investigation agent" })).toBeEnabled());
    // the finished run belonged to CLM-1 and must not appear under CLM-2
    expect(screen.queryByText(/Claim CLM-1 shares/)).not.toBeInTheDocument();
  });
});

describe("counterfactual toggles", () => {
  it("excluding a signal posts once, re-scores, requests the filtered graph, and resets cleanly", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Repair shop" }));

    await waitFor(() => expect(api.count("POST", "/counterfactual")).toBe(1));
    expect(api.where("POST", "/counterfactual")[0].body).toEqual({ exclude: "repair_shop", hops: 3 });
    expect(api.where("GET", "/graph").some((c) => c.query.get("exclude") === "repair_shop")).toBe(true);
    expect(await screen.findByText("Without the repair shop the score falls by 8.")).toBeInTheDocument();
    expect(screen.getByTestId("graph")).toHaveAttribute("data-excluded", "repair_shop");
    expect(screen.getByRole("button", { name: "Repair shop" })).toHaveAttribute("aria-pressed", "true");

    // toggling the same chip again resets: no new counterfactual POST, graph reloaded unfiltered
    await userEvent.click(screen.getByRole("button", { name: "Repair shop" }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-excluded", ""));
    expect(api.count("POST", "/counterfactual")).toBe(1);
    expect(screen.queryByText(/falls by 8/)).not.toBeInTheDocument();
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("disables signals that the claim does not have, and sends nothing when they are clicked", async () => {
    const api = await ready(installFakeApi());
    const device = screen.getByRole("button", { name: "Device" });
    expect(device).toBeDisabled();
    await userEvent.click(device);
    expect(api.count("POST", "/counterfactual")).toBe(0);
  });

  it("selecting a new claim clears an active exclusion", async () => {
    await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Repair shop" }));
    await screen.findByText(/falls by 8/);
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    expect(screen.getByTestId("graph")).toHaveAttribute("data-excluded", "");
    expect(screen.queryByText(/falls by 8/)).not.toBeInTheDocument();
  });
});

describe("your decision (under the report) and closing the investigation", () => {
  const runAgent = async () => {
    await userEvent.click(screen.getByRole("button", { name: "Run investigation agent" }));
    await screen.findByText(/shares a payout destination with CLM-2/);
  };

  it("shows Approve, Reject and Need more information only after a report exists", async () => {
    await ready(installFakeApi());
    expect(screen.queryByRole("button", { name: "Approve and close" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Need more information" })).not.toBeInTheDocument();
    expect(screen.getByText(/Run the investigation agent first/)).toBeInTheDocument();
    await runAgent();
    for (const name of ["Approve and close", "Reject and close", "Need more information"]) {
      expect(screen.getByRole("button", { name })).toBeInTheDocument();
    }
  });

  it("approving needs a reason and a second click, sends one POST with the run id, then locks the claim", async () => {
    const api = await ready(installFakeApi());
    await runAgent();
    const approve = screen.getByRole("button", { name: "Approve and close" });
    expect(approve).toBeDisabled(); // no reason yet
    await userEvent.type(screen.getByLabelText("Decision reason"), "Joint account verified with the bank");
    await userEvent.click(approve);
    expect(api.count("POST", "/close")).toBe(0);
    await userEvent.click(screen.getByRole("button", { name: "Confirm: approve and close" }));

    await waitFor(() => expect(api.count("POST", "/close")).toBe(1));
    expect(api.where("POST", "/close")[0].body).toEqual({ run_id: "run-CLM-1", outcome: "approved", reason: "Joint account verified with the bank" });
    const closed = await screen.findByLabelText("Investigation closed");
    expect(closed).toHaveTextContent("Approved by investigator");
    expect(closed).toHaveTextContent("“Joint account verified with the bank”");
    expect(closed).toHaveTextContent(/normal claims processing/);
    // Locked: no agent button and no decision buttons any more.
    expect(screen.queryByRole("button", { name: /Run investigation agent|Run again/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Need more information" })).not.toBeInTheDocument();
    // The queue shows the outcome.
    await waitFor(() => expect(queue().getByText("Approved")).toBeInTheDocument());
  });

  it("rejecting asks once; Cancel sends nothing", async () => {
    const api = await ready(installFakeApi());
    await runAgent();
    await userEvent.type(screen.getByLabelText("Decision reason"), "Payout holder is not the claimant");
    await userEvent.click(screen.getByRole("button", { name: "Reject and close" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(api.count("POST", "/close")).toBe(0);
    await userEvent.click(screen.getByRole("button", { name: "Reject and close" }));
    await userEvent.click(screen.getByRole("button", { name: "Confirm: reject and close" }));
    await waitFor(() => expect(api.count("POST", "/close")).toBe(1));
    expect(api.where("POST", "/close")[0].body.outcome).toBe("rejected");
    expect(await screen.findByLabelText("Investigation closed")).toHaveTextContent("Rejected by investigator");
  });

  it("Need more information records once and keeps the investigation open", async () => {
    const hold = deferred();
    const api = await ready(
      installFakeApi({ "POST /api/claims/:id/disposition": async (c, p) => (await hold.promise, dispositionCase(p.id, c.body.disposition)) }),
    );
    await runAgent();
    await userEvent.type(screen.getByLabelText("Decision reason"), "asked the bank");
    await userEvent.dblClick(screen.getByRole("button", { name: "Need more information" }));
    expect(api.count("POST", "/disposition")).toBe(1);
    hold.resolve();
    expect(await screen.findByText(/Recorded: Needs More Information\. The investigation stays open/)).toBeInTheDocument();
    expect(api.where("POST", "/disposition")[0].body).toEqual({ disposition: "needs_info", notes: "asked the bank" });
    expect(api.count("POST", "/close")).toBe(0);
    expect(screen.getByRole("button", { name: "Approve and close" })).toBeInTheDocument();
  });

  it("a pending confirmation and a typed reason do not carry over to another claim", async () => {
    const api = await ready(installFakeApi());
    await runAgent();
    await userEvent.type(screen.getByLabelText("Decision reason"), "reason for claim 1");
    await userEvent.click(screen.getByRole("button", { name: "Reject and close" }));
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    expect(screen.queryByRole("button", { name: /Confirm:/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Decision reason")).not.toBeInTheDocument(); // no report for CLM-2 yet
    expect(api.count("POST", "/close")).toBe(0);
  });

  it("keeps the reason and shows the error when closing fails", async () => {
    await ready(installFakeApi({ "POST /api/claims/:id/close": () => { throw new HttpError(409, { error: "this report is no longer available to seal" }); } }));
    await runAgent();
    await userEvent.type(screen.getByLabelText("Decision reason"), "keep me");
    await userEvent.click(screen.getByRole("button", { name: "Approve and close" }));
    await userEvent.click(screen.getByRole("button", { name: "Confirm: approve and close" }));
    expect(await screen.findByText("this report is no longer available to seal")).toBeInTheDocument();
    expect(screen.getByLabelText("Decision reason")).toHaveValue("keep me");
    expect(screen.queryByLabelText("Investigation closed")).not.toBeInTheDocument();
  });

  it("a closed claim shows its sealed report on request, fetched once", async () => {
    const api = await ready(
      installFakeApi({ "GET /api/claims/:id": (_c, p) => ({ ...summary(p.id), closure: p.id === "CLM-1" ? closureOf(p.id, "rejected", "confirmed") : undefined }) }),
    );
    expect(await screen.findByLabelText("Investigation closed")).toHaveTextContent("Rejected by investigator");
    expect(screen.queryByRole("button", { name: "Run investigation agent" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "View sealed report" }));
    expect(await screen.findByLabelText("Sealed report")).toHaveTextContent(/shares a payout destination with CLM-2/);
    await userEvent.click(screen.getByRole("button", { name: "Hide sealed report" }));
    await userEvent.click(screen.getByRole("button", { name: "View sealed report" }));
    expect(api.count("GET", "/sealed-report")).toBe(1);
    expect(api.count("POST", "/agent")).toBe(0);
  });
});

describe("FNOL intake", () => {
  it("submits a valid FNOL once, reports the real policy-check result, and opens the new claim", async () => {
    const api = installFakeApi();
    api.handlers["POST /api/fnol"] = (c) => {
      api.setClaims([summary(c.body.claim_id), summary("CLM-1"), summary("CLM-2")]);
      return fnolResult(c.body.claim_id, ["amount_within_limit"]);
    };
    await ready(api);
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));

    expect(await screen.findByText(/Ordinary policy checks flagged a problem/)).toBeInTheDocument();
    // each check the backend ran is listed with its own result
    const checks = within(screen.getByRole("list", { name: "Policy checks" }));
    expect(checks.getByText(/Amount is within the policy payout limit/).closest("li")).toHaveClass("bad");
    expect(checks.getByText(/Policy is active/).closest("li")).toHaveClass("ok");
    const posts = api.where("POST", "/api/fnol");
    expect(posts).toHaveLength(1);
    const f = posts[0].body;
    expect(f.vin).toHaveLength(17);
    expect(f.claim_id).toMatch(/^CLM-9\d{6}$/); // 7 digits: never collides with a seeded claim
    expect(f.payout_account_id).toBe("PAY-01"); // the "looks clean" preset reuses a known account
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", f.claim_id));
    expect(api.count("POST", "/agent")).toBe(0);
  });

  it("shows validation problems from the API and does not select anything", async () => {
    const api = await ready(
      installFakeApi({ "POST /api/fnol": () => { throw new HttpError(400, { error: "invalid FNOL", problems: ["vin must be 17 letters/digits", "amount must be between 1 and 1000000"] }); } }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
    expect(await screen.findByText(/vin must be 17 letters\/digits; amount must be between/)).toBeInTheDocument();
    expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-1");
    expect(api.count("POST", "/api/fnol")).toBe(1);
  });

  it("each preset builds a distinct, well-formed FNOL", async () => {
    const api = await ready(installFakeApi());
    const names = ["Looks clean on its own", "Busy repair shop only", "Vendor settlement account", "Isolated claim"];
    for (const n of names) {
      await userEvent.click(screen.getByRole("button", { name: n }));
      await userEvent.click(screen.getByRole("button", { name: "Submit synthetic FNOL" }));
      await waitFor(() => expect(screen.getByRole("button", { name: "Submit synthetic FNOL" })).toBeEnabled());
      await new Promise((r) => setTimeout(r, 5)); // distinct millisecond => distinct claim id
    }
    const bodies = api.where("POST", "/api/fnol").map((c) => c.body);
    expect(bodies).toHaveLength(4);
    expect(new Set(bodies.map((b) => b.claim_id)).size).toBe(4);
    expect(bodies[2].payout_type).toBe("vendor_direct_settlement");
    expect(bodies[3].payout_account_id).toBe(`PAY-${bodies[3].claim_id}`); // isolated: nothing shared
    for (const b of bodies) expect(b.vin).toMatch(/^[A-Za-z0-9]{17}$/);
  });
});

// keep the unused fixture helpers referenced so a rename breaks loudly
void [counterfactual, graph];

describe("API not running (the Vite proxy answers 502 with a non-JSON body)", () => {
  it("tells the investigator how to start it instead of 'Request failed (502)'", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("Bad Gateway", { status: 502 })));
    render(<App />);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/API is not running/);
    expect(alert).toHaveTextContent(/npm run dev:all/);
  });
});
