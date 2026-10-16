import { describe, expect, it } from "vitest";
import { connectingLinks } from "./ringBackbone";
import type { AlgoLink } from "./types";

const l = (fromType: string, from: string, rel: string, toType: string, to: string): AlgoLink => ({
  from: { type: fromType, id: from }, rel, to: { type: toType, id: to }, evidence_cost: 1,
});

describe("connectingLinks (ring drawing)", () => {
  it("keeps every link between claims and drops identifiers only one claim uses, repeatedly", () => {
    const backbone = [
      l("Claim", "C1", "PAID_TO", "PayoutDestination", "P1"), // shared by C1 and C2: kept
      l("Claim", "C2", "PAID_TO", "PayoutDestination", "P1"),
      l("Claim", "C2", "FILED_BY", "Claimant", "K2"), // K2 -> contact X2, used by nobody else: both dropped
      l("Claimant", "K2", "HAS_CONTACT", "ContactPoint", "X2"),
      l("Claim", "C3", "USED_DEVICE", "Device", "D1"), // shared by C2 and C3: kept
      l("Claim", "C2", "USED_DEVICE", "Device", "D1"),
      l("Claim", "C3", "USED_DEVICE", "Device", "D9"), // unshared leaf
    ];
    const { links, hidden } = connectingLinks(backbone);
    expect(hidden).toBe(3);
    expect(links).toEqual([backbone[0], backbone[1], backbone[4], backbone[5]]);
  });

  it("never drops a claim, even one hanging at the end of the tree", () => {
    const backbone = [l("Claim", "C1", "PAID_TO", "PayoutDestination", "P1"), l("Claim", "C2", "PAID_TO", "PayoutDestination", "P1")];
    expect(connectingLinks(backbone)).toEqual({ links: backbone, hidden: 0 });
  });

  it("keeps a claimant that links two claims (the same person filed both)", () => {
    const backbone = [l("Claim", "C1", "FILED_BY", "Claimant", "K"), l("Claim", "C2", "FILED_BY", "Claimant", "K")];
    expect(connectingLinks(backbone).links).toHaveLength(2);
  });
});
