package ingest

// Analytics overlay.
//
// FalkorDB's built-in algorithms (algo.WCC, algo.MSF, algo.SPpaths) filter by node label,
// relationship type and a numeric edge property; they cannot evaluate a property such as
// "payout type is claimant". The overlay therefore materializes two derived facts that
// are already implied by stored properties:
//
//   - labels: ClaimantPayout on claimant payout accounts, PrivateDevice on non-public
//     devices, so strong-evidence algorithms can include exactly those entities;
//   - an evidence cost `w` on every evidence relationship, derived from the published
//     scoring points (cost = round(100 / points); context-only evidence costs 1000), so
//     weighted algorithms prefer strong evidence and can exclude weak-only context links.
//
// The overlay is never source data: it is recomputed from node properties on every
// ingestion and can be rebuilt for a whole graph with EnsureOverlay. It does not affect
// the deterministic score, which is computed from the traversal, not from `w`.

import (
	"fmt"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Overlay names used by the graph algorithms.
const (
	LabelClaimantPayout = "ClaimantPayout"
	LabelPrivateDevice  = "PrivateDevice"
	CostProperty        = "w"
)

// Evidence costs per relationship (one half of a claim-to-claim transition).
// round(100/points): payout 25 -> 4, device 20 -> 5, contact 12 -> 8, address 10 -> 10,
// repair shop 8 -> 13, public IP 3 -> 33. FILED_BY is structural (0). Context-only
// evidence (vendor settlement account, public device, private IP) costs 1000, above
// MaxStrongPathCost, so cost-capped algorithms never route through it.
const (
	CostPayout        = 4
	CostDevice        = 5
	CostContact       = 8
	CostAddress       = 10
	CostRepairShop    = 13
	CostPublicIP      = 33
	CostFiledBy       = 0
	CostContext       = 1000
	MaxStrongPathCost = CostContext - 1
)

// Global overlay statements (EnsureOverlay). Constants only, no input.
var overlayAll = []string{
	`MATCH (pd:PayoutDestination) WHERE pd.type = 'claimant' SET pd:ClaimantPayout`,
	`MATCH (pd:ClaimantPayout) WHERE pd.type IS NULL OR pd.type <> 'claimant' REMOVE pd:ClaimantPayout`,
	`MATCH (d:Device) WHERE coalesce(d.is_public, false) = false SET d:PrivateDevice`,
	`MATCH (d:PrivateDevice) WHERE d.is_public = true REMOVE d:PrivateDevice`,
	fmt.Sprintf(`MATCH (:Claim)-[r:PAID_TO]->(pd) SET r.w = CASE WHEN pd.type = 'claimant' THEN %d ELSE %d END`, CostPayout, CostContext),
	fmt.Sprintf(`MATCH (:Claim)-[r:USED_DEVICE]->(d) SET r.w = CASE WHEN coalesce(d.is_public, false) THEN %d ELSE %d END`, CostContext, CostDevice),
	fmt.Sprintf(`MATCH (:Claim)-[r:FILED_FROM]->(ip) SET r.w = CASE WHEN coalesce(ip.is_public, false) THEN %d ELSE %d END`, CostPublicIP, CostContext),
	fmt.Sprintf(`MATCH (:Claim)-[r:REPAIRED_AT]->() SET r.w = %d`, CostRepairShop),
	fmt.Sprintf(`MATCH (:Claim)-[r:FILED_BY]->() SET r.w = %d`, CostFiledBy),
	fmt.Sprintf(`MATCH (:Claimant)-[r:HAS_CONTACT]->() SET r.w = %d`, CostContact),
	fmt.Sprintf(`MATCH (:Claimant)-[r:RESIDES_AT]->() SET r.w = %d`, CostAddress),
}

// Per-claim overlay statements run by Ingest. They refresh the entities this claim uses
// and EVERY edge into them, because a resubmitted FNOL may change an entity's type or
// visibility for all claims that share it. $claim_id / $claimant_id are parameters.
var overlayClaim = []string{
	`MATCH (:Claim {claim_id:$claim_id})-[:PAID_TO]->(pd) WHERE pd.type = 'claimant' SET pd:ClaimantPayout`,
	`MATCH (:Claim {claim_id:$claim_id})-[:PAID_TO]->(pd:ClaimantPayout) WHERE pd.type IS NULL OR pd.type <> 'claimant' REMOVE pd:ClaimantPayout`,
	`MATCH (:Claim {claim_id:$claim_id})-[:USED_DEVICE]->(d) WHERE coalesce(d.is_public, false) = false SET d:PrivateDevice`,
	`MATCH (:Claim {claim_id:$claim_id})-[:USED_DEVICE]->(d:PrivateDevice) WHERE d.is_public = true REMOVE d:PrivateDevice`,
	fmt.Sprintf(`MATCH (:Claim {claim_id:$claim_id})-[:PAID_TO]->(pd)<-[r:PAID_TO]-(:Claim) SET r.w = CASE WHEN pd.type = 'claimant' THEN %d ELSE %d END`, CostPayout, CostContext),
	fmt.Sprintf(`MATCH (:Claim {claim_id:$claim_id})-[:USED_DEVICE]->(d)<-[r:USED_DEVICE]-(:Claim) SET r.w = CASE WHEN coalesce(d.is_public, false) THEN %d ELSE %d END`, CostContext, CostDevice),
	fmt.Sprintf(`MATCH (:Claim {claim_id:$claim_id})-[:FILED_FROM]->(ip)<-[r:FILED_FROM]-(:Claim) SET r.w = CASE WHEN coalesce(ip.is_public, false) THEN %d ELSE %d END`, CostPublicIP, CostContext),
	fmt.Sprintf(`MATCH (:Claim {claim_id:$claim_id})-[r:REPAIRED_AT]->() SET r.w = %d`, CostRepairShop),
	fmt.Sprintf(`MATCH (:Claim {claim_id:$claim_id})-[r:FILED_BY]->() SET r.w = %d`, CostFiledBy),
	fmt.Sprintf(`MATCH (:Claimant {claimant_id:$claimant_id})-[r:HAS_CONTACT]->() SET r.w = %d`, CostContact),
	fmt.Sprintf(`MATCH (:Claimant {claimant_id:$claimant_id})-[r:RESIDES_AT]->() SET r.w = %d`, CostAddress),
}

// OverlayVersion is recorded in a GraphMeta marker node once EnsureOverlay has run, so
// readers can trust the overlay without scanning every relationship.
const OverlayVersion = 1

const cypherOverlayMarker = `MERGE (m:GraphMeta {key:'analytics_overlay'}) SET m.version = $v`

// EnsureOverlay rebuilds the analytics overlay for the whole graph. It is idempotent and
// is run after seeding, bulk loads and at API start-up, so graphs written before the
// overlay existed (or by other tools) are brought up to date. Ingest maintains it from
// then on, claim by claim.
func EnsureOverlay(g *falkordb.Graph) error {
	for _, q := range overlayAll {
		if _, err := g.Query(q, nil, nil); err != nil {
			return fmt.Errorf("analytics overlay: %w", err)
		}
	}
	if _, err := g.Query(cypherOverlayMarker, map[string]any{"v": OverlayVersion}, nil); err != nil {
		return fmt.Errorf("analytics overlay marker: %w", err)
	}
	return bumpEvidenceVersion(g)
}

func refreshOverlayForClaim(g *falkordb.Graph, claimID, claimantID string) error {
	p := map[string]any{"claim_id": claimID, "claimant_id": claimantID}
	for _, q := range overlayClaim {
		if _, err := g.Query(q, p, nil); err != nil {
			return fmt.Errorf("analytics overlay for %s: %w", claimID, err)
		}
	}
	return nil
}
