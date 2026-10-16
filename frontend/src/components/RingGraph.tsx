import { useEffect, useRef, useState } from "react";
import cytoscape from "cytoscape";
import type { AlgoLink, AlgoNode, Ring, StrongestLink } from "../types";
import { ALGO_CATEGORY, CATEGORY_COLOR, CATEGORY_LABEL, ENTITY_LABEL } from "../ui";
import { GRAPH_STYLE } from "./graphStyle";
import { algoNodeId as nodeId, connectingLinks } from "../ringBackbone";

interface Props {
  ring: Ring;
  link: StrongestLink | null; // strongest evidence chain to highlight
  onPick: (claimId: string | null) => void;
}

// Algorithm edges carry no reliable direction (MSF is undirected), so an edge is keyed by its ends in sorted order.
const edgeId = (l: AlgoLink) => [l.rel, ...[nodeId(l.from), nodeId(l.to)].sort()].join("|");

function nodeDef(n: AlgoNode, target: string): cytoscape.ElementDefinition {
  const kind = n.type === "Claim" ? "claim" : n.type === "Claimant" ? "claimant" : "entity";
  const category = ALGO_CATEGORY[n.type];
  return {
    group: "nodes",
    data: {
      id: nodeId(n),
      kind,
      label: n.id,
      target: kind === "claim" && n.id === target ? true : undefined,
      color: CATEGORY_COLOR[category ?? ""] ?? "#94a3b8",
      display: kind === "entity" ? `${ENTITY_LABEL[n.type] ?? n.type}\n${n.id}` : n.id,
      type: n.type,
      category,
    },
  };
}

function edgeDef(l: AlgoLink): cytoscape.ElementDefinition {
  const ent = l.from.type === "Claim" || l.from.type === "Claimant" ? l.to : l.from;
  return {
    group: "edges",
    data: {
      id: edgeId(l),
      source: nodeId(l.from),
      target: nodeId(l.to),
      rel: l.rel,
      cost: l.evidence_cost,
      color: CATEGORY_COLOR[ALGO_CATEGORY[ent.type] ?? ""] ?? "#94a3b8",
    },
  };
}

/** The ring's minimum evidence backbone (algo.MSF), with the strongest link (algo.SPpaths) highlighted on demand. */
export default function RingGraph({ ring, link, onPick }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const cyRef = useRef<cytoscape.Core | null>(null);
  const [hover, setHover] = useState<string | null>(null);
  const [hidden, setHidden] = useState(0);

  useEffect(() => {
    if (!box.current) return;
    const seen = new Set<string>();
    const elements: cytoscape.ElementDefinition[] = [];
    const shown = connectingLinks(ring.backbone);
    setHidden(shown.hidden);
    for (const l of shown.links) {
      for (const n of [l.from, l.to]) {
        if (!seen.has(nodeId(n))) {
          seen.add(nodeId(n));
          elements.push(nodeDef(n, ring.target_claim));
        }
      }
      if (!seen.has(edgeId(l))) {
        seen.add(edgeId(l));
        elements.push(edgeDef(l));
      }
    }
    const root = `Claim:${ring.target_claim}`;
    const cy = cytoscape({
      container: box.current,
      elements,
      style: GRAPH_STYLE,
      layout: { name: "preset" },
      minZoom: 0.25,
      maxZoom: 2.5,
      wheelSensitivity: 0.25,
    });
    // A spanning tree can be a long chain, which a radial layout piles up. Place the nodes on an ellipse that fills the
    // canvas, in depth-first order from the target (so the tree's branches do not cross), then let the force-directed
    // layout relax it inside the canvas. Deterministic: the same ring is always drawn the same way.
    const w = Math.max(box.current.clientWidth, 320);
    const h = Math.max(box.current.clientHeight, 240);
    const order: string[] = [];
    const start = cy.getElementById(root).nonempty() ? cy.getElementById(root) : cy.nodes().first();
    cy.elements().dfs({ root: start, visit: (v) => void order.push(v.id()) });
    for (const n of cy.nodes()) if (!order.includes(n.id())) order.push(n.id()); // defensive: a forest
    order.forEach((id, i) => {
      const t = (2 * Math.PI * i) / order.length - Math.PI / 2;
      cy.getElementById(id).position({ x: w / 2 + (w / 2 - 60) * Math.cos(t), y: h / 2 + (h / 2 - 50) * Math.sin(t) });
    });
    cy.layout({
      name: "cose",
      randomize: false,
      animate: false,
      boundingBox: { x1: 0, y1: 0, w, h },
      nodeRepulsion: () => 6000,
      idealEdgeLength: () => 40,
      edgeElasticity: () => 100,
      gravity: 0.25,
      numIter: 1200,
      padding: 20,
    } as cytoscape.LayoutOptions).run();
    cy.fit(undefined, 20);
    cyRef.current = cy;
    if (import.meta.env.DEV) (window as unknown as { __ringCy?: cytoscape.Core }).__ringCy = cy; // handle for the browser e2e tests

    cy.on("mouseover", "node", (ev) => {
      const d = ev.target.data();
      if (d.kind === "claim") setHover(d.target ? `Target claim ${d.label}` : `Claim ${d.label} — click for the strongest link`);
      else if (d.kind === "entity") setHover(`${CATEGORY_LABEL[d.category] ?? d.type} ${d.label}`);
      else setHover(`Claimant ${d.label}`);
    });
    cy.on("mouseover", "edge", (ev) => setHover(`${ev.target.data("rel")} · evidence cost ${ev.target.data("cost")}`));
    cy.on("mouseout", () => setHover(null));
    cy.on("tap", "node[kind = 'claim']", (ev) => onPick(ev.target.data("target") ? null : ev.target.data("label")));
    cy.on("tap", (ev) => {
      if (ev.target === cy) onPick(null);
    });

    const ro = new ResizeObserver(() => cy.resize());
    ro.observe(box.current);
    return () => {
      ro.disconnect();
      cy.destroy();
      if (cyRef.current === cy) cyRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ring]);

  // Highlight the strongest link. Its path can use evidence the backbone left out (a spanning tree keeps one route
  // per pair); those elements are added dashed, next to a node already on screen.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.remove(".link-extra");
    cy.elements().removeClass("dim onpath");
    if (!link?.found || !link.links?.length) return;
    const onPath = cy.collection();
    cy.batch(() => {
      for (const l of link.links!) {
        for (const [n, near] of [
          [l.from, l.to],
          [l.to, l.from],
        ]) {
          let el = cy.getElementById(nodeId(n));
          if (el.empty()) {
            const anchor = cy.getElementById(nodeId(near));
            const p = anchor.nonempty() ? anchor.position() : { x: 0, y: 0 };
            el = cy.add({ ...nodeDef(n, ring.target_claim), position: { x: p.x + 60, y: p.y + 40 }, classes: "link-extra" });
          }
          onPath.merge(el);
        }
        let e = cy.getElementById(edgeId(l));
        if (e.empty()) e = cy.add({ ...edgeDef(l), classes: "link-extra" });
        onPath.merge(e);
      }
      cy.elements().addClass("dim");
      onPath.removeClass("dim").addClass("onpath");
    });
  }, [link, ring]);

  return (
    <>
      <div ref={box} className="cy" aria-label="Ring evidence backbone" />
      {hidden > 0 && (
        <div className="ring-hidden">
          {hidden} identifier{hidden === 1 ? "" : "s"} used by only one claim not drawn
        </div>
      )}
      {hover && (
        <div className="hover-card">
          <strong>{hover}</strong>
        </div>
      )}
    </>
  );
}
