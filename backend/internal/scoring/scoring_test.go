package scoring

import (
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

type mod func(f *ingest.FNOL)

func payout(id string) mod {
	return func(f *ingest.FNOL) { f.PayoutAccountID, f.PayoutType = id, "claimant" }
}
func vendor(id string) mod {
	return func(f *ingest.FNOL) { f.PayoutAccountID, f.PayoutType = id, "vendor_direct_settlement" }
}
func device(id string) mod { return func(f *ingest.FNOL) { f.DeviceID, f.DevicePublic = id, false } }
func shop(id string) mod   { return func(f *ingest.FNOL) { f.RepairShopID = id } }
func phone(v string) mod   { return func(f *ingest.FNOL) { f.ContactType, f.ContactValue = "phone", v } }
func addr(id string) mod   { return func(f *ingest.FNOL) { f.AddressID = id } }
func pubIP(ip string) mod  { return func(f *ingest.FNOL) { f.IPAddress = ip } }

// mk builds a valid FNOL with unique defaults, filed (and incident) on `filed`.
func mk(id, filed string, mods ...mod) *ingest.FNOL {
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: vin(id), IncidentDate: filed, FilingDate: filed, Amount: 1000,
		PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	for _, m := range mods {
		m(f)
	}
	return f
}

func vin(id string) string {
	out := "VIN"
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			out += string(r)
		}
	}
	for len(out) < 17 {
		out += "0"
	}
	return out[:17]
}

type caseSpec struct{ claim, disposition string }

func scratch(t *testing.T, name string) *falkordb.Graph {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	return g
}

func load(t *testing.T, g *falkordb.Graph, claims []*ingest.FNOL, cases []caseSpec) {
	for _, f := range claims {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatalf("ingest %s: %v", f.ClaimID, err)
		}
	}
	for i, c := range cases {
		_, err := g.Query(`MATCH (c:Claim {claim_id:$claim})
			MERGE (ic:InvestigationCase {case_id:$id})
			  SET ic.disposition=$disp, ic.notes='synthetic', ic.timestamp='2026-10-01'
			MERGE (ic)-[:INVESTIGATES]->(c)`,
			map[string]any{"claim": c.claim, "id": "CASE-" + c.claim + string(rune('0'+i)), "disp": c.disposition}, nil)
		if err != nil {
			t.Fatalf("case on %s: %v", c.claim, err)
		}
	}
}

func factorPoints(r *Report, name string) (int, bool) {
	for _, f := range r.Factors {
		if f.Name == name {
			return f.Points, true
		}
	}
	return 0, false
}

// Expected scores come from Idea.txt Section 7 (T01...T28).
func TestScenarioScores(t *testing.T) {
	tests := []struct {
		name   string
		claims []*ingest.FNOL
		cases  []caseSpec
		want   int
		band   string
	}{
		{"T01 payout+device, 3 others, one within 7d", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-09-12", payout("P"), device("D")),
			mk("C", "2026-08-01", payout("P"), device("D")), mk("D1", "2026-07-01", payout("P"), device("D"))}, nil, 90, BandHigh}, // v2: +10 burst
		{"T02 payout+device, one other, no window", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-07-01", payout("P"), device("D"))}, nil, 60, BandHigh},
		{"T03 payout+device, three others, no window", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-07-01", payout("P"), device("D")),
			mk("C", "2026-05-01", payout("P"), device("D")), mk("D1", "2026-03-01", payout("P"), device("D"))}, nil, 70, BandHigh},
		{"T05 device+address, one within 7d", []*ingest.FNOL{
			mk("T", "2026-09-10", device("D"), addr("A")), mk("B", "2026-09-12", device("D"), addr("A"))}, nil, 45, BandReview}, // v2: household-explained
		{"T06 payout/device + older confirmed-fraud case", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-07-01", payout("P"), device("D"))},
			[]caseSpec{{"B", "confirmed_fraud"}}, 75, BandHigh},
		{"T10 payout+device, two others within 48h", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-09-11", payout("P"), device("D")),
			mk("C", "2026-09-12", payout("P"), device("D"))}, nil, 75, BandHigh},
		{"T11 busy repair shop only", []*ingest.FNOL{
			mk("T", "2026-09-10", shop("S")), mk("B", "2026-01-01", shop("S")), mk("C", "2026-02-01", shop("S")),
			mk("D1", "2026-03-01", shop("S")), mk("E", "2026-04-01", shop("S"))}, nil, 23, BandLow},
		{"T12 public IP only", []*ingest.FNOL{
			mk("T", "2026-09-10", pubIP("203.0.113.7")), mk("B", "2026-01-01", pubIP("203.0.113.7")),
			mk("C", "2026-02-01", pubIP("203.0.113.7")), mk("D1", "2026-03-01", pubIP("203.0.113.7")),
			mk("E", "2026-04-01", pubIP("203.0.113.7"))}, nil, 18, BandLow},
		{"T14 joint payout + address household", []*ingest.FNOL{
			mk("T", "2026-09-10", payout("P"), addr("A")), mk("B", "2026-07-01", payout("P"), addr("A"))}, nil, 40, BandReview}, // v2: household-explained
		{"T15 company IP + busy shop within 7d", []*ingest.FNOL{
			mk("T", "2026-09-10", pubIP("198.51.100.4"), shop("S")), mk("B", "2026-09-11", pubIP("198.51.100.4"), shop("S")),
			mk("C", "2026-09-12", pubIP("198.51.100.4"), shop("S")), mk("D1", "2026-09-13", pubIP("198.51.100.4"), shop("S"))}, nil, 36, BandReview},
		{"T19 vendor direct-settlement account is not a claimant payout", []*ingest.FNOL{
			mk("T", "2026-09-10", vendor("V"), shop("S")), mk("B", "2026-01-01", vendor("V"), shop("S")),
			mk("C", "2026-02-01", vendor("V"), shop("S")), mk("D1", "2026-03-01", vendor("V"), shop("S"))}, nil, 23, BandLow},
		{"T23 not-substantiated case via shop earns no bonus", []*ingest.FNOL{
			mk("T", "2026-09-10", shop("S")), mk("B", "2026-01-01", shop("S"))},
			[]caseSpec{{"B", "not_substantiated"}}, 13, BandLow},
		{"T24 needs-info case via device earns no bonus", []*ingest.FNOL{
			mk("T", "2026-09-10", device("D")), mk("B", "2026-01-01", device("D"))},
			[]caseSpec{{"B", "needs_info"}}, 25, BandLow},
		{"T28 no connections", []*ingest.FNOL{mk("T", "2026-09-10"), mk("B", "2026-09-10")}, nil, 0, BandLow},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := scratch(t, "claimgraph_test_scoring_"+string(rune('a'+i)))
			load(t, g, tc.claims, tc.cases)
			r, err := Analyze(g, "T", 3)
			if err != nil {
				t.Fatal(err)
			}
			if r.Score != tc.want || r.Band != tc.band {
				t.Fatalf("score=%d band=%s want %d %s; factors=%+v", r.Score, r.Band, tc.want, tc.band, r.Factors)
			}
			// Deterministic: a second run is identical.
			r2, _ := Analyze(g, "T", 3)
			if r2.Score != r.Score || len(r2.Factors) != len(r.Factors) {
				t.Fatal("score is not deterministic")
			}
		})
	}
}

func TestVendorAccountNeverScoresAsPayout(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_vendor")
	load(t, g, []*ingest.FNOL{mk("T", "2026-09-10", vendor("V")), mk("B", "2026-01-01", vendor("V"))}, nil)
	r, _ := Analyze(g, "T", 3)
	if _, ok := factorPoints(r, FSharedPayout); ok {
		t.Fatal("vendor direct-settlement account must not earn the payout signal")
	}
}

func TestNoConnectionsInventsNothing(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_empty")
	load(t, g, []*ingest.FNOL{mk("T", "2026-09-10")}, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 || r.ConnectedClaims != 0 || len(r.Factors) != 0 || len(r.PriorCases) != 0 {
		t.Fatalf("expected an empty report, got %+v", r)
	}
	if len(r.Uncertainty) == 0 || r.Investigation.Message == "" {
		t.Fatal("must state explicitly that no supporting connection was found")
	}
}

func TestCounterfactualT27PayoutExclusion(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_cf27")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-07-01", payout("P"), device("D"))}, nil)
	cf, err := RunCounterfactual(g, "T", 3, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Original.Score != 60 || cf.Counterfactual.Score != 25 || cf.Delta != -35 {
		t.Fatalf("want 60 -> 25 (-35), got %d -> %d (%d)", cf.Original.Score, cf.Counterfactual.Score, cf.Delta)
	}
	if cf.Original.Band != BandHigh || cf.Counterfactual.Band != BandLow || cf.StillJustifies {
		t.Fatalf("bands wrong: %s -> %s, justifies=%v", cf.Original.Band, cf.Counterfactual.Band, cf.StillJustifies)
	}
	names := map[string]bool{}
	for _, c := range cf.RemovedFactors {
		names[c.Name] = true
	}
	if !names[FSharedPayout] || !names[FReinforcement] {
		t.Fatalf("payout and reinforcement bonus should be removed, got %+v", cf.RemovedFactors)
	}
	if len(cf.Counterfactual.Investigation.RelatedClaims) != 1 {
		t.Fatal("device path should still connect the claims")
	}
}

func TestCounterfactualT26ShopExclusionAndGraphUnchanged(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_cf26")
	all := func(id, filed string) *ingest.FNOL {
		return mk(id, filed, payout("P"), device("D"), phone("+1-555-0100"), shop("S"))
	}
	load(t, g, []*ingest.FNOL{all("T", "2026-09-10"), all("B", "2026-09-11"), all("C", "2026-09-12")}, nil)

	count := func() (n, e int64) {
		for q, dst := range map[string]*int64{`MATCH (n) RETURN count(n)`: &n, `MATCH ()-[r]->() RETURN count(r)`: &e} {
			r, err := g.ROQuery(q, nil, nil)
			if err != nil || !r.Next() {
				t.Fatal(err)
			}
			v, _ := r.Record().GetByIndex(0)
			*dst = v.(int64)
		}
		return
	}
	n0, e0 := count()

	cf, err := RunCounterfactual(g, "T", 3, "repair_shop")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Original.Score != 95 || cf.Counterfactual.Score != 87 || cf.Delta != -8 {
		t.Fatalf("want 95 -> 87 (-8), got %d -> %d (%d)", cf.Original.Score, cf.Counterfactual.Score, cf.Delta)
	}
	if !cf.StillJustifies || cf.Counterfactual.Band != BandHigh {
		t.Fatalf("should remain High Priority: %+v", cf.Counterfactual.Band)
	}
	if len(cf.Counterfactual.Investigation.RelatedClaims) != 2 {
		t.Fatal("other paths still connect both related claims")
	}
	for _, rc := range cf.Counterfactual.Investigation.RelatedClaims {
		for _, p := range rc.Paths {
			if investigate.SignalCategory(p.Steps[0]) == investigate.CatRepairShop {
				t.Fatal("excluded repair-shop path leaked into the counterfactual view")
			}
		}
	}
	if n1, e1 := count(); n0 != n1 || e0 != e1 {
		t.Fatalf("counterfactual mutated the graph: nodes %d->%d edges %d->%d", n0, n1, e0, e1)
	}
}

func TestCounterfactualRejectsBadCategory(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_cfbad")
	load(t, g, []*ingest.FNOL{mk("T", "2026-09-10")}, nil)
	for _, c := range []string{"", "DROP", "tow_provider", "x') DETACH DELETE n //"} {
		if _, err := RunCounterfactual(g, "T", 3, c); err == nil {
			t.Errorf("category %q should be rejected", c)
		}
	}
}

func TestBands(t *testing.T) {
	for score, want := range map[int]string{0: BandLow, 29: BandLow, 30: BandReview, 59: BandReview, 60: BandHigh, 100: BandHigh} {
		if b, _ := Band(score); b != want {
			t.Errorf("Band(%d)=%s want %s", score, b, want)
		}
	}
}
