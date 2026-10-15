package eval

// Engine parity: FalkorDB-side traversal (production) must agree with the independent
// Go-loop reference on every fixture, for every depth and every counterfactual
// exclusion, and the pair-evidence query must reproduce each related claim's paths.

import (
	"encoding/json"
	"sort"
	"testing"

	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

// normalized serializes a result with related claims in a fixed order (the engines may
// order claims of equal distance differently; the content must be identical).
func normalized(t *testing.T, r *investigate.Result) string {
	t.Helper()
	c := *r
	c.Stats = nil // timing differs between runs and engines
	c.RelatedClaims = append([]investigate.RelatedClaim(nil), r.RelatedClaims...)
	sort.Slice(c.RelatedClaims, func(i, j int) bool {
		a, b := c.RelatedClaims[i], c.RelatedClaims[j]
		if a.Hops != b.Hops {
			return a.Hops < b.Hops
		}
		return a.ClaimID < b.ClaimID
	})
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func assertEngineParity(t *testing.T, g *falkordb.Graph, target string) {
	t.Helper()
	exclusions := append([]string{""}, investigate.ScoringCategories...)
	for hops := 1; hops <= investigate.MaxHopsLimit; hops++ {
		for _, ex := range exclusions {
			got, err := investigate.FindRelatedClaimsExcluding(g, target, hops, ex)
			if err != nil {
				t.Fatalf("new engine hops=%d exclude=%q: %v", hops, ex, err)
			}
			want, err := investigate.FindRelatedClaimsReference(g, target, hops, ex)
			if err != nil {
				t.Fatalf("reference engine hops=%d exclude=%q: %v", hops, ex, err)
			}
			if a, b := normalized(t, got), normalized(t, want); a != b {
				t.Fatalf("engines disagree (hops=%d exclude=%q): %d vs %d related claims", hops, ex, len(got.RelatedClaims), len(want.RelatedClaims))
			}
		}
	}
	// Pair evidence query reproduces the paths of every related claim.
	full, err := investigate.FindRelatedClaims(g, target, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range full.RelatedClaims {
		paths, err := investigate.EvidencePaths(g, target, rc.ClaimID, rc.Hops, "")
		if err != nil {
			t.Fatalf("EvidencePaths %s: %v", rc.ClaimID, err)
		}
		a, _ := json.Marshal(paths)
		b, _ := json.Marshal(rc.Paths)
		if string(a) != string(b) {
			t.Fatalf("EvidencePaths(%s) differs from the investigation paths\n%s\n%s", rc.ClaimID, a, b)
		}
	}
}
