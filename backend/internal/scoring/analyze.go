package scoring

import (
	"fmt"
	"sort"

	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

// cypherPriorCases fetches recorded investigation outcomes on a set of claims.
const cypherPriorCases = `
	MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c:Claim)
	WHERE c.claim_id IN $ids
	RETURN ic.case_id AS case_id, c.claim_id AS claim_id, ic.disposition AS disposition,
	       ic.notes AS notes, ic.timestamp AS ts`

// Analyze runs the full read-only investigation for a claim: related claims and
// paths from FalkorDB, prior outcomes on those claims, then the deterministic score.
func Analyze(g *falkordb.Graph, claimID string, maxHops int) (*Report, error) {
	return analyze(g, claimID, maxHops, "")
}

func analyze(g *falkordb.Graph, claimID string, maxHops int, exclude string) (*Report, error) {
	res, err := investigate.FindRelatedClaimsExcluding(g, claimID, maxHops, exclude)
	if err != nil {
		return nil, err
	}
	cases, err := priorCases(g, res)
	if err != nil {
		return nil, err
	}
	return Compute(res, cases), nil
}

// priorCases returns outcomes recorded on claims connected to the target.
func priorCases(g *falkordb.Graph, res *investigate.Result) ([]PriorCase, error) {
	if len(res.RelatedClaims) == 0 {
		return []PriorCase{}, nil
	}
	ids := make([]any, 0, len(res.RelatedClaims))
	for _, rc := range res.RelatedClaims {
		ids = append(ids, rc.ClaimID)
	}
	r, err := g.ROQuery(cypherPriorCases, map[string]any{"ids": ids}, nil)
	if err != nil {
		return nil, fmt.Errorf("prior cases query: %w", err)
	}
	out := []PriorCase{}
	for r.Next() {
		rec := r.Record()
		get := func(k string) string { v, _ := rec.Get(k); s, _ := v.(string); return s }
		out = append(out, PriorCase{
			CaseID: get("case_id"), ClaimID: get("claim_id"), Disposition: get("disposition"),
			Notes: get("notes"), Timestamp: get("ts"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ClaimID != out[j].ClaimID {
			return out[i].ClaimID < out[j].ClaimID
		}
		return out[i].CaseID < out[j].CaseID
	})
	return out, nil
}

// FactorChange describes how one score factor changed under a counterfactual.
type FactorChange struct {
	Name                 string `json:"name"`
	OriginalPoints       int    `json:"original_points"`
	CounterfactualPoints int    `json:"counterfactual_points"`
}

// Counterfactual compares the real analysis with one where an evidence category is ignored.
type Counterfactual struct {
	ExcludedCategory string         `json:"excluded_category"`
	Original         *Report        `json:"original"`
	Counterfactual   *Report        `json:"counterfactual"`
	Delta            int            `json:"delta"` // counterfactual score - original score
	RemovedFactors   []FactorChange `json:"removed_factors"`
	StillJustifies   bool           `json:"still_justifies_investigation"`
	Explanation      string         `json:"explanation"`
}

// RunCounterfactual recomputes the score as if one evidence category were ignored.
// It issues read-only queries only: the graph and the original claim are never changed.
func RunCounterfactual(g *falkordb.Graph, claimID string, maxHops int, category string) (*Counterfactual, error) {
	if !investigate.ValidCategory(category) {
		return nil, fmt.Errorf("exclude_signal_type must be one of %v", investigate.ScoringCategories)
	}
	orig, err := analyze(g, claimID, maxHops, "")
	if err != nil {
		return nil, err
	}
	cf, err := analyze(g, claimID, maxHops, category)
	if err != nil {
		return nil, err
	}
	return compare(orig, cf, category), nil
}

func compare(orig, cf *Report, category string) *Counterfactual {
	after := map[string]int{}
	for _, f := range cf.Factors {
		after[f.Name] = f.Points
	}
	changes := []FactorChange{}
	for _, f := range orig.Factors {
		if p, ok := after[f.Name]; !ok || p != f.Points {
			changes = append(changes, FactorChange{Name: f.Name, OriginalPoints: f.Points, CounterfactualPoints: p})
		}
	}
	c := &Counterfactual{
		ExcludedCategory: category, Original: orig, Counterfactual: cf,
		Delta: cf.Score - orig.Score, RemovedFactors: changes, StillJustifies: cf.Band != BandLow,
	}
	c.Explanation = explain(c)
	return c
}

func explain(c *Counterfactual) string {
	s := fmt.Sprintf("Ignoring %s evidence changes the investigation-priority score from %d (%s) to %d (%s), a change of %d.",
		c.ExcludedCategory, c.Original.Score, c.Original.Band, c.Counterfactual.Score, c.Counterfactual.Band, c.Delta)
	switch {
	case c.Counterfactual.ConnectedClaims == 0:
		return s + " No connecting evidence remains, so this signal alone linked the claim to others."
	case c.StillJustifies:
		return s + " The remaining evidence still warrants further investigation; the conclusion does not depend only on the excluded signal."
	default:
		return s + " The remaining evidence alone does not justify prioritizing the claim; the original priority depended on the excluded signal."
	}
}
