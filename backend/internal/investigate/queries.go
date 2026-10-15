package investigate

// All Cypher used by the investigation lives here as hardcoded, parameterized
// templates. Nothing is ever concatenated into them.

const (
	// cypherTargetClaim fetches the target claim's basic facts.
	cypherTargetClaim = `
		MATCH (c:Claim {claim_id:$claim_id})
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date`

	// cypherOneHop returns every claim-to-claim transition through ONE shared entity,
	// starting from any claim in $ids. Two branches, both returning the same columns:
	//
	//  1. Claim-level entities:  (a)-[r1]->(entity)<-[r2]-(b)
	//     payout destination, device, repair shop, public/private IP.
	//     TowProvider is deliberately NOT traversed: it is context-only (0 points) and
	//     must not create "connected claims" that earn breadth/temporal points.
	//  2. Claimant-level entities: (a)-[:FILED_BY]->(c1)-[r1]->(entity)<-[r2]-(c2)<-[:FILED_BY]-(b)
	//     contact point and address. Different claimants only (c1 <> c2), because the
	//     same person filing twice is not a shared-entity signal.
	//
	// Vehicles, policies and claimants are intentionally NOT linking entities.
	cypherOneHop = `
		MATCH (a:Claim)-[r1:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]->(e)<-[r2:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]-(b:Claim)
		WHERE a.claim_id IN $ids AND b.claim_id <> a.claim_id
		RETURN a.claim_id AS from_claim, b.claim_id AS to_claim,
		       null AS from_claimant, null AS to_claimant,
		       labels(e)[0] AS entity_type,
		       coalesce(e.account_id, e.device_id, e.ip, e.shop_id) AS entity_id,
		       e.type AS entity_subtype, e.is_public AS entity_public,
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
		       e.type AS entity_subtype, e.is_public AS entity_public,
		       type(r1) AS from_rel, type(r2) AS to_rel,
		       r1.evidence_id AS from_evidence, r2.evidence_id AS to_evidence,
		       r1.recorded_at AS from_at, r2.recorded_at AS to_at,
		       b.filing_date AS to_filed, b.incident_date AS to_incident
		LIMIT $row_limit`
)
