package investigate

// Cypher used by the investigation lives in Go as hardcoded, parameterized templates.
// Nothing user-supplied is ever concatenated into them. The multi-hop traversal
// templates are assembled from constant fragments in traverse.go.

const (
	// cypherTargetClaim fetches the target claim's basic facts.
	cypherTargetClaim = `
		MATCH (c:Claim {claim_id:$claim_id})
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date`

	// cypherSameClaimant lists the other claims filed by the target's claimant (context only:
	// a person's own claims are never links between claims, so they add no points).
	cypherSameClaimant = `
		MATCH (c:Claim {claim_id:$id})-[:FILED_BY]->(k:Claimant)<-[:FILED_BY]-(o:Claim)
		WHERE ID(o) <> ID(c)
		WITH k, o, [(ic:InvestigationCase)-[:INVESTIGATES]->(o) | ic.disposition][0] AS decision
		ORDER BY o.filing_date DESC, o.claim_id
		WITH k, count(o) AS total, collect(o.claim_id)[..$limit] AS ids,
		     collect(coalesce(o.filing_date, ''))[..$limit] AS filed,
		     collect(coalesce(o.incident_date, ''))[..$limit] AS incident,
		     collect(coalesce(decision, ''))[..$limit] AS decisions
		RETURN k.claimant_id AS claimant, total, ids, filed, incident, decisions`
)
