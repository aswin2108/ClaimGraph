import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import cytoscape from "cytoscape";
import type { GraphView } from "../types";
import { CATEGORY_COLOR, CATEGORY_LABEL, ENTITY_LABEL, reducedMotion } from "../ui";
import { GRAPH_STYLE as STYLE } from "./graphStyle";
import ViewTabs, { type GraphViewMode } from "./ViewTabs";

interface Props {
  graph: GraphView | null;
  hops: number;
  onHops: (n: number) => void;
  focusClaim: string | null;
  onFocus: (claimId: string | null) => void;
  excluded: string | null;
  busy: boolean;
  ringSize?: number;
  onView: (v: GraphViewMode) => void;
}

interface Hover {
  title: string;
  lines: string[];
}

/** Which traversal step reveals each element: the target is step 0, each further claim-to-claim hop adds one. */
function computeStages(g: GraphView): { stage: Map<string, number>; max: number } {
  const stage = new Map<string, number>();
  const adj = new Map<string, string[]>();
  const link = (a: string, b: string) => {
    if (!adj.has(a)) adj.set(a, []);
    adj.get(a)!.push(b);
  };
  for (const e of g.edges) {
    link(e.source, e.target);
    link(e.target, e.source);
  }
  for (const n of g.nodes) if (n.kind === "claim") stage.set(n.id, n.hops ?? 0);
  // claimants first inherit their claim's step, entities appear with the later claim they join...
  for (const n of g.nodes) {
    if (n.kind !== "claimant") continue;
    const claim = (adj.get(n.id) ?? []).find((x) => x.startsWith("claim:"));
    stage.set(n.id, claim ? (stage.get(claim) ?? 0) : 0);
  }
  for (const n of g.nodes) {
    if (n.kind !== "entity") continue;
    let s = 0;
    for (const x of adj.get(n.id) ?? []) s = Math.max(s, stage.get(x) ?? 0);
    stage.set(n.id, s);
  }
  // ...and a claimant appears together with the shared entity it connects through.
  for (const n of g.nodes) {
    if (n.kind !== "claimant") continue;
    const ents = (adj.get(n.id) ?? []).filter((x) => x.startsWith("entity:")).map((x) => stage.get(x) ?? 0);
    if (ents.length) stage.set(n.id, Math.min(...ents));
  }
  let max = 0;
  for (const v of stage.values()) max = Math.max(max, v);
  return { stage, max };
}


// Growth animation: entities grow out at once, the claims they lead to follow once the entity has landed.
const GROW_MS = 650;
const GROW_DELAY_MS = 520;

const HOP_HELP =
  "A hop is one claim-to-claim link through something two claims share: a payout account, device, phone or email, address, repair shop or IP. " +
  "1 hop = claims that share something with this claim directly. 2 hops = claims linked to those claims, and so on, up to 3. " +
  "More hops reach further, but the score only counts each kind of evidence once.";

export default function GraphPanel({ graph, hops, onHops, focusClaim, onFocus, excluded, busy, ringSize, onView }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const cyRef = useRef<cytoscape.Core | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const home = useRef(new Map<string, cytoscape.Position>()); // each node's laid-out position
  const pending = useRef<number[]>([]); // delayed reveals of the current step
  const [stage, setStage] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [hover, setHover] = useState<Hover | null>(null);

  const stages = useMemo(() => (graph ? computeStages(graph) : { stage: new Map<string, number>(), max: 0 }), [graph]);

  const clearPending = () => {
    pending.current.forEach((t) => window.clearTimeout(t));
    pending.current = [];
  };

  // Stops stepping through hops. The current step's growth finishes (the next reveal, or a new graph, clears it).
  const stop = useCallback(() => {
    window.clearInterval(timer.current);
    setPlaying(false);
  }, []);

  // Show elements up to a traversal step. With grow, each newly reached node grows out of the node it is
  // linked from, so its edge stretches out behind it: entities grow from the claim reached in the previous
  // hop, then the newly reached claims grow from those entities. Without grow (scrubbing, hop changes,
  // reduced motion) everything jumps to its place.
  const reveal = useCallback(
    (s: number, grow: boolean) => {
      const cy = cyRef.current;
      if (!cy) return;
      clearPending();
      cy.elements(".fresh").removeClass("fresh");
      const stageOf = (id: string) => stages.stage.get(id) ?? 0;
      const edgeStage = (e: cytoscape.EdgeSingular) => Math.max(stageOf(e.source().id()), stageOf(e.target().id()));
      const starts: { node: cytoscape.NodeSingular; from?: cytoscape.Position; delay: number }[] = [];
      cy.batch(() => {
        cy.nodes().stop(true, true); // finish any running growth at its final position
        cy.nodes().forEach((n) => {
          const id = n.id();
          const st = stageOf(id);
          const to = home.current.get(id);
          if (st > s) {
            n.addClass("hidden");
            if (to) n.position(to);
          } else if (n.hasClass("hidden")) {
            if (!grow) {
              n.removeClass("hidden");
              if (to) n.position(to);
              return;
            }
            // Grow from a node already on screen; a node linked only to other new nodes waits for them.
            const around = n.neighborhood("node");
            const older = around.filter((x) => stageOf(x.id()) < st);
            const pool = older.nonempty() ? older : around.filter((x) => x.id() !== id);
            const anchor = pool.nonempty() ? pool.min((x) => stageOf(x.id())).ele : undefined;
            starts.push({ node: n, from: anchor ? home.current.get(anchor.id()) : undefined, delay: older.nonempty() ? 0 : GROW_DELAY_MS });
          }
        });
        cy.edges().forEach((e) => {
          if (edgeStage(e) > s) e.addClass("hidden");
          else if (!grow) e.removeClass("hidden");
        });
      });
      for (const { node, from, delay } of starts) {
        const start = () => {
          const to = home.current.get(node.id());
          node.position(from ?? to ?? node.position());
          node.removeClass("hidden").addClass("fresh");
          // its edges appear with it, once the node at the other end is on screen
          node.connectedEdges().forEach((e) => {
            const other = e.source().id() === node.id() ? e.target() : e.source();
            if (edgeStage(e) <= s && !other.hasClass("hidden")) e.removeClass("hidden").addClass("fresh");
          });
          if (to) node.animate({ position: to }, { duration: GROW_MS, easing: "ease-out-cubic" });
          pending.current.push(window.setTimeout(() => node.connectedEdges().union(node).removeClass("fresh"), GROW_MS + 300));
          // (a cancelled removal is covered: every reveal starts by clearing the highlight)
        };
        if (delay === 0) start();
        else pending.current.push(window.setTimeout(start, delay));
      }
    },
    [stages],
  );

  const play = useCallback((animate = true) => {
    stop();
    if (!animate || reducedMotion() || stages.max === 0) {
      setStage(stages.max);
      reveal(stages.max, false);
      return;
    }
    let s = 0;
    setStage(0);
    reveal(0, false);
    setPlaying(true);
    timer.current = window.setInterval(() => {
      s += 1;
      setStage(s);
      reveal(s, true);
      if (s >= stages.max) stop();
    }, 1300);
  }, [reveal, stages.max, stop]);

  // Build the Cytoscape instance whenever the evidence graph changes, then play the traversal once.
  useEffect(() => {
    stop();
    clearPending();
    cyRef.current?.destroy();
    cyRef.current = null;
    if (!graph || !box.current) return;

    const elements: cytoscape.ElementDefinition[] = [
      ...graph.nodes.map((n) => ({
        group: "nodes" as const,
        data: {
          id: n.id,
          kind: n.kind,
          label: n.label,
          target: n.target ? true : undefined,
          context: n.context ? true : undefined,
          color: CATEGORY_COLOR[n.category ?? ""] ?? "#94a3b8",
          display: n.kind === "entity" ? `${ENTITY_LABEL[n.type ?? ""] ?? n.type}\n${n.label}` : n.label,
          type: n.type,
          category: n.category,
        },
      })),
      ...graph.edges.map((e) => ({
        group: "edges" as const,
        data: {
          id: e.id,
          source: e.source,
          target: e.target,
          rel: e.rel,
          color: CATEGORY_COLOR[e.category ?? ""] ?? "#94a3b8",
          context: e.context ? true : undefined,
          evidence: e.evidence_id,
          at: e.recorded_at,
        },
      })),
    ];

    const cy = cytoscape({
      container: box.current,
      elements,
      style: STYLE,
      layout: {
        name: "breadthfirst",
        roots: [`#${CSS.escape(`claim:${graph.target_claim}`)}`],
        circle: true,
        spacingFactor: 1.05,
        padding: 24,
      } as cytoscape.LayoutOptions,
      minZoom: 0.3,
      maxZoom: 2.5,
      wheelSensitivity: 0.25,
    });
    cyRef.current = cy;
    home.current = new Map(cy.nodes().map((n) => [n.id(), { ...n.position() }] as [string, cytoscape.Position]));
    if (import.meta.env.DEV) (window as unknown as { __cy?: cytoscape.Core }).__cy = cy; // handle for the browser e2e tests

    cy.on("mouseover", "edge", (ev) => {
      const d = ev.target.data();
      setHover({ title: d.rel, lines: [d.evidence ? `Evidence ${d.evidence}` : "", d.at ? `Recorded ${d.at}` : ""].filter(Boolean) });
    });
    cy.on("mouseover", "node", (ev) => {
      const d = ev.target.data();
      if (d.kind === "entity") setHover({ title: `${d.type} ${d.label}`, lines: [CATEGORY_LABEL[d.category] ?? "", d.context ? "Context only — not scored" : ""].filter(Boolean) });
      else if (d.kind === "claim") setHover({ title: `Claim ${d.label}`, lines: ["Click to trace its evidence path"] });
      else setHover({ title: `Claimant ${d.label}`, lines: [] });
    });
    cy.on("mouseout", () => setHover(null));
    cy.on("tap", "node[kind = 'claim']", (ev) => onFocus(ev.target.data("label")));
    cy.on("tap", (ev) => {
      if (ev.target === cy) onFocus(null);
    });

    const ro = new ResizeObserver(() => cy.resize());
    ro.observe(box.current);
    return () => {
      ro.disconnect();
      clearPending(); // delayed reveals must never touch a destroyed graph
      cy.destroy();
      if (cyRef.current === cy) cyRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph]);

  // Replay the traversal only for a newly selected claim; hop or counterfactual changes show the result at once.
  const lastTarget = useRef<string | null>(null);
  useEffect(() => {
    if (graph) {
      play(graph.target_claim !== lastTarget.current);
      lastTarget.current = graph.target_claim;
    }
    return stop;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph]);

  // Highlight the shortest evidence path from the target to a chosen related claim.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy || !graph) return;
    cy.elements().removeClass("dim onpath");
    if (!focusClaim) return;
    const goal = cy.getElementById(`claim:${focusClaim}`);
    const root = cy.getElementById(`claim:${graph.target_claim}`);
    if (goal.empty() || root.empty()) return;
    const found = cy.elements().aStar({ root, goal, directed: false });
    if (!found.found) return;
    cy.elements().addClass("dim");
    found.path.removeClass("dim").addClass("onpath");
  }, [focusClaim, graph]);

  const caption = useMemo(() => {
    if (!graph) return "";
    if (stage === 0) return `Start: target claim ${graph.target_claim}`;
    const claims = graph.nodes.filter((n) => n.kind === "claim" && (n.hops ?? 0) === stage).map((n) => n.label);
    const ents = graph.nodes
      .filter((n) => n.kind === "entity" && !n.context && stages.stage.get(n.id) === stage)
      .map((n) => `${ENTITY_LABEL[n.type ?? ""] ?? n.type} ${n.label}`);
    return `Hop ${stage}: ${claims.length} claim${claims.length === 1 ? "" : "s"} reached${claims.length ? ` (${claims.join(", ")})` : ""}${ents.length ? ` via ${ents.join(", ")}` : ""}`;
  }, [graph, stage, stages]);

  const scrub = (v: number) => {
    stop();
    setStage(v);
    reveal(v, false);
  };

  const legend = useMemo(() => {
    if (!graph) return [];
    const seen = new Set<string>();
    for (const n of graph.nodes) if (n.kind === "entity" && n.category) seen.add(n.category);
    return [...seen];
  }, [graph]);

  const empty = graph && graph.nodes.length <= 1;

  return (
    <section className="card graph-card">
      <header className="card-head">
        <h2>Evidence graph</h2>
        <div className="head-tools">
          {excluded && <span className="chip chip-warn">Ignoring {CATEGORY_LABEL[excluded] ?? excluded}</span>}
          <span className="hint" tabIndex={0} aria-label={HOP_HELP}>
            ?<span className="hint-pop" role="tooltip">{HOP_HELP}</span>
          </span>
          <label className="select">
            <span>Max hops</span>
            <select value={hops} onChange={(e) => onHops(Number(e.target.value))}>
              {[1, 2, 3].map((h) => (
                <option key={h} value={h}>
                  {h}
                </option>
              ))}
            </select>
          </label>
          <ViewTabs view="traversal" onView={onView} hops={hops} ringSize={ringSize} />
        </div>
      </header>

      <div className="graph-stage">
        <div ref={box} className="cy" aria-label="Interactive evidence graph" />
        {busy && <div className="overlay-msg">Querying FalkorDB…</div>}
        {empty && !busy && (
          <div className="overlay-msg">
            No connected claims found. No relationships exist in the graph that link this claim to others.
          </div>
        )}
        {hover && (
          <div className="hover-card">
            <strong>{hover.title}</strong>
            {hover.lines.map((l) => (
              <div key={l}>{l}</div>
            ))}
          </div>
        )}
        {graph?.truncated && <div className="trunc">Some results were cut off by traversal limits.</div>}
      </div>

      <footer className="graph-foot">
        <div className="replay">
          {/* A replay shows the whole traversal, so it first clears a highlighted path. */}
          <button className="btn btn-ghost" onClick={() => (playing ? stop() : (onFocus(null), play(true)))} disabled={!graph || stages.max === 0}>
            {playing ? "Pause" : "▶ Replay traversal"}
          </button>
          <input
            type="range"
            min={0}
            max={stages.max}
            value={stage}
            onChange={(e) => scrub(Number(e.target.value))}
            disabled={!graph || stages.max === 0}
            aria-label="Traversal step"
          />
          <span className="caption">{caption}</span>
        </div>
        <div className="legend">
          {legend.map((c) => (
            <span key={c} className="legend-item">
              <i style={{ background: CATEGORY_COLOR[c] ?? "#94a3b8" }} />
              {CATEGORY_LABEL[c] ?? c}
            </span>
          ))}
        </div>
      </footer>
    </section>
  );
}
