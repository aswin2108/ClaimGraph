package investigate

// REFERENCE IMPLEMENTATION - NOT USED IN PRODUCTION.
//
// It predates the infrastructure rule (InfrastructureMinClaims) and does not apply it, so
// the engines are only compared on graphs where no identifier has more than that many users.
// It does apply the context-only rule (ContextOnly): those identifiers never connect claims.
//
// This is the original Go-driven traversal: Go chains up to MaxHops one-transition
// Cypher queries and does the breadth-first search itself. Production now lets
// FalkorDB do the multi-hop traversal (see traverse.go). This copy is kept only as an
// independent oracle: the evaluation harness runs both engines on every fixture and
// fails if they disagree on related claims, hop counts or evidence paths. Retire it
// (delete this file and the parity checks) once the new engine has run clean for a while.

import (
	"fmt"
	"sort"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	maxPartialPerClaim = 50  // candidate paths built per claim before trimming (bounds fan-out)
	rowLimitPerHop     = 500 // rows fetched per branch per hop

	// cypherOneHop returns every claim-to-claim transition through ONE shared entity,
	// starting from any claim in $ids (claim-level entities, then claimant-level
	// contact/address entities between different claimants).
	cypherOneHop = `
		MATCH (a:Claim)-[r1:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]->(e)<-[r2:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]-(b:Claim)
		WHERE a.claim_id IN $ids AND b.claim_id <> a.claim_id AND NOT (a)-[:FILED_BY]->()<-[:FILED_BY]-(b)
		RETURN a.claim_id AS from_claim, b.claim_id AS to_claim,
		       null AS from_claimant, null AS to_claimant,
		       labels(e)[0] AS entity_type,
		       coalesce(e.account_id, e.device_id, e.ip, e.shop_id) AS entity_id,
		       e.type AS entity_subtype, e.is_public AS entity_public, indegree(e) AS entity_degree,
		       type(r1) AS from_rel, type(r2) AS to_rel,
		       r1.evidence_id AS from_evidence, r2.evidence_id AS to_evidence,
		       r1.recorded_at AS from_at, r2.recorded_at AS to_at,
		       b.filing_date AS to_filed, b.incident_date AS to_incident
		LIMIT $row_limit
		UNION
		MATCH (a:Claim)-[:FILED_BY]->(c1:Claimant)-[r1:HAS_CONTACT|RESIDES_AT]->(e)<-[r2:HAS_CONTACT|RESIDES_AT]-(c2:Claimant)<-[:FILED_BY]-(b:Claim)
		WHERE a.claim_id IN $ids AND b.claim_id <> a.claim_id AND c1.claimant_id <> c2.claimant_id
		RETURN a.claim_id AS from_claim, b.claim_id AS to_claim,
		       c1.claimant_id AS from_claimant, c2.claimant_id AS to_claimant,
		       labels(e)[0] AS entity_type,
		       coalesce(e.value, e.address_id) AS entity_id,
		       e.type AS entity_subtype, e.is_public AS entity_public, indegree(e) AS entity_degree,
		       type(r1) AS from_rel, type(r2) AS to_rel,
		       r1.evidence_id AS from_evidence, r2.evidence_id AS to_evidence,
		       r1.recorded_at AS from_at, r2.recorded_at AS to_at,
		       b.filing_date AS to_filed, b.incident_date AS to_incident
		LIMIT $row_limit`
)

type partial struct {
	steps []Step
}

type claimInfo struct{ filed, incident string }

// FindRelatedClaimsReference is the original Go-loop engine (see the file comment).
func FindRelatedClaimsReference(g *falkordb.Graph, claimID string, maxHops int, excludeCategory string) (*Result, error) {
	if excludeCategory != "" && !ValidCategory(excludeCategory) {
		return nil, fmt.Errorf("unknown evidence category %q", excludeCategory)
	}
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("invalid claim_id %q", claimID)
	}
	if maxHops == 0 {
		maxHops = DefaultMaxHops
	}
	if maxHops < 1 || maxHops > MaxHopsLimit {
		return nil, fmt.Errorf("max_hops must be between 1 and %d", MaxHopsLimit)
	}
	targetFiled, err := requireClaim(g, claimID)
	if err != nil {
		return nil, err
	}

	res := &Result{TargetClaim: claimID, TargetFilingDate: targetFiled, ExcludedCategory: excludeCategory,
		MaxHops: maxHops, RelatedClaims: []RelatedClaim{}}

	visited := map[string]bool{claimID: true}
	// frontier: claim -> all shortest paths that reach it (target has the empty path).
	frontier := map[string][]partial{claimID: {{}}}
	reached := map[string]*RelatedClaim{}
	info := map[string]claimInfo{}

	for hop := 1; hop <= maxHops && len(frontier) > 0; hop++ {
		ids := make([]any, 0, len(frontier))
		for id := range frontier {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].(string) < ids[j].(string) })

		steps, filed, rowsCut, err := oneHop(g, ids)
		if err != nil {
			return nil, err
		}
		if rowsCut {
			res.Truncated = true
		}
		for k, v := range filed {
			info[k] = v
		}

		next := map[string][]partial{}
		for _, st := range steps {
			if visited[st.ToClaim] {
				continue // already reached on a shorter (or same-level earlier) hop
			}
			if excludeCategory != "" && SignalCategory(st) == excludeCategory {
				continue // counterfactual: this evidence is ignored in the analytical view
			}
			if ContextOnly(SignalCategory(st)) {
				continue // context-only identifiers never connect claims (see ContextOnly)
			}
			for _, parent := range frontier[st.FromClaim] {
				if hop > 1 && len(next[st.ToClaim]) >= maxPartialPerClaim {
					res.PathsTrimmed = true // bound path fan-out on dense graphs
					continue
				}
				chain := append(append([]Step{}, parent.steps...), st)
				next[st.ToClaim] = append(next[st.ToClaim], partial{steps: chain})
			}
		}

		for claim, paths := range next {
			visited[claim] = true
			rc := &RelatedClaim{ClaimID: claim, Hops: hop, FilingDate: info[claim].filed, IncidentDate: info[claim].incident}
			sort.Slice(paths, func(i, j int) bool { return pathKey(paths[i].steps) < pathKey(paths[j].steps) })
			for _, p := range paths {
				// Direct (1-hop) paths are never capped: scoring needs every signal category.
				if hop > 1 && len(rc.Paths) == MaxPathsPerClaim {
					res.PathsTrimmed = true
					break
				}
				rc.Paths = append(rc.Paths, Path{Hops: hop, Steps: p.steps, Description: describe(p.steps)})
			}
			reached[claim] = rc
		}
		frontier = next
	}

	for _, rc := range reached {
		res.RelatedClaims = append(res.RelatedClaims, *rc)
	}
	sort.Slice(res.RelatedClaims, func(i, j int) bool {
		a, b := res.RelatedClaims[i], res.RelatedClaims[j]
		if a.Hops != b.Hops {
			return a.Hops < b.Hops
		}
		return a.ClaimID < b.ClaimID
	})
	res.Connected = connectedOf(res.RelatedClaims)
	res.ConnectedTotal = len(res.Connected)
	if len(res.RelatedClaims) > MaxRelatedClaims {
		res.RelatedClaims = res.RelatedClaims[:MaxRelatedClaims]
	}
	// The identifier summary (context-only identifiers, infrastructure) is one independent
	// query, not traversal logic, so both engines report it with the same function.
	links, err := infrastructure(g, claimID, excludeCategory)
	if err != nil {
		return nil, err
	}
	res.InfrastructureLinks = links
	setOutcome(res)
	return res, nil
}

// oneHop runs the parameterized one-hop query for every claim in ids.
// The bool is true when the per-branch row limit was reached (results may be incomplete).
func oneHop(g *falkordb.Graph, ids []any) ([]Step, map[string]claimInfo, bool, error) {
	r, err := g.ROQuery(cypherOneHop, map[string]any{"ids": ids, "row_limit": rowLimitPerHop + 1}, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("one-hop query: %w", err)
	}
	var steps []Step
	infos := map[string]claimInfo{}
	for r.Next() {
		rec := r.Record()
		get := func(k string) any { v, _ := rec.Get(k); return v }
		st := Step{
			FromClaim: str(get("from_claim")), ToClaim: str(get("to_claim")),
			FromClaimant: str(get("from_claimant")), ToClaimant: str(get("to_claimant")),
			EntityType: str(get("entity_type")), EntityID: str(get("entity_id")),
			EntitySubtype: str(get("entity_subtype")),
			FromRel:       str(get("from_rel")), ToRel: str(get("to_rel")),
			FromEvidence: str(get("from_evidence")), ToEvidence: str(get("to_evidence")),
			FromAt: str(get("from_at")), ToAt: str(get("to_at")),
		}
		if b, ok := get("entity_public").(bool); ok {
			st.EntityPublic = &b
		}
		if d, ok := get("entity_degree").(int64); ok {
			st.EntityClaims = int(d)
		}
		steps = append(steps, st)
		infos[st.ToClaim] = claimInfo{filed: str(get("to_filed")), incident: str(get("to_incident"))}
	}
	// Deterministic order regardless of database row order.
	sort.Slice(steps, func(i, j int) bool {
		a, b := steps[i], steps[j]
		if a.FromClaim != b.FromClaim {
			return a.FromClaim < b.FromClaim
		}
		if a.ToClaim != b.ToClaim {
			return a.ToClaim < b.ToClaim
		}
		if a.EntityType != b.EntityType {
			return a.EntityType < b.EntityType
		}
		return a.EntityID < b.EntityID
	})
	return steps, infos, len(steps) > rowLimitPerHop, nil
}
