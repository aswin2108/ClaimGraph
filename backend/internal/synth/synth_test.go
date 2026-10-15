package synth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"claimgraph/internal/disposition"
	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

func small(claims, rings int) Config {
	c := DefaultConfig()
	c.Claims, c.Rings = claims, rings
	return c
}

func connect(t *testing.T) *falkordb.FalkorDB {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	return db
}

func fresh(t *testing.T, db *falkordb.FalkorDB, name string) *falkordb.Graph {
	t.Helper()
	g := db.SelectGraph(name)
	_ = g.Delete()
	t.Cleanup(func() { _ = db.SelectGraph(name).Delete(); _ = db.SelectGraph(name + stagingSuffix).Delete() })
	return g
}

func TestGenerateIsDeterministicAndValid(t *testing.T) {
	a, b := Generate(small(800, 5)), Generate(small(800, 5))
	ja, _ := json.Marshal(a.FNOLs)
	jb, _ := json.Marshal(b.FNOLs)
	if string(ja) != string(jb) {
		t.Fatal("same config must generate the same book")
	}
	if c := Generate(func() Config { c := small(800, 5); c.Seed++; return c }()); len(c.FNOLs) == len(a.FNOLs) {
		cj, _ := json.Marshal(c.FNOLs)
		if string(cj) == string(ja) {
			t.Fatal("a different seed must generate a different book")
		}
	}
	ids := map[string]bool{}
	prev := ""
	for _, f := range a.FNOLs {
		if err := f.Validate(); err != nil {
			t.Fatalf("%s: %v", f.ClaimID, err)
		}
		if ids[f.ClaimID] {
			t.Fatalf("duplicate claim id %s", f.ClaimID)
		}
		ids[f.ClaimID] = true
		if f.FilingDate < prev {
			t.Fatalf("claims must be in filing order (%s)", f.ClaimID)
		}
		prev = f.FilingDate
	}
	if len(a.FNOLs) != 800 || len(a.Rings) != 5 {
		t.Fatalf("got %d claims / %d rings", len(a.FNOLs), len(a.Rings))
	}
	demo := a.Showcase.DemoFNOL
	if ids[demo.ClaimID] || demo.Validate() != nil || demo.FilingDate < prev {
		t.Fatalf("the showcase claim must be withheld, valid and the newest claim: %+v", demo)
	}
	if st := ingest.PolicyCheckStatus(demo.PolicyChecks()); st != "passed" {
		t.Fatalf("the showcase claim must pass the ordinary policy checks, got %s", st)
	}
	inRing := map[string]int{}
	for _, r := range a.Rings {
		for _, c := range r.Claims {
			if _, dup := inRing[c]; dup {
				t.Fatalf("claim %s is in two planted rings", c)
			}
			inRing[c] = r.Index
		}
	}
	if a.Stats.BusiestShopClaims < 30 || a.Stats.VendorPaidClaims == 0 || a.Stats.KioskClaims == 0 || a.Stats.CarrierIPClaims == 0 {
		t.Fatalf("noise is missing: %+v", a.Stats)
	}
}

// snapshot is a canonical dump of every node and edge (labels, all properties, endpoints).
func snapshot(t *testing.T, g *falkordb.Graph) []string {
	t.Helper()
	key := func(labels, props any) string {
		ls := []string{}
		for _, l := range labels.([]interface{}) {
			ls = append(ls, l.(string))
		}
		sort.Strings(ls)
		b, _ := json.Marshal(props) // map keys are sorted
		return strings.Join(ls, ":") + string(b)
	}
	var out []string
	// GraphMeta holds the overlay marker written by EnsureOverlay (bulk path only).
	r, err := g.ROQuery(`MATCH (n) WHERE NOT n:GraphMeta RETURN labels(n) AS l, properties(n) AS p`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		l, _ := r.Record().Get("l")
		p, _ := r.Record().Get("p")
		out = append(out, "N "+key(l, p))
	}
	r, err = g.ROQuery(`MATCH (a)-[e]->(b) RETURN type(e) AS t, properties(e) AS p, labels(a) AS la, properties(a) AS pa, labels(b) AS lb, properties(b) AS pb`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		rec := r.Record()
		get := func(k string) any { v, _ := rec.Get(k); return v }
		b, _ := json.Marshal(get("p"))
		out = append(out, fmt.Sprintf("E %s %s %s -> %s", get("t"), b, key(get("la"), get("pa")), key(get("lb"), get("pb"))))
	}
	sort.Strings(out)
	return out
}

// The bulk loader must write exactly what one-by-one ingestion and the disposition
// recorder write, including the analytics overlay.
func TestBulkLoadEqualsIngest(t *testing.T) {
	db := connect(t)
	ds := Generate(small(300, 4))

	fresh(t, db, "claimgraph_test_synth_bulk")
	rep, err := Load(db, "claimgraph_test_synth_bulk", ds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Swapped || rep.Nodes["Claim"] != 300 {
		t.Fatalf("load report: %+v", rep)
	}
	if exists, _ := db.Conn.Exists(t.Context(), "claimgraph_test_synth_bulk"+stagingSuffix).Result(); exists != 0 {
		t.Fatal("the staging graph must be gone after the swap")
	}
	bulk := snapshot(t, db.SelectGraph("claimgraph_test_synth_bulk"))

	g := fresh(t, db, "claimgraph_test_synth_ingest")
	for _, f := range ds.FNOLs {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range ds.Cases {
		ts, _ := time.Parse(time.RFC3339, c.Timestamp)
		rec := &disposition.Recorder{Graph: g, Now: func() time.Time { return ts }}
		if _, err := rec.Record(c.ClaimID, c.Disposition, c.Notes); err != nil {
			t.Fatal(err)
		}
	}
	one := snapshot(t, g)

	if len(bulk) != len(one) {
		t.Errorf("bulk graph has %d items, ingested graph %d", len(bulk), len(one))
	}
	for i := 0; i < len(bulk) && i < len(one); i++ {
		if bulk[i] != one[i] {
			t.Fatalf("first difference at item %d:\n bulk:   %s\n ingest: %s", i, bulk[i], one[i])
		}
	}
}

// Every planted ring is recovered exactly by WCC (no merging with legitimate noise), and
// the showcase ring is longer than the bounded investigation can see.
func TestPlantedRingsAreRecoveredAndShowcaseWorks(t *testing.T) {
	db := connect(t)
	ds := Generate(small(1500, 6))
	name := "claimgraph_test_synth_rings"
	fresh(t, db, name)
	if _, err := Load(db, name, ds, nil); err != nil {
		t.Fatal(err)
	}
	g := db.SelectGraph(name)
	for _, pr := range ds.Rings {
		ring, err := graphalgo.RingOf(g, pr.Claims[0])
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range ring.Members {
			got = append(got, m.ClaimID)
		}
		if strings.Join(got, ",") != strings.Join(pr.Claims, ",") {
			t.Errorf("ring %d (%s): WCC found %v, planted %v", pr.Index, pr.Shape, got, pr.Claims)
		}
		if (pr.ConfirmedCase != "") != (ring.ConfirmedFraudCases > 0) {
			t.Errorf("ring %d: confirmed case mismatch", pr.Index)
		}
	}

	// Submit the showcase claim live, as in the demo.
	demo := ds.Showcase.DemoFNOL
	if _, err := ingest.Ingest(g, demo); err != nil {
		t.Fatal(err)
	}
	ring, err := graphalgo.RingOf(g, demo.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if ring.Size != ds.Showcase.RingSize || ring.ConfirmedFraudCases != 1 {
		t.Fatalf("showcase ring: size %d (want %d), confirmed %d", ring.Size, ds.Showcase.RingSize, ring.ConfirmedFraudCases)
	}
	inv, err := investigate.FindRelatedClaims(g, demo.ClaimID, 3)
	if err != nil {
		t.Fatal(err)
	}
	ringMembers := map[string]bool{}
	for _, c := range ds.Rings[0].Claims {
		ringMembers[c] = true
	}
	seen := 0
	for _, rc := range inv.RelatedClaims {
		if ringMembers[rc.ClaimID] {
			seen++
		}
	}
	if seen == 0 || seen >= len(ds.Rings[0].Claims) {
		t.Fatalf("the 3-transition investigation should see part of the showcase ring (saw %d of %d)", seen, len(ds.Rings[0].Claims))
	}
	if st := ingest.PolicyCheckStatus(demo.PolicyChecks()); st != "passed" {
		t.Fatal("the showcase claim must look clean on its own")
	}
}
