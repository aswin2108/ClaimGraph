package graphalgo

import (
	"fmt"
	"strings"
	"time"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	// MaxLinkEdges bounds the strongest-link search: 64 relationships = 16 claimant-level
	// (4-relationship) transitions or 32 claim-level ones. Strong-only edges keep it fast.
	MaxLinkEdges = 64

	// cypherStrongestLink: weighted shortest path (Dijkstra) between two claims over every
	// evidence relationship, in both directions, minimizing the total evidence cost `w`.
	// costProp/maxCost exclude any path that would rely on context-only evidence (vendor
	// settlement accounts, public devices, private IPs cost 1000).
	cypherStrongestLink = `
		MATCH (a:Claim {claim_id:$from}), (b:Claim {claim_id:$to})
		CALL algo.SPpaths({sourceNode: a, targetNode: b,
		                   relTypes: $rels,
		                   relDirection: 'both', weightProp: 'w', costProp: 'w', maxCost: $max_cost, maxLen: $max_len})
		YIELD path, pathWeight
		RETURN nodes(path) AS nodes, relationships(path) AS edges, pathWeight AS cost`
)

// StrongestLink is the lowest-cost evidence chain between two claims.
type StrongestLink struct {
	FromClaim   string   `json:"from_claim"`
	ToClaim     string   `json:"to_claim"`
	Algorithm   string   `json:"algorithm"`
	Found       bool     `json:"found"`
	TotalCost   int      `json:"total_evidence_cost,omitempty"`
	Claims      []string `json:"claims_on_path,omitempty"` // in path order, both ends included
	Links       []Link   `json:"links"`
	Evidence    []string `json:"evidence_used,omitempty"` // shared entities on the path, in order
	Description string   `json:"description"`
	Note        string   `json:"note"`
	DurationMs  int64    `json:"duration_ms"`
}

const linkNote = "Only strong evidence is used (claimant payout account, private device, phone/email, address, same claimant); repair shops, IP addresses and context-only identifiers are not. Stronger evidence costs less: payout 4, device 5, contact 8, address 10 per relationship; the same claimant costs 0. A link explains how claims connect; it is not proof of wrongdoing."

// FindStrongestLink returns the strongest evidence chain between two claims, at any
// depth up to MaxLinkEdges relationships.
func FindStrongestLink(g *falkordb.Graph, from, to string) (*StrongestLink, error) {
	start := time.Now()
	if !ingest.ValidID(from) || !ingest.ValidID(to) {
		return nil, fmt.Errorf("invalid claim id")
	}
	if from == to {
		return nil, fmt.Errorf("the two claims must be different")
	}
	for _, id := range []string{from, to} {
		if err := requireClaim(g, id); err != nil {
			return nil, err
		}
	}
	if err := CheckOverlay(g); err != nil {
		return nil, err
	}
	out := &StrongestLink{FromClaim: from, ToClaim: to, Algorithm: "algo.SPpaths", Links: []Link{}, Note: linkNote}
	sc, err := presentSchema(g, []string{"Claim"}, linkRels)
	if err != nil {
		return nil, err
	}
	found := false
	var r *falkordb.QueryResult
	if len(sc.rels) > 0 {
		r, err = roQuery(g, cypherStrongestLink, map[string]any{"from": from, "to": to, "rels": sc.rels,
			"max_cost": ingest.MaxStrongPathCost, "max_len": MaxLinkEdges})
		if err != nil {
			return nil, err
		}
		found = r.Next()
	}
	if !found {
		out.Description = "No chain of strong evidence connects these claims (weak links such as a shared repair shop are not used here)."
		out.DurationMs = time.Since(start).Milliseconds()
		return out, nil
	}
	rec := r.Record()
	nodes, _ := get(rec, "nodes").([]interface{})
	edges, _ := get(rec, "edges").([]interface{})
	out.Found, out.TotalCost = true, int(num(get(rec, "cost")))

	path := make([]GraphNode, 0, len(nodes))
	for _, v := range nodes {
		if n, ok := v.(*falkordb.Node); ok {
			path = append(path, nodeOf(n))
		}
	}
	var parts []string
	for i, v := range edges {
		e, ok := v.(*falkordb.Edge)
		if !ok || i+1 >= len(path) {
			continue
		}
		l := Link{From: path[i], Rel: e.Relation, To: path[i+1], Cost: int(num(e.Properties[ingest.CostProperty]))}
		out.Links = append(out.Links, l)
	}
	for i, n := range path {
		label := n.Type + "(" + n.ID + ")"
		if n.Type == "Claim" {
			label = n.ID
			out.Claims = append(out.Claims, n.ID)
		} else if n.Type != "Claimant" || sameClaimantBridge(path, i) {
			out.Evidence = append(out.Evidence, label)
		}
		parts = append(parts, label)
	}
	out.Description = strings.Join(parts, " - ")
	out.DurationMs = time.Since(start).Milliseconds()
	return out, nil
}

// sameClaimantBridge reports whether a Claimant node on the path links two claims
// directly (the same person filed both), which is evidence in its own right; a claimant
// next to a contact or address is only the route to that shared identifier.
func sameClaimantBridge(path []GraphNode, i int) bool {
	return i > 0 && i+1 < len(path) && path[i-1].Type == "Claim" && path[i+1].Type == "Claim"
}
