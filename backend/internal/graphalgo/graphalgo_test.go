package graphalgo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

func fx(id, filed string, mod func(f *ingest.FNOL)) *ingest.FNOL {
	vin := "VIN"
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			vin += string(r)
		}
	}
	for len(vin) < 17 {
		vin += "0"
	}
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: vin[:17], IncidentDate: filed, FilingDate: filed, Amount: 1000,
		PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	if mod != nil {
		mod(f)
	}
	return f
}

func scratch(t *testing.T, name string) *falkordb.Graph {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	_ = g.Delete()
	t.Cleanup(func() { _ = g.Delete() })
	return g
}

func load(t *testing.T, g *falkordb.Graph, fs ...*ingest.FNOL) {
	t.Helper()
	for _, f := range fs {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatalf("ingest %s: %v", f.ClaimID, err)
		}
	}
}

func snapshot(t *testing.T, g *falkordb.Graph) string {
	t.Helper()
	r, err := g.ROQuery(`MATCH (n) OPTIONAL MATCH (n)-[e]->() RETURN count(DISTINCT n) AS n, count(e) AS e`, nil, nil)
	if err != nil || !r.Next() {
		t.Fatalf("snapshot: %v", err)
	}
	n, _ := r.Record().Get("n")
	e, _ := r.Record().Get("e")
	return fmt.Sprint(n, "/", e)
}

func ids(cs []RingClaim) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ClaimID)
	}
	return strings.Join(out, ",")
}

// T-B payout, B-C device, C-D phone (claimant level) form a ring. E (vendor settlement
// account with T), F (busy repair shop with T) and G (public device with B) are context
// only and must stay out.
func ringFixture(t *testing.T, g *falkordb.Graph) {
	load(t, g,
		fx("T", "2026-09-10", func(f *ingest.FNOL) {
			f.PayoutAccountID = "PAY-RING"
			f.RepairShopID = "SHOP-BUSY"
		}),
		fx("B", "2026-09-12", func(f *ingest.FNOL) {
			f.PayoutAccountID, f.DeviceID = "PAY-RING", "DEV-RING"
		}),
		fx("C", "2026-08-01", func(f *ingest.FNOL) {
			f.DeviceID = "DEV-RING"
			f.ContactType, f.ContactValue = "phone", "+1-555-0400"
		}),
		fx("D", "2026-07-01", func(f *ingest.FNOL) { f.ContactType, f.ContactValue = "phone", "+1-555-0400" }),
		fx("E", "2026-09-11", func(f *ingest.FNOL) { f.PayoutAccountID, f.PayoutType = "PAY-VENDOR", "vendor_direct_settlement" }),
		fx("F", "2026-09-11", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-BUSY" }),
		fx("G", "2026-09-11", func(f *ingest.FNOL) { f.DeviceID, f.DevicePublic = "DEV-KIOSK", true }),
		fx("H", "2026-09-11", nil),
	)
	// T also uses the vendor account? No: T's own payout is the ring account. Give E's
	// vendor account a second user inside the ring so a wrong filter would merge E in.
	load(t, g, fx("B2", "2026-09-13", func(f *ingest.FNOL) {
		f.PayoutAccountID, f.PayoutType = "PAY-VENDOR", "vendor_direct_settlement"
		f.DeviceID, f.DevicePublic = "DEV-KIOSK", true
	}))
}

func TestRingFindsStrongComponentOnly(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_ring")
	ringFixture(t, g)
	before := snapshot(t, g)

	ring, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ring.Members); got != "B,C,D,T" || ring.Size != 4 || ring.RingID != "RING-B" {
		t.Fatalf("ring members %q size %d id %s; want B,C,D,T / 4 / RING-B", got, ring.Size, ring.RingID)
	}
	shared := map[string]int{}
	for _, e := range ring.SharedEntities {
		shared[e.ID] = e.Claims
	}
	for _, id := range []string{"PAY-RING", "DEV-RING", "+1-555-0400"} {
		if shared[id] != 2 {
			t.Errorf("shared entity %s: %d member claims, want 2 (%+v)", id, shared[id], ring.SharedEntities)
		}
	}
	for _, id := range []string{"SHOP-BUSY", "PAY-VENDOR", "DEV-KIOSK"} {
		if _, ok := shared[id]; ok {
			t.Errorf("context-only entity %s must not be ring evidence", id)
		}
	}
	if ring.FirstFiled != "2026-07-01" || ring.LastFiled != "2026-09-12" || ring.SpanDays != 73 || ring.TotalAmount != 4000 {
		t.Errorf("ring stats: %+v", ring)
	}

	// MSF backbone: a spanning tree over the strong component (nodes - 1 edges) that
	// reaches every member claim and uses strong relationships only.
	reached := map[string]bool{}
	nodes := map[string]bool{}
	for _, l := range ring.Backbone {
		if l.Rel == "REPAIRED_AT" || l.Rel == "FILED_FROM" || l.Cost >= ingest.CostContext {
			t.Errorf("backbone uses weak/context evidence: %+v", l)
		}
		for _, n := range []GraphNode{l.From, l.To} {
			nodes[n.Type+":"+n.ID] = true
			if n.Type == "Claim" {
				reached[n.ID] = true
			}
		}
	}
	if len(ring.Backbone) != len(nodes)-1 {
		t.Errorf("backbone is not a tree: %d edges over %d nodes", len(ring.Backbone), len(nodes))
	}
	for _, c := range []string{"T", "B", "C", "D"} {
		if !reached[c] {
			t.Errorf("backbone does not reach %s", c)
		}
	}
	if ring.BackboneCost == 0 {
		t.Error("backbone cost not reported")
	}
	if after := snapshot(t, g); after != before {
		t.Fatalf("ring analysis changed the graph: %s -> %s", before, after)
	}
}

func TestRingSingletonAndErrors(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_single")
	ringFixture(t, g)
	ring, err := RingOf(g, "H")
	if err != nil {
		t.Fatal(err)
	}
	if ring.Size != 1 || ring.RingID != "" || ring.Message == "" || len(ring.SharedEntities) != 0 {
		t.Fatalf("an unconnected claim must be reported as no ring, got %+v", ring)
	}
	if _, err := RingOf(g, "NOPE-1"); !errors.Is(err, ErrClaimNotFound) {
		t.Errorf("unknown claim: %v", err)
	}
	if _, err := RingOf(g, "x'}) DETACH DELETE (n"); err == nil {
		t.Error("injection-style id must be rejected")
	}
}

// The algorithm reaches ring members that the bounded 3-transition investigation cannot.
func TestRingReachesBeyondTraversalDepth(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_deep")
	// R0 -payout- R1 -device- R2 -payout- R3 -device- R4 -payout- R5
	load(t, g,
		fx("R0", "2026-09-01", func(f *ingest.FNOL) { f.PayoutAccountID = "P01" }),
		fx("R1", "2026-09-02", func(f *ingest.FNOL) { f.PayoutAccountID, f.DeviceID = "P01", "D12" }),
		fx("R2", "2026-09-03", func(f *ingest.FNOL) { f.DeviceID, f.PayoutAccountID = "D12", "P23" }),
		fx("R3", "2026-09-04", func(f *ingest.FNOL) { f.PayoutAccountID, f.DeviceID = "P23", "D34" }),
		fx("R4", "2026-09-05", func(f *ingest.FNOL) { f.DeviceID, f.PayoutAccountID = "D34", "P45" }),
		fx("R5", "2026-09-06", func(f *ingest.FNOL) { f.PayoutAccountID = "P45" }),
	)
	inv, err := investigate.FindRelatedClaims(g, "R0", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.RelatedClaims) != 3 {
		t.Fatalf("bounded traversal should reach R1..R3 only, got %d", len(inv.RelatedClaims))
	}
	ring, err := RingOf(g, "R0")
	if err != nil {
		t.Fatal(err)
	}
	if ids(ring.Members) != "R0,R1,R2,R3,R4,R5" {
		t.Fatalf("WCC should find the whole chain: %s", ids(ring.Members))
	}
	link, err := FindStrongestLink(g, "R0", "R5")
	if err != nil {
		t.Fatal(err)
	}
	if !link.Found || strings.Join(link.Claims, ",") != "R0,R1,R2,R3,R4,R5" {
		t.Fatalf("strongest link R0->R5: %+v", link)
	}
	// 3 payout transitions (4+4 each) + 2 device transitions (5+5 each).
	if link.TotalCost != 3*8+2*10 || len(link.Links) != 10 {
		t.Errorf("cost %d over %d links, want 44 over 10", link.TotalCost, len(link.Links))
	}
}

// SPpaths minimizes evidence cost, not hop count: two strong transitions beat one weak one,
// and context-only evidence is never used.
func TestStrongestLinkPrefersStrongEvidence(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_link")
	load(t, g,
		fx("X", "2026-09-01", func(f *ingest.FNOL) { f.RepairShopID, f.PayoutAccountID = "SHOP-XY", "PAY-XZ" }),
		fx("Y", "2026-09-02", func(f *ingest.FNOL) { f.RepairShopID, f.DeviceID = "SHOP-XY", "DEV-ZY" }),
		fx("Z", "2026-09-03", func(f *ingest.FNOL) { f.PayoutAccountID, f.DeviceID = "PAY-XZ", "DEV-ZY" }),
		fx("V1", "2026-09-04", func(f *ingest.FNOL) { f.PayoutAccountID, f.PayoutType = "PAY-V", "vendor_direct_settlement" }),
		fx("V2", "2026-09-05", func(f *ingest.FNOL) { f.PayoutAccountID, f.PayoutType = "PAY-V", "vendor_direct_settlement" }),
		fx("S1", "2026-09-06", func(f *ingest.FNOL) { f.ClaimantID = "DRV-SAME" }),
		fx("S2", "2026-09-07", func(f *ingest.FNOL) { f.ClaimantID = "DRV-SAME" }),
	)
	link, err := FindStrongestLink(g, "X", "Y")
	if err != nil {
		t.Fatal(err)
	}
	if !link.Found || strings.Join(link.Claims, ",") != "X,Z,Y" || link.TotalCost != 18 {
		t.Fatalf("want X-Z-Y via payout+device (cost 18) over the direct shop link (26): %+v", link)
	}
	if strings.Contains(link.Description, "SHOP-XY") {
		t.Error("the weak shop link must not be chosen")
	}

	none, err := FindStrongestLink(g, "V1", "V2")
	if err != nil {
		t.Fatal(err)
	}
	if none.Found || len(none.Links) != 0 {
		t.Fatalf("a vendor settlement account alone must not link claims: %+v", none)
	}

	same, err := FindStrongestLink(g, "S1", "S2")
	if err != nil {
		t.Fatal(err)
	}
	if !same.Found || same.TotalCost != 0 || len(same.Evidence) != 1 || same.Evidence[0] != "Claimant(DRV-SAME)" {
		t.Fatalf("the same claimant filing both claims is reported as evidence: %+v", same)
	}
	if _, err := FindStrongestLink(g, "X", "X"); err == nil {
		t.Error("same claim twice must be rejected")
	}
	if _, err := FindStrongestLink(g, "X", "NOPE-9"); !errors.Is(err, ErrClaimNotFound) {
		t.Errorf("unknown claim: %v", err)
	}
}

func TestListRingsOrdering(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_list")
	ringFixture(t, g) // ring B,C,D,T (4 claims)
	// A 3-claim ring with a human-confirmed case ranks first despite being smaller.
	load(t, g,
		fx("K1", "2026-05-01", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-K" }),
		fx("K2", "2026-05-02", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-K" }),
		fx("K3", "2026-05-03", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-K" }),
		fx("P1", "2026-05-01", func(f *ingest.FNOL) { f.DeviceID = "DEV-PAIR" }),
		fx("P2", "2026-05-01", func(f *ingest.FNOL) { f.DeviceID = "DEV-PAIR" }),
	)
	if _, err := g.Query(`MATCH (c:Claim {claim_id:'K2'}) CREATE (:InvestigationCase {case_id:'CASE-K2', disposition:'confirmed_fraud', timestamp:'2026-06-01'})-[:INVESTIGATES]->(c)`, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, g)
	list, err := ListRings(g, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if list.TotalRings != 2 || len(list.Rings) != 2 {
		t.Fatalf("want 2 rings of >=3 claims (the 2-claim pair excluded), got %+v", list.Rings)
	}
	if list.Rings[0].RingID != "RING-K1" || list.Rings[0].ConfirmedFraudCases != 1 || list.Rings[1].RingID != "RING-B" || list.Rings[1].Size != 4 {
		t.Fatalf("ordering: %+v", list.Rings)
	}
	if two, _ := ListRings(g, 2, 10); two.TotalRings != 3 {
		t.Errorf("min_size 2 should include the pair: %d rings", two.TotalRings)
	}
	if _, err := ListRings(g, 1, 10); err == nil {
		t.Error("min_size 1 must be rejected")
	}
	if after := snapshot(t, g); after != before {
		t.Fatal("ListRings changed the graph")
	}
}

// The evidence cost on every relationship must match the category the scoring engine
// assigns to the same evidence, including after an FNOL changes a shared entity's type.
func TestOverlayMatchesScoringCategories(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_overlay")
	load(t, g,
		fx("O1", "2026-09-01", func(f *ingest.FNOL) {
			f.PayoutAccountID = "PAY-SH"
			f.DeviceID, f.DevicePublic = "DEV-SH", false
			f.IPAddress = "203.0.113.50"
			f.ContactType, f.ContactValue = "email", "o@example.test"
			f.AddressID = "ADDR-O"
		}),
		fx("O2", "2026-09-02", func(f *ingest.FNOL) {
			f.PayoutAccountID = "PAY-SH"
			f.DeviceID, f.DevicePublic = "DEV-SH", false
			f.IPAddress = "10.1.2.3"
		}),
	)
	want := map[string]int{
		investigate.CatPayout: ingest.CostPayout, investigate.CatDevice: ingest.CostDevice, investigate.CatContact: ingest.CostContact,
		investigate.CatAddress: ingest.CostAddress, investigate.CatRepairShop: ingest.CostRepairShop, investigate.CatPublicIP: ingest.CostPublicIP,
		investigate.CatVendorPayout: ingest.CostContext, investigate.CatPublicDevice: ingest.CostContext, investigate.CatPrivateIP: ingest.CostContext,
	}
	check := func(stage string) {
		t.Helper()
		r, err := g.ROQuery(`
			MATCH (:Claim)-[r:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]->(e) RETURN labels(e) AS labels, e.type AS subtype, e.is_public AS pub, r.w AS w
			UNION ALL
			MATCH (:Claimant)-[r:HAS_CONTACT|RESIDES_AT]->(e) RETURN labels(e) AS labels, e.type AS subtype, null AS pub, r.w AS w`, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for r.Next() {
			rec := r.Record()
			lv, _ := rec.Get("labels")
			sv, _ := rec.Get("subtype")
			pv, _ := rec.Get("pub")
			wv, _ := rec.Get("w")
			var typ string
			for _, l := range strings_(lv) {
				if l != ingest.LabelClaimantPayout && l != ingest.LabelPrivateDevice {
					typ = l
				}
			}
			st := investigate.Step{EntityType: typ, EntitySubtype: str(sv)}
			if b, ok := pv.(bool); ok {
				st.EntityPublic = &b
			}
			cat := investigate.SignalCategory(st)
			if got := int(num(wv)); got != want[cat] {
				t.Errorf("%s: %s edge (%s) has w=%d, want %d", stage, typ, cat, got, want[cat])
			}
			n++
		}
		if n == 0 {
			t.Fatal("no evidence edges checked")
		}
	}
	check("initial")

	// A corrected FNOL turns the shared account into a vendor settlement account: the label
	// and BOTH claims' PAID_TO costs must follow, or WCC would still treat it as a ring link.
	load(t, g, fx("O2", "2026-09-02", func(f *ingest.FNOL) {
		f.PayoutAccountID, f.PayoutType = "PAY-SH", "vendor_direct_settlement"
		f.DeviceID, f.DevicePublic = "DEV-SH", true
		f.IPAddress = "10.1.2.3"
	}))
	check("after resubmission")
	ring, err := RingOf(g, "O1")
	if err != nil {
		t.Fatal(err)
	}
	if ring.Size != 1 {
		t.Fatalf("after the account became a vendor account and the device public, O1 and O2 share no strong evidence: %+v", ring.Members)
	}

	// EnsureOverlay rebuilds the same overlay for data written without it.
	if _, err := g.Query(`MATCH ()-[r]->() SET r.w = null`, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Query(`MATCH (n:ClaimantPayout) REMOVE n:ClaimantPayout`, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ingest.EnsureOverlay(g); err != nil {
		t.Fatal(err)
	}
	check("after EnsureOverlay")
}

// A graph written before the overlay existed must produce a clear error, never a
// misleading "no ring" answer.
func TestMissingOverlayIsReportedNotHidden(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_nooverlay")
	ringFixture(t, g)
	if err := CheckOverlay(g); err != nil {
		t.Fatalf("ingested data must already carry the overlay: %v", err)
	}
	if _, err := g.Query(`MATCH ()-[r:PAID_TO]->() SET r.w = null`, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RingOf(g, "T"); !errors.Is(err, ErrOverlayMissing) {
		t.Fatalf("RingOf on an unmarked graph with gaps: want ErrOverlayMissing, got %v", err)
	}
	// An older marker version is not trusted either.
	if _, err := g.Query(`CREATE (:GraphMeta {key:'analytics_overlay', version:0})`, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RingOf(g, "T"); !errors.Is(err, ErrOverlayMissing) {
		t.Fatalf("RingOf: want ErrOverlayMissing, got %v", err)
	}
	if _, err := ListRings(g, 2, 5); !errors.Is(err, ErrOverlayMissing) {
		t.Fatalf("ListRings: want ErrOverlayMissing, got %v", err)
	}
	if _, err := FindStrongestLink(g, "T", "B"); !errors.Is(err, ErrOverlayMissing) {
		t.Fatalf("FindStrongestLink: want ErrOverlayMissing, got %v", err)
	}
	if err := ingest.EnsureOverlay(g); err != nil {
		t.Fatal(err)
	}
	if ring, err := RingOf(g, "T"); err != nil || ring.Size != 4 {
		t.Fatalf("after EnsureOverlay the ring is back: %v %+v", err, ring)
	}
}

func ringJSON(t *testing.T, r *Ring) string {
	t.Helper()
	c := *r
	c.DurationMs, c.Cached = 0, false
	b, _ := json.Marshal(c)
	return string(b)
}

// Cached algorithm results must equal fresh ones, follow every ingestion, and never leak
// across a graph that is deleted and recreated under the same name.
func TestAlgorithmCacheIsCorrect(t *testing.T) {
	g := scratch(t, "claimgraph_test_algo_cache")
	ringFixture(t, g)

	first, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	again, err := RingOf(g, "C")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Cached || !again.Cached || ids(again.Members) != ids(first.Members) {
		t.Fatalf("rings should come from the cache: %v %v", first.Cached, again.Cached)
	}
	// Without the version marker nothing is cached: the fresh computation must match.
	if _, err := g.Query(`MATCH (m:GraphMeta {key:'evidence_version'}) DELETE m`, nil, nil); err != nil {
		t.Fatal(err)
	}
	fresh, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Cached || ringJSON(t, fresh) != ringJSON(t, first) {
		t.Fatalf("cached and fresh results differ:\n%s\n%s", ringJSON(t, first), ringJSON(t, fresh))
	}

	// A new FNOL joins the ring: the next lookup must see it.
	load(t, g, fx("NEW", "2026-09-20", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-RING" }))
	warm, _ := RingOf(g, "T") // fills the cache for this version
	load(t, g, fx("NEW2", "2026-09-21", func(f *ingest.FNOL) { f.DeviceID = "DEV-RING" }))
	after, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	if warm.Size != 5 || after.Size != 6 || !after.Cached {
		t.Fatalf("ring must follow ingestion: %d then %d (cached=%v)", warm.Size, after.Size, after.Cached)
	}

	// Same graph name, different data: the random epoch keeps old results out.
	_ = g.Delete()
	load(t, g, fx("T", "2026-09-10", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-X" }),
		fx("Z", "2026-09-11", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-X" }))
	re, err := RingOf(g, "T")
	if err != nil {
		t.Fatal(err)
	}
	if ids(re.Members) != "T,Z" {
		t.Fatalf("a recreated graph must not reuse old results: %s", ids(re.Members))
	}
}
