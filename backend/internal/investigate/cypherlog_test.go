package investigate

import (
	"strings"
	"testing"

	"claimgraph/internal/ingest"
)

// "Show Cypher": the traversal reports every query it sent, and user input (the claim id)
// is only ever a bound parameter, never part of the query text.
func TestTraversalReportsItsCypherWithIDsAsParameters(t *testing.T) {
	g := scratch(t, "claimgraph_test_cypherlog")
	shared := func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-QXSHARED" }
	load(t, g, fx("QXTARGET", shared), fx("QXOTHER", shared),
		fx("QXFAR", func(f *ingest.FNOL) { f.DeviceID = "DEV-QX" }),
		fx("QXMID", func(f *ingest.FNOL) { f.PayoutAccountID, f.DeviceID = "PAY-QXSHARED", "DEV-QX" }))

	res, err := FindRelatedClaims(g, "QXTARGET", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Queries) != res.Stats.Queries {
		t.Fatalf("%d queries logged, stats say %d", len(res.Queries), res.Stats.Queries)
	}
	purposes := []string{"Load the target claim", "Hop 1", "Hops 2-3", "Shared infrastructure", "Other claims filed by the same claimant"}
	if len(res.Queries) != len(purposes) {
		t.Fatalf("%d queries logged, want %d: %+v", len(res.Queries), len(purposes), res.Queries)
	}
	for i, q := range res.Queries {
		if !strings.HasPrefix(q.Purpose, purposes[i]) {
			t.Errorf("query %d is %q, want %q...", i, q.Purpose, purposes[i])
		}
		if strings.Contains(q.Cypher, "QXTARGET") {
			t.Errorf("claim id pasted into the query text of %q", q.Purpose)
		}
		if !strings.Contains(q.Cypher, "$id") && !strings.Contains(q.Cypher, "$claim_id") {
			t.Errorf("query %q does not bind the claim id as a parameter", q.Purpose)
		}
		if v := q.Params["id"]; v == nil {
			if q.Params["claim_id"] != "QXTARGET" {
				t.Errorf("query %q params %v do not carry the claim id", q.Purpose, q.Params)
			}
		} else if v != "QXTARGET" {
			t.Errorf("query %q bound id %v", q.Purpose, v)
		}
		if m := q.MissingParams(); len(m) > 0 {
			t.Errorf("query %q uses parameters with no logged value: %v", q.Purpose, m)
		}
		if strings.HasPrefix(q.Cypher, "\t") || strings.HasPrefix(q.Cypher, "\n") {
			t.Errorf("query %q is not tidied for display", q.Purpose)
		}
	}
	for _, q := range res.Queries[1:3] { // every parameter the query uses was logged as sent
		for _, k := range []string{"id", "limit", "fanout", "infra", "per_claim"} {
			if _, ok := q.Params[k]; !ok {
				t.Errorf("query %q: parameter $%s used but not logged", q.Purpose, k)
			}
		}
	}
	if res.Queries[1].Results == 0 || res.Queries[2].Results == 0 {
		t.Fatalf("hop queries report no results: %+v", res.Queries[1:3])
	}

	// One hop: no deep query is sent, so none is reported.
	one, err := FindRelatedClaims(g, "QXTARGET", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range one.Queries {
		if strings.HasPrefix(q.Purpose, "Hops 2") {
			t.Fatal("a 1-hop traversal reported a deep query it never sent")
		}
	}
}
