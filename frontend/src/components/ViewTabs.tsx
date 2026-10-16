export type GraphViewMode = "traversal" | "ring";

interface Props {
  view: GraphViewMode;
  onView: (v: GraphViewMode) => void;
  hops: number;
  ringSize?: number; // undefined while the ring is unknown (loading or failed)
}

/** Switches the graph card between the bounded traversal and the algorithm-found ring. */
export default function ViewTabs({ view, onView, hops, ringSize }: Props) {
  const ringLabel = ringSize === undefined ? "Ring" : ringSize > 1 ? `Ring · ${ringSize} claims` : "Ring · none";
  return (
    <div className="tabs" role="tablist" aria-label="Graph view">
      <button role="tab" aria-selected={view === "traversal"} className={view === "traversal" ? "on" : ""} onClick={() => onView("traversal")}>
        Traversal ≤ {hops} hop{hops === 1 ? "" : "s"}
      </button>
      <button role="tab" aria-selected={view === "ring"} className={view === "ring" ? "on" : ""} onClick={() => onView("ring")}>
        {ringLabel}
      </button>
    </div>
  );
}
