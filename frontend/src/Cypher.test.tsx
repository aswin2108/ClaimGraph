import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "./App";
import { installFakeApi } from "./test/fakeApi";

// Cytoscape needs a real canvas; both canvases are covered by the browser e2e tests.
vi.mock("./components/GraphPanel", () => ({
  default: (p: { onView: (v: string) => void }) => (
    <div data-testid="graph">
      <button onClick={() => p.onView("ring")}>open-ring</button>
    </div>
  ),
}));
vi.mock("./components/RingGraph", () => ({ default: () => <div data-testid="ring-graph" /> }));

const ready = async () => {
  const api = installFakeApi();
  render(<App />);
  await screen.findByText("High Priority");
  return api;
};

describe("Show Cypher", () => {
  it("shows the traversal's queries and bound parameters on request, without any new request", async () => {
    const api = await ready();
    const toggle = await screen.findByRole("button", { name: "Show Cypher (2 queries)" });
    expect(screen.queryByLabelText("Cypher behind this traversal")).not.toBeInTheDocument();
    const before = api.calls.length;
    await userEvent.click(toggle);
    const box = screen.getByLabelText("Cypher behind this traversal");
    expect(within(box).getByText("Load the target claim")).toBeInTheDocument();
    expect(box).toHaveTextContent("MATCH (c:Claim {claim_id:$claim_id})");
    expect(box).toHaveTextContent('$claim_id = "CLM-1"');
    expect(box).toHaveTextContent("never pasted into the query");
    expect(api.calls.length).toBe(before); // the log came with the investigation
    await userEvent.click(screen.getByRole("button", { name: "Hide Cypher" }));
    expect(screen.queryByLabelText("Cypher behind this traversal")).not.toBeInTheDocument();
  });

  it("shows the ring's algo.WCC call, marked as answered from the cached run", async () => {
    await ready();
    await userEvent.click(screen.getByText("open-ring"));
    await screen.findByTestId("ring-graph");
    await userEvent.click(await screen.findByRole("button", { name: "Show Cypher (1 query)" }));
    const box = screen.getByLabelText("Cypher behind this ring");
    expect(box).toHaveTextContent("CALL algo.WCC");
    expect(box).toHaveTextContent("answered from the cached whole-book run");
    expect(box).toHaveTextContent('$labels = ["Claim","Claimant"]');
  });
});
