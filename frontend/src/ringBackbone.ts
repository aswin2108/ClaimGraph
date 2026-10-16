import type { AlgoLink, AlgoNode } from "./types";

export const algoNodeId = (n: AlgoNode) => `${n.type}:${n.id}`;

/**
 * The part of a ring's spanning tree (algo.MSF) that connects claims. The tree also reaches every identifier
 * that only one claim uses (its own phone, device, ...); such a leaf links nothing, so it is dropped, repeatedly,
 * because a claimant becomes a leaf once its unshared contact is gone. Claims are never dropped.
 * Display only: the API result is unchanged.
 */
export function connectingLinks(backbone: AlgoLink[]): { links: AlgoLink[]; hidden: number } {
  const degree = new Map<string, number>();
  const bump = (id: string, d: number) => degree.set(id, (degree.get(id) ?? 0) + d);
  for (const l of backbone) {
    bump(algoNodeId(l.from), 1);
    bump(algoNodeId(l.to), 1);
  }
  const isClaim = (id: string) => id.startsWith("Claim:");
  const removed = new Set<string>();
  let changed = true;
  while (changed) {
    changed = false;
    for (const l of backbone) {
      const a = algoNodeId(l.from);
      const b = algoNodeId(l.to);
      if (removed.has(a) || removed.has(b)) continue;
      for (const [leaf, other] of [
        [a, b],
        [b, a],
      ]) {
        if (!isClaim(leaf) && degree.get(leaf) === 1) {
          removed.add(leaf);
          bump(other, -1);
          changed = true;
          break;
        }
      }
    }
  }
  return {
    links: backbone.filter((l) => !removed.has(algoNodeId(l.from)) && !removed.has(algoNodeId(l.to))),
    hidden: removed.size,
  };
}
