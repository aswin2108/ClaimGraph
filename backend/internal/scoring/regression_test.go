package scoring

import (
	"fmt"
	"strings"
	"testing"

	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"
)

func tow(id string) mod { return func(f *ingest.FNOL) { f.TowProviderID = id } }

// Tow provider is context-only: it must not create connected claims or any points.
func TestTowProviderIsContextOnly(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_tow")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", tow("TOW-1")), mk("B", "2026-09-11", tow("TOW-1")),
		mk("C", "2026-09-12", tow("TOW-1")), mk("D1", "2026-09-13", tow("TOW-1")),
	}, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 || r.ConnectedClaims != 0 {
		t.Fatalf("shared tow provider must earn nothing: score=%d connected=%d", r.Score, r.ConnectedClaims)
	}
}

// Contact evidence goes through two claimants; the reported path must show them.
func TestContactPathShowsClaimantHop(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_claimanthop")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", phone("+1-555-0100")), mk("B", "2026-09-11", phone("+1-555-0100"))}, nil)
	r, _ := Analyze(g, "T", 3)
	got := r.Investigation.RelatedClaims[0].Paths[0].Description
	want := "T -[FILED_BY]-> Claimant(DRV-T) -[HAS_CONTACT]-> ContactPoint(+1-555-0100) <-[HAS_CONTACT]- Claimant(DRV-B) <-[FILED_BY]- B"
	if got != want {
		t.Fatalf("path description:\n got %s\nwant %s", got, want)
	}
}

// Dense layered graph: path fan-out must stay bounded and the result must say so.
func TestDenseLayersStayBounded(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_layers")
	var fs []*ingest.FNOL
	add := func(prefix string, n int, mods ...mod) {
		for i := 0; i < n; i++ {
			fs = append(fs, mk(fmt.Sprintf("%s%02d", prefix, i), "2026-01-01", mods...))
		}
	}
	fs = append(fs, mk("T", "2026-09-10", device("DX")))
	add("A", 25, device("DX"), payout("PY"))       // layer 1: shares device with T
	add("B", 25, payout("PY"), shop("SZ"))         // layer 2: shares payout with layer 1
	add("C", 25, shop("SZ"), pubIP("203.0.113.9")) // layer 3: shares shop with layer 2
	load(t, g, fs, nil)

	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range r.Investigation.RelatedClaims {
		if rc.Hops > 1 && len(rc.Paths) > 5 {
			t.Fatalf("%s kept %d paths, cap is 5", rc.ClaimID, len(rc.Paths))
		}
	}
	// Aggregated traversal takes every reached claim in: 25 (device) + 25 (payout via layer 1)
	// = 50, complete. Layer 3 hangs off a 50-claim repair shop (infrastructure) and is not a
	// connection. Only the example paths per claim are trimmed, and the result says so.
	inv := r.Investigation
	if inv.Truncated || !inv.PathsTrimmed || inv.ConnectedTotal != 50 || r.ConnectedClaims != 50 {
		t.Errorf("want complete (not truncated), paths trimmed, 50 connected; got truncated=%v trimmed=%v total=%d scored=%d",
			inv.Truncated, inv.PathsTrimmed, inv.ConnectedTotal, r.ConnectedClaims)
	}
	for _, rc := range inv.RelatedClaims {
		if rc.ClaimID[0] == 'C' {
			t.Fatalf("%s is reachable only through infrastructure and must not be a connection", rc.ClaimID)
		}
	}
	if r.Score > 100 {
		t.Fatalf("score above cap: %d", r.Score)
	}
}

// A shop used by hundreds of claims is shared infrastructure: it counts once (+8), its
// users are not connections (no breadth, no timing points) and nothing is truncated.
func TestBusyShopCountsOnceAsInfrastructure(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	g := scratch(t, "claimgraph_test_scoring_rowlimit")
	for i := 0; i < 520; i++ {
		if _, err := ingest.Ingest(g, mk(fmt.Sprintf("R%03d", i), "2026-01-01", shop("SHOP-BIG"))); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Analyze(g, "R000", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Investigation.Truncated || r.Score != PtsRepairShop || r.Band != BandLow || len(r.Investigation.RelatedClaims) != 0 {
		t.Fatalf("want shop only (8, Low, no connections, complete); got score %d %s, %d related, truncated=%v",
			r.Score, r.Band, len(r.Investigation.RelatedClaims), r.Investigation.Truncated)
	}
	links := r.Investigation.InfrastructureLinks
	if len(links) != 1 || links[0].OtherClaims != 519 {
		t.Fatalf("the shop must be summarized with its exact user count: %+v", links)
	}
	if !strings.Contains(strings.Join(r.Uncertainty, " "), "shared infrastructure used by 519 other claims") {
		t.Errorf("the report must explain the infrastructure rule: %v", r.Uncertainty)
	}
}

// Timing and breadth come only from real connections: a claim filed the same week at a
// busy shop earns no temporal points, while a payout link still counts in full.
func TestInfrastructureDoesNotInflateBreadthOrTiming(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_infra")
	if _, err := ingest.Ingest(g, mk("T", "2026-09-10", shop("SHOP-BUSY"), payout("PAY-T"))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ { // all filed within 7 days of T
		if _, err := ingest.Ingest(g, mk(fmt.Sprintf("S%02d", i), "2026-09-12", shop("SHOP-BUSY"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ingest.Ingest(g, mk("P", "2026-07-01", payout("PAY-T"))); err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	// payout 25 + shop 8 (once, as infrastructure) + one connected claim 5 = 38; no timing bonus.
	if r.Score != PtsPayout+PtsRepairShop+PtsPerClaim || r.ConnectedClaims != 1 {
		t.Fatalf("want 38 with 1 connected claim, got %d with %d: %+v", r.Score, r.ConnectedClaims, r.Factors)
	}
	for _, f := range r.Factors {
		if f.Name == FTemporal {
			t.Fatal("claims reached only through a busy shop must not earn the timing bonus")
		}
	}
}

// The 50-claim listing cap is presentation only: a direct repair-shop link left out of the
// list (52 claims with stronger payout evidence filled it) still earns its category points.
func TestDirectLinksCutFromTheListStillScore(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	g := scratch(t, "claimgraph_test_scoring_listcap")
	claims := []*ingest.FNOL{mk("T", "2026-06-01", payout("PAY-MULE"), shop("SHOP-S"))}
	for i := 0; i < investigate.MaxRelatedClaims+2; i++ {
		claims = append(claims, mk(fmt.Sprintf("M%02d", i), "2025-01-01", payout("PAY-MULE")))
	}
	for i := 0; i < 3; i++ {
		claims = append(claims, mk(fmt.Sprintf("S%d", i), "2025-01-01", shop("SHOP-S")))
	}
	load(t, g, claims, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, rc := range r.Investigation.RelatedClaims {
		listed[rc.ClaimID] = true
	}
	if len(r.Investigation.RelatedClaims) != investigate.MaxRelatedClaims || listed["S0"] {
		t.Fatalf("setup: the list should be full of payout links and leave the shop links out: %d listed", len(r.Investigation.RelatedClaims))
	}
	pts := map[string]int{}
	for _, f := range r.Factors {
		pts[f.Name] = f.Points
	}
	if pts[FSharedPayout] != PtsPayout || pts[FSharedRepair] != PtsRepairShop {
		t.Fatalf("both direct categories must score (payout %d, shop %d): %+v", pts[FSharedPayout], pts[FSharedRepair], r.Factors)
	}
}
