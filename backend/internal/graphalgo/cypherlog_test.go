package graphalgo

import (
	"strings"
	"testing"
)

// "Show Cypher" for the ring and the strongest link: the algorithm calls are reported, and
// claim ids are bound parameters, never part of the query text.
func TestRingAndLinkReportTheirCypher(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_cypherlog")
	ringFixture(t, g)

	ring, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	var wcc, msf bool
	for _, q := range ring.Queries {
		wcc = wcc || strings.Contains(q.Cypher, "algo.WCC")
		msf = msf || strings.Contains(q.Cypher, "algo.MSF")
		if m := q.MissingParams(); len(m) > 0 {
			t.Errorf("query %q uses parameters with no logged value: %v", q.Purpose, m)
		}
		if q.Purpose == "" || q.Cypher == "" {
			t.Errorf("incomplete query entry: %+v", q)
		}
		for _, id := range []string{"'T'", `"T"`, "'B'", `"B"`} {
			if strings.Contains(q.Cypher, id) {
				t.Errorf("claim id %s pasted into %q", id, q.Purpose)
			}
		}
	}
	if !wcc || !msf {
		t.Fatalf("ring queries must include algo.WCC and algo.MSF: %+v", ring.Queries)
	}

	link, err := FindStrongestLink(g, "T", "D")
	if err != nil {
		t.Fatal(err)
	}
	q := link.Query
	if q == nil || !strings.Contains(q.Cypher, "algo.SPpaths") || q.Params["from"] != "T" || q.Params["to"] != "D" || q.Results != 1 {
		t.Fatalf("strongest-link query: %+v", q)
	}
	if m := q.MissingParams(); len(m) > 0 {
		t.Fatalf("algo.SPpaths query uses parameters with no logged value: %v", m)
	}
	if strings.Contains(q.Cypher, "'T'") || strings.Contains(q.Cypher, "'D'") {
		t.Fatal("claim ids pasted into the algo.SPpaths query")
	}
}
