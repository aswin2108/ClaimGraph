import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import {
  agentResult, counterfactual, deferred, dispositionCase, fnolResult, graph, HttpError, installFakeApi, reportA, summary,
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
    expect(screen.getByText(/Agent is reading the graph/)).toBeInTheDocument();
    hold.resolve();
    expect(await screen.findByText(/shares a payout destination with CLM-2/)).toBeInTheDocument();
    expect(api.count("POST", "/agent")).toBe(1);
    expect(api.where("POST", "/agent")[0].body).toEqual({ hops: 3 });
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

describe("record disposition", () => {
  it("records a normal outcome with one POST containing the notes, then clears them", async () => {
    const api = await ready(installFakeApi());
    const notes = screen.getByLabelText("Disposition notes");
    await userEvent.type(notes, "checked the account holder");
    await userEvent.click(screen.getByRole("button", { name: "No Fraud Found" }));

    await waitFor(() => expect(api.count("POST", "/disposition")).toBe(1));
    expect(api.where("POST", "/disposition")[0].body).toEqual({ disposition: "not_substantiated", notes: "checked the account holder" });
    expect(await screen.findByText(/Recorded: No Fraud Found/)).toBeInTheDocument();
    expect(notes).toHaveValue("");
    expect(api.count("GET", /\/api\/claims$/)).toBeGreaterThanOrEqual(2); // queue refreshed
  });

  it("'Confirmed Fraud' needs a second, explicit click and Cancel sends nothing", async () => {
    const api = await ready(installFakeApi());
    await userEvent.click(screen.getByRole("button", { name: "Confirmed Fraud" }));
    expect(api.count("POST", "/disposition")).toBe(0);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Confirmed Fraud" })).toBeInTheDocument();
    expect(api.count("POST", "/disposition")).toBe(0);

    await userEvent.click(screen.getByRole("button", { name: "Confirmed Fraud" }));
    await userEvent.click(screen.getByRole("button", { name: "Click again to confirm" }));
    await waitFor(() => expect(api.count("POST", "/disposition")).toBe(1));
    expect(api.where("POST", "/disposition")[0].body.disposition).toBe("confirmed_fraud");
  });

  it("a pending confirmation does not carry over to another claim, and neither do typed notes", async () => {
    const api = await ready(installFakeApi());
    await userEvent.type(screen.getByLabelText("Disposition notes"), "note for claim 1");
    await userEvent.click(screen.getByRole("button", { name: "Confirmed Fraud" }));
    await userEvent.click(queue().getByRole("button", { name: /CLM-2/ }));
    await waitFor(() => expect(screen.getByTestId("graph")).toHaveAttribute("data-target", "CLM-2"));
    expect(screen.getByLabelText("Disposition notes")).toHaveValue("");
    expect(screen.queryByRole("button", { name: "Click again to confirm" })).not.toBeInTheDocument();
    expect(api.count("POST", "/disposition")).toBe(0);
  });

  it("a double click on a normal outcome records once (button is disabled while in flight)", async () => {
    const hold = deferred();
    const api = await ready(
      installFakeApi({ "POST /api/claims/:id/disposition": async (c, p) => (await hold.promise, dispositionCase(p.id, c.body.disposition)) }),
    );
    const btn = screen.getByRole("button", { name: "Needs More Information" });
    await userEvent.dblClick(btn);
    expect(api.count("POST", "/disposition")).toBe(1);
    hold.resolve();
    await screen.findByText(/Recorded: Needs More Information/);
  });

  it("keeps the notes and shows the error when recording fails", async () => {
    await ready(installFakeApi({ "POST /api/claims/:id/disposition": () => { throw new HttpError(400, { error: "notes too long" }); } }));
    await userEvent.type(screen.getByLabelText("Disposition notes"), "keep me");
    await userEvent.click(screen.getByRole("button", { name: "No Fraud Found" }));
    expect(await screen.findByText("notes too long")).toBeInTheDocument();
    expect(screen.getByLabelText("Disposition notes")).toHaveValue("keep me");
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
