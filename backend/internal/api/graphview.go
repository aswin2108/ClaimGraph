package api

import (
	"sort"

	"claimgraph/internal/claims"
	"claimgraph/internal/investigate"
)

// GraphNode and GraphEdge are shaped for direct use as Cytoscape.js elements.
type GraphNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // claim | claimant | entity
	Label    string `json:"label"`
	Type     string `json:"type,omitempty"`     // entity type, e.g. PayoutDestination
	Category string `json:"category,omitempty"` // evidence category (payout, device, ...)
	Target   bool   `json:"target,omitempty"`   // the claim under investigation
	Hops     *int   `json:"hops,omitempty"`     // claim-to-claim distance from the target
	Context  bool   `json:"context,omitempty"`  // shown for context only, not part of any evidence path
	// Infrastructure marks a shared identifier used by many claims (a busy repair shop, a carrier IP)
	// or a context-only one: drawn once, attached to the target only; its other users are not
	// connections and are never drawn. OtherClaims is its exact usage count from the graph.
	Infrastructure bool `json:"infrastructure,omitempty"`
	OtherClaims    int  `json:"other_claims,omitempty"`
}

type GraphEdge struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Rel        string `json:"rel"`
	Category   string `json:"category,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
	Context    bool   `json:"context,omitempty"`
}

// GraphView is the evidence graph for one investigation.
type GraphView struct {
	TargetClaim      string      `json:"target_claim"`
	ExcludedCategory string      `json:"excluded_category,omitempty"`
	Nodes            []GraphNode `json:"nodes"`
	Edges            []GraphEdge `json:"edges"`
	Truncated        bool        `json:"truncated"`
}

// BuildGraphView converts an investigation result into graph elements. Every node
// and edge comes from a returned evidence path, the target's shared-infrastructure
// summary or a stored tow-provider context link; nothing is invented. tows maps claim ID -> tow provider (context only).
func BuildGraphView(res *investigate.Result, tows map[string]claims.Entity) *GraphView {
	v := &GraphView{TargetClaim: res.TargetClaim, ExcludedCategory: res.ExcludedCategory,
		Truncated: res.Truncated, Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	nodes := map[string]GraphNode{}
	edges := map[string]GraphEdge{}

	claimNode := func(id string, hops int, target bool) {
		n := nodes["claim:"+id]
		n.ID, n.Kind, n.Label, n.Target = "claim:"+id, "claim", id, target
		h := hops
		n.Hops = &h
		nodes[n.ID] = n
	}
	addEdge := func(src, dst, rel, cat, evid, at string) {
		id := src + "|" + rel + "|" + dst
		edges[id] = GraphEdge{ID: id, Source: src, Target: dst, Rel: rel, Category: cat, EvidenceID: evid, RecordedAt: at}
	}

	claimNode(res.TargetClaim, 0, true)
	for _, rc := range res.RelatedClaims {
		claimNode(rc.ClaimID, rc.Hops, false)
		for _, p := range rc.Paths {
			for _, st := range p.Steps {
				cat := investigate.SignalCategory(st)
				ent := "entity:" + st.EntityType + ":" + st.EntityID
				nodes[ent] = GraphNode{ID: ent, Kind: "entity", Label: st.EntityID, Type: st.EntityType, Category: cat}
				claimNode(st.FromClaim, hopsOf(res, st.FromClaim), st.FromClaim == res.TargetClaim)
				claimNode(st.ToClaim, hopsOf(res, st.ToClaim), false)

				if st.FromClaimant != "" { // contact/address evidence passes through the claimants
					a, b := "claimant:"+st.FromClaimant, "claimant:"+st.ToClaimant
					nodes[a] = GraphNode{ID: a, Kind: "claimant", Label: st.FromClaimant}
					nodes[b] = GraphNode{ID: b, Kind: "claimant", Label: st.ToClaimant}
					addEdge("claim:"+st.FromClaim, a, "FILED_BY", cat, "", "")
					addEdge(a, ent, st.FromRel, cat, st.FromEvidence, st.FromAt)
					addEdge(b, ent, st.ToRel, cat, st.ToEvidence, st.ToAt)
					addEdge("claim:"+st.ToClaim, b, "FILED_BY", cat, "", "")
				} else {
					addEdge("claim:"+st.FromClaim, ent, st.FromRel, cat, st.FromEvidence, st.FromAt)
					addEdge("claim:"+st.ToClaim, ent, st.ToRel, cat, st.ToEvidence, st.ToAt)
				}
			}
		}
	}

	// Shared infrastructure the target uses (investigate.Result.InfrastructureLinks): one node per
	// identifier, linked to the target only, with its exact usage count. A busy repair shop still
	// counts once in the score, so it is shown; its hundreds of other customers are not.
	for _, l := range res.InfrastructureLinks {
		ent := "entity:" + l.EntityType + ":" + l.EntityID
		if _, onPath := nodes[ent]; !onPath {
			nodes[ent] = GraphNode{ID: ent, Kind: "entity", Label: l.EntityID, Type: l.EntityType, Category: l.Category,
				Context: true, Infrastructure: true, OtherClaims: l.OtherClaims}
		}
		id := "claim:" + res.TargetClaim + "|" + l.Rel + "|" + ent
		edges[id] = GraphEdge{ID: id, Source: "claim:" + res.TargetClaim, Target: ent, Rel: l.Rel, Category: l.Category,
			EvidenceID: l.EvidenceID, RecordedAt: l.RecordedAt, Context: true}
	}

	// Context-only tow providers for the claims shown (never part of scoring).
	for claim, t := range tows {
		if _, shown := nodes["claim:"+claim]; !shown || t.ID == "" {
			continue
		}
		tid := "entity:TowProvider:" + t.ID
		nodes[tid] = GraphNode{ID: tid, Kind: "entity", Label: t.ID, Type: "TowProvider", Category: investigate.CatTowProvider, Context: true}
		id := "claim:" + claim + "|TOWED_BY|" + tid
		edges[id] = GraphEdge{ID: id, Source: "claim:" + claim, Target: tid, Rel: "TOWED_BY", Category: investigate.CatTowProvider, Context: true}
	}

	for _, n := range nodes {
		v.Nodes = append(v.Nodes, n)
	}
	for _, e := range edges {
		v.Edges = append(v.Edges, e)
	}
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].ID < v.Nodes[j].ID })
	sort.Slice(v.Edges, func(i, j int) bool { return v.Edges[i].ID < v.Edges[j].ID })
	return v
}

func hopsOf(res *investigate.Result, claim string) int {
	if claim == res.TargetClaim {
		return 0
	}
	for _, rc := range res.RelatedClaims {
		if rc.ClaimID == claim {
			return rc.Hops
		}
	}
	return 0
}
