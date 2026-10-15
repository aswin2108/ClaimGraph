package actions

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

func fnol(id, filed string, mod func(f *ingest.FNOL)) *ingest.FNOL {
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: ("VIN" + id + "00000000000000")[:17], IncidentDate: filed, FilingDate: filed, Amount: 1000,
		PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	if mod != nil {
		mod(f)
	}
	return f
}

func graphWith(t *testing.T, name string, fs ...*ingest.FNOL) *falkordb.Graph {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	_ = g.Delete()
	t.Cleanup(func() { _ = g.Delete() })
	for _, f := range fs {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func plan(t *testing.T, g *falkordb.Graph, claim string) *Plan {
	t.Helper()
	rep, err := scoring.Analyze(g, claim, 3)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := graphalgo.RingSummaryOf(g, claim)
	if err != nil {
		t.Fatal(err)
	}
	return Recommend(rep, ring)
}

func kinds(p *Plan) string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.Kind)
	}
	return strings.Join(out, ",")
}

// Actions are verification steps only: never a decision about a claim, never an accusation.
var forbidden = regexp.MustCompile(`(?i)\b(deny|denied|reject|approve|pay out|release payment|cancel|fraudster|guilty|criminal|committed fraud)\b`)

func checkLanguage(t *testing.T, p *Plan) {
	t.Helper()
	for _, a := range p.Actions {
		if !a.RequiresHuman {
			t.Errorf("%s must require human approval", a.Kind)
		}
		if m := forbidden.FindString(a.Title + " " + a.Rationale); m != "" {
			t.Errorf("%s uses decision/accusatory language %q: %s", a.Kind, m, a.Title)
		}
	}
}

func TestRingProducesHoldAndVerificationActions(t *testing.T) {
	var fs []*ingest.FNOL
	for i := 0; i < 5; i++ { // five people at five addresses, one mule account, a burst on new policies
		i := i
		fs = append(fs, fnol(fmt.Sprintf("Q%d", i), fmt.Sprintf("2026-08-%02d", 1+i*5), func(f *ingest.FNOL) {
			f.PayoutAccountID, f.AddressID = "PAY-MULE", fmt.Sprintf("ADDR-%d", i)
			f.PolicyStart = "2026-07-20"
		}))
	}
	g := graphWith(t, "claimgraph_test_actions_ring", fs...)
	p := plan(t, g, "Q4")
	checkLanguage(t, p)
	got := kinds(p)
	for _, want := range []string{KindHoldRingForReview, KindVerifyPayoutHolder, KindVerifyPolicyOrigins} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if p.Actions[0].Kind != KindHoldRingForReview || !p.Actions[0].Simulated || len(p.Actions[0].Claims) != 4 {
		t.Fatalf("the ring hold comes first, is simulated and lists the 4 other open claims: %+v", p.Actions[0])
	}
	for _, a := range p.Actions {
		if a.Kind == KindVerifyPayoutHolder && (!strings.Contains(a.Title, "PAY-MULE") || !strings.Contains(a.EvidencePath, "PAY-MULE")) {
			t.Fatalf("the payout action must name the account and cite a real path: %+v", a)
		}
	}
}

func TestHouseholdAndIsolatedClaims(t *testing.T) {
	g := graphWith(t, "claimgraph_test_actions_household",
		fnol("H1", "2026-09-10", func(f *ingest.FNOL) { f.PayoutAccountID, f.AddressID = "JOINT", "HOME" }),
		fnol("H2", "2026-03-01", func(f *ingest.FNOL) { f.PayoutAccountID, f.AddressID = "JOINT", "HOME" }),
		fnol("LONE", "2026-09-10", nil),
	)
	h := plan(t, g, "H1")
	checkLanguage(t, h)
	if kinds(h) != KindConfirmHousehold {
		t.Fatalf("a household gets a 'confirm household' step only, got %s", kinds(h))
	}
	lone := plan(t, g, "LONE")
	if kinds(lone) != KindProcessOrdinarily || lone.Actions[0].Rationale == "" {
		t.Fatalf("an unconnected claim needs no graph verification: %+v", lone.Actions)
	}
}
