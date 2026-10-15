// Package eval is the synthetic evaluation harness (Idea.txt Section 7).
//
// Every fixture builds its OWN isolated graph, ingests synthetic FNOLs through the real
// ingestion path, runs the real investigation + scoring pipeline against a target claim
// "T", and compares the outcome with the expectation written in Idea.txt. Expected scores
// are typed here once, in code, never in the UI.
//
// Implemented: T01-T20 (suspicious patterns and hard-negative controls).
// The results are for this synthetic suite only, not a claim about real-world accuracy.
package eval

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

// ---- fixture helpers -------------------------------------------------------------------

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

// mk builds a valid FNOL whose every identifier is unique unless a mod shares it.
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

// ---- scenarios -------------------------------------------------------------------------

type scenario struct {
	id        string
	desc      string
	claims    []*ingest.FNOL // the target is always the claim with ID "T"
	cases     []caseSpec
	score     int
	band      string
	related   []string // exact set of connected claim IDs
	factors   []string // factor names that must be present
	noFactors []string // factor names that must be absent
	prior     int      // expected number of prior cases retrieved
	priorDisp string   // disposition every retrieved prior case must carry
	suspect   bool     // T01-T10
	control   bool     // T11-T20
}

// same returns n copies of the mods for claims B, C, ... filed on the given dates.
func family(dates []string, mods ...mod) []*ingest.FNOL {
	names := []string{"B", "C", "D1", "E", "F"}
	var out []*ingest.FNOL
	for i, d := range dates {
		out = append(out, mk(names[i], d, mods...))
	}
	return out
}

func with(target *ingest.FNOL, rest []*ingest.FNOL) []*ingest.FNOL {
	return append([]*ingest.FNOL{target}, rest...)
}

func scenarios() []scenario {
	const tgt = "2026-09-10"
	pd := []mod{payout("P"), device("D")}
	return []scenario{
		// ---- A. suspicious patterns ----
		{id: "T01", desc: "payout+device across four claims, one within 7d (score v2: +10 burst)", suspect: true, score: 90, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, pd...), family([]string{"2026-09-12", "2026-08-01", "2026-07-01"}, pd...)),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice, scoring.FReinforcement, scoring.FTemporal}},
		{id: "T02", desc: "payout+device with one claim, no 7-day window", suspect: true, score: 60, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, pd...), family([]string{"2026-07-01"}, pd...)),
			related: []string{"B"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice}, noFactors: []string{scoring.FTemporal}},
		{id: "T03", desc: "payout+device connect to three other claims, no direct claim-claim edge", suspect: true, score: 70, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, pd...), family([]string{"2026-07-01", "2026-05-01", "2026-03-01"}, pd...)),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice}, noFactors: []string{scoring.FTemporal}},
		{id: "T04", desc: "payout+contact with a claim filed within 7d", suspect: true, score: 62, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, payout("P"), phone("+1-555-0104")), family([]string{"2026-09-12"}, payout("P"), phone("+1-555-0104"))),
			related: []string{"B"}, factors: []string{scoring.FSharedPayout, scoring.FSharedContact, scoring.FTemporal}},
		{id: "T05", desc: "device+address with one claim within 7d (score v2: household-explained, no bonus)", suspect: true, score: 45, band: scoring.BandReview,
			claims:  with(mk("T", tgt, device("D"), addr("A")), family([]string{"2026-09-12"}, device("D"), addr("A"))),
			related: []string{"B"}, factors: []string{scoring.FSharedDevice, scoring.FSharedAddress}},
		{id: "T06", desc: "payout/device link to an older human-confirmed case", suspect: true, score: 75, band: scoring.BandHigh,
			claims: with(mk("T", tgt, pd...), family([]string{"2026-07-01"}, pd...)), cases: []caseSpec{{"B", "confirmed_fraud"}},
			related: []string{"B"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice, scoring.FConfirmedFraud}, prior: 1},
		{id: "T07", desc: "two claims via payout, contact and address, with temporal proximity (score v2: household-explained, no bonus)", suspect: true, score: 67, band: scoring.BandHigh,
			claims: with(mk("T", tgt, payout("P"), phone("+1-555-0107"), addr("A")),
				family([]string{"2026-09-12", "2026-08-01"}, payout("P"), phone("+1-555-0107"), addr("A"))),
			related: []string{"B", "C"}, factors: []string{scoring.FSharedPayout, scoring.FSharedContact, scoring.FSharedAddress, scoring.FTemporal}, noFactors: []string{scoring.FReinforcement}},
		{id: "T08", desc: "multi-hop cluster: B direct (payout+device), C via B (contact), D via C (address) (score v2: +10 burst)", suspect: true, score: 90, band: scoring.BandHigh,
			claims: []*ingest.FNOL{
				mk("T", tgt, pd...),
				mk("B", "2026-09-12", payout("P"), device("D"), phone("+1-555-0108")),
				mk("C", "2026-08-01", phone("+1-555-0108"), addr("A8")),
				mk("D1", "2026-07-01", addr("A8")),
			},
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice, scoring.FTemporal, scoring.FClaimBreadth, scoring.FBurst}},
		{id: "T09", desc: "device+contact with a claim within 7d", suspect: true, score: 57, band: scoring.BandReview,
			claims:  with(mk("T", tgt, device("D"), phone("+1-555-0109")), family([]string{"2026-09-12"}, device("D"), phone("+1-555-0109"))),
			related: []string{"B"}, factors: []string{scoring.FSharedDevice, scoring.FSharedContact, scoring.FTemporal}},
		{id: "T10", desc: "payout+device with two claims within 48h", suspect: true, score: 75, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, pd...), family([]string{"2026-09-11", "2026-09-12"}, pd...)),
			related: []string{"B", "C"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice, scoring.FTemporal}},

		// ---- B. legitimate / hard-negative controls ----
		{id: "T11", desc: "busy repair shop only (four other claims)", control: true, score: 23, band: scoring.BandLow,
			claims:  with(mk("T", tgt, shop("S")), family([]string{"2026-01-01", "2026-02-01", "2026-03-01", "2026-04-01"}, shop("S"))),
			related: []string{"B", "C", "D1", "E"}, factors: []string{scoring.FSharedRepair}, noFactors: []string{scoring.FTemporal, scoring.FReinforcement}},
		{id: "T12", desc: "public IP shared across five claims", control: true, score: 18, band: scoring.BandLow,
			claims:  with(mk("T", tgt, pubIP("203.0.113.7")), family([]string{"2026-01-01", "2026-02-01", "2026-03-01", "2026-04-01"}, pubIP("203.0.113.7"))),
			related: []string{"B", "C", "D1", "E"}, factors: []string{scoring.FSharedPublicIP}},
		{id: "T13", desc: "four other claims share an address only", control: true, score: 25, band: scoring.BandLow,
			claims:  with(mk("T", tgt, addr("A")), family([]string{"2026-01-01", "2026-02-01", "2026-03-01", "2026-04-01"}, addr("A"))),
			related: []string{"B", "C", "D1", "E"}, factors: []string{scoring.FSharedAddress}, noFactors: []string{scoring.FReinforcement}},
		{id: "T14", desc: "household: joint payout account + shared address, no temporal (score v2: household-explained, no bonus)", control: true, score: 40, band: scoring.BandReview,
			claims:  with(mk("T", tgt, payout("P"), addr("A")), family([]string{"2026-07-01"}, payout("P"), addr("A"))),
			related: []string{"B"}, factors: []string{scoring.FSharedPayout, scoring.FSharedAddress}, noFactors: []string{scoring.FTemporal, scoring.FReinforcement}},
		{id: "T15", desc: "company-network IP + busy shop within 7 days", control: true, score: 36, band: scoring.BandReview,
			claims:  with(mk("T", tgt, pubIP("198.51.100.4"), shop("S")), family([]string{"2026-09-11", "2026-09-12", "2026-09-13"}, pubIP("198.51.100.4"), shop("S"))),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedPublicIP, scoring.FSharedRepair, scoring.FTemporal}},
		{id: "T16", desc: "same repair shop, incidents spread over 60 days", control: true, score: 23, band: scoring.BandLow,
			claims:  with(mk("T", tgt, shop("S")), family([]string{"2026-08-01", "2026-07-01", "2026-06-01"}, shop("S"))),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedRepair}, noFactors: []string{scoring.FTemporal}},
		{id: "T17", desc: "one phone number shared by two claimants", control: true, score: 17, band: scoring.BandLow,
			claims:  with(mk("T", tgt, phone("+1-555-0117")), family([]string{"2026-01-01"}, phone("+1-555-0117"))),
			related: []string{"B"}, factors: []string{scoring.FSharedContact}, noFactors: []string{scoring.FTemporal}},
		{id: "T18", desc: "two other claims share a public IP and repair shop", control: true, score: 21, band: scoring.BandLow,
			claims:  with(mk("T", tgt, pubIP("203.0.113.18"), shop("S")), family([]string{"2026-01-01", "2026-02-01"}, pubIP("203.0.113.18"), shop("S"))),
			related: []string{"B", "C"}, factors: []string{scoring.FSharedPublicIP, scoring.FSharedRepair}},
		{id: "T19", desc: "vendor direct-settlement account is not a claimant payout", control: true, score: 23, band: scoring.BandLow,
			claims:  with(mk("T", tgt, vendor("V"), shop("S")), family([]string{"2026-01-01", "2026-02-01", "2026-03-01"}, vendor("V"), shop("S"))),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedRepair}, noFactors: []string{scoring.FSharedPayout}},
		{id: "T20", desc: "address + repair shop of a common facility, no temporal", control: true, score: 33, band: scoring.BandReview,
			claims:  with(mk("T", tgt, addr("A"), shop("S")), family([]string{"2026-01-01", "2026-02-01", "2026-03-01"}, addr("A"), shop("S"))),
			related: []string{"B", "C", "D1"}, factors: []string{scoring.FSharedAddress, scoring.FSharedRepair}, noFactors: []string{scoring.FTemporal, scoring.FReinforcement}},
	}
}

// ---- harness ---------------------------------------------------------------------------

func isolatedGraph(t *testing.T, id string) *falkordb.Graph {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph("claimgraph_eval_" + strings.ToLower(id)) // one partition per fixture
	_ = g.Delete()                                                // clean start even after a crashed run
	t.Cleanup(func() { _ = g.Delete() })
	return g
}

func load(t *testing.T, g *falkordb.Graph, s scenario) {
	t.Helper()
	for _, f := range s.claims {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatalf("ingest %s: %v", f.ClaimID, err)
		}
	}
	for i, c := range s.cases {
		_, err := g.Query(`MATCH (c:Claim {claim_id:$claim})
			MERGE (ic:InvestigationCase {case_id:$id})
			  SET ic.disposition=$disp, ic.notes='synthetic', ic.timestamp='2026-08-01'
			MERGE (ic)-[:INVESTIGATES]->(c)`,
			map[string]any{"claim": c.claim, "id": fmt.Sprintf("CASE-%s-%d", c.claim, i), "disp": c.disposition}, nil)
		if err != nil {
			t.Fatalf("case on %s: %v", c.claim, err)
		}
	}
}

func factorNames(r *scoring.Report) map[string]bool {
	m := map[string]bool{}
	for _, f := range r.Factors {
		m[f.Name] = true
	}
	return m
}

// check runs one scenario and returns the report plus every problem found.
func check(t *testing.T, s scenario) (*scoring.Report, []string) {
	t.Helper()
	g := isolatedGraph(t, s.id)
	load(t, g, s)

	r, err := scoring.Analyze(g, "T", 3)
	if err != nil {
		t.Fatalf("%s analyze: %v", s.id, err)
	}
	assertEngineParity(t, g, "T")
	var bad []string
	if r.Score != s.score || r.Band != s.band {
		bad = append(bad, fmt.Sprintf("score/band %d %s, want %d %s", r.Score, r.Band, s.score, s.band))
	}

	// Exact connected-claim set, and a real path behind every one (no fabricated claims).
	got := map[string]bool{}
	for _, rc := range r.Investigation.RelatedClaims {
		got[rc.ClaimID] = true
		if rc.Hops < 1 || rc.Hops > 3 {
			bad = append(bad, fmt.Sprintf("%s reported at %d hops (limit 3)", rc.ClaimID, rc.Hops))
		}
		if len(rc.Paths) == 0 {
			bad = append(bad, rc.ClaimID+" has no evidence path")
		}
		for _, p := range rc.Paths {
			if p.Description == "" || len(p.Steps) == 0 {
				bad = append(bad, rc.ClaimID+" has an empty path")
			}
		}
	}
	want := append([]string(nil), s.related...)
	sort.Strings(want)
	var have []string
	for id := range got {
		have = append(have, id)
	}
	sort.Strings(have)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		bad = append(bad, fmt.Sprintf("connected claims %v, want %v", have, want))
	}

	names := factorNames(r)
	for _, f := range s.factors {
		if !names[f] {
			bad = append(bad, "missing factor "+f)
		}
	}
	for _, f := range s.noFactors {
		if names[f] {
			bad = append(bad, "unexpected factor "+f)
		}
	}
	if len(r.PriorCases) != s.prior {
		bad = append(bad, fmt.Sprintf("prior cases %d, want %d", len(r.PriorCases), s.prior))
	}

	for _, pc := range r.PriorCases {
		if s.priorDisp != "" && pc.Disposition != s.priorDisp {
			bad = append(bad, fmt.Sprintf("prior case disposition %s, want %s", pc.Disposition, s.priorDisp))
		}
	}

	// Determinism: a second run over the same graph is identical.
	r2, err := scoring.Analyze(g, "T", 3)
	if err != nil || r2.Score != r.Score || len(r2.Factors) != len(r.Factors) || r2.ConnectedClaims != r.ConnectedClaims {
		bad = append(bad, "result is not deterministic")
	}
	// The score is a priority score for humans, never a fraud verdict.
	if r.Disclaimer == "" {
		bad = append(bad, "missing disclaimer")
	}
	return r, bad
}

func TestEvaluationT01toT20(t *testing.T) {
	var alerts, suspects, falseHigh, controls, passed int
	var rows []string

	for _, s := range scenarios() {
		t.Run(s.id, func(t *testing.T) {
			r, bad := check(t, s)
			if len(bad) == 0 {
				passed++
			}
			if s.suspect {
				suspects++
				if r.Band != scoring.BandLow {
					alerts++
				}
			}
			if s.control {
				controls++
				if r.Band == scoring.BandHigh {
					falseHigh++
				}
			}
			status := "PASS"
			if len(bad) > 0 {
				status = "FAIL"
				t.Errorf("%s (%s): %s", s.id, s.desc, strings.Join(bad, "; "))
			}
			rows = append(rows, fmt.Sprintf("%-4s %-4s want %3d %-13s got %3d %-13s %s", s.id, status, s.score, s.band, r.Score, r.Band, s.desc))
		})
	}

	t.Log("\n" + strings.Join(rows, "\n"))
	t.Logf("scenarios passed: %d/20", passed)
	t.Logf("suspicious cases T01-T10 receiving at least a Review alert: %d/%d", alerts, suspects)
	t.Logf("hard negatives T11-T20 wrongly marked High Priority: %d/%d", falseHigh, controls)

	if suspects == 10 && alerts != 10 {
		t.Errorf("acceptance: all 10 suspicious cases must alert, got %d", alerts)
	}
	if controls == 10 && falseHigh != 0 {
		t.Errorf("acceptance: no hard negative may be High Priority, got %d", falseHigh)
	}
}

// Fixtures must not contaminate each other: a claim named T exists in every partition.
func TestFixturesAreIsolated(t *testing.T) {
	a := isolatedGraph(t, "iso_a")
	b := isolatedGraph(t, "iso_b")
	load(t, a, scenario{claims: []*ingest.FNOL{mk("T", "2026-09-10", shop("S")), mk("B", "2026-09-11", shop("S"))}})
	load(t, b, scenario{claims: []*ingest.FNOL{mk("T", "2026-09-10", shop("S"))}})
	ra, err := scoring.Analyze(a, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := scoring.Analyze(b, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if ra.ConnectedClaims != 1 || rb.ConnectedClaims != 0 || rb.Score != 0 {
		t.Fatalf("partitions leaked: a=%d b=%d score=%d", ra.ConnectedClaims, rb.ConnectedClaims, rb.Score)
	}
}
