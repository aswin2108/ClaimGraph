package investigate

// Cypher used by the investigation lives in Go as hardcoded, parameterized templates.
// Nothing user-supplied is ever concatenated into them. The multi-hop traversal
// templates are assembled from constant fragments in traverse.go.

const (
	// cypherTargetClaim fetches the target claim's basic facts.
	cypherTargetClaim = `
		MATCH (c:Claim {claim_id:$claim_id})
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date`
)
