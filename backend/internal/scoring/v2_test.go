package scoring

import (
	"testing"

	"claimgraph/internal/ingest"
)

func policyFrom(start string) mod {
	return func(f *ingest.FNOL) { f.PolicyStart, f.PolicyEnd = start, "2027-12-31" }
}

func pts(r *Report, name string) int {
	for _, f := range r.Factors {
		if f.Name == name {
			return f.Points
		}
	}
	return 0
}

// A joint account and a family phone shared inside one household keep their category points
// but lose the multi-signal bonus; the same identifiers shared across addresses keep it.
func TestHouseholdExplanation(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_household")
	load(t, g, []*ingest.FNOL{
		mk("H1", "2026-09-10", payout("JOINT"), phone("+1-555-0201"), addr("HOME")),
		mk("H2", "2026-06-01", payout("JOINT"), phone("+1-555-0201"), addr("HOME")),
	}, nil)
	h, err := Analyze(g, "H1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HouseholdExplained || pts(h, FReinforcement) != 0 || pts(h, FSharedPayout) != PtsPayout {
		t.Fatalf("household: explained=%v factors=%+v", h.HouseholdExplained, h.Factors)
	}

	g2 := scratch(t, "claimgraph_test_scoring_crosshousehold")
	load(t, g2, []*ingest.FNOL{
		mk("R1", "2026-09-10", payout("MULE"), phone("+1-555-0202"), addr("ADDR-1")),
		mk("R2", "2026-06-01", payout("MULE"), phone("+1-555-0202"), addr("ADDR-2")),
	}, nil)
	r, err := Analyze(g2, "R1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.HouseholdExplained || pts(r, FReinforcement) != PtsReinforce {
		t.Fatalf("sharing across addresses is unexplained: explained=%v factors=%+v", r.HouseholdExplained, r.Factors)
	}
}

// The burst counts only claims linked by strong evidence.
func TestClaimBurstNeedsStrongLinks(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_burst")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", payout("P"), shop("S")),
		mk("A", "2026-08-01", payout("P")), mk("B", "2026-08-20", payout("P")), mk("C", "2026-07-15", payout("P")),
		mk("W1", "2026-09-01", shop("S")), mk("W2", "2026-09-02", shop("S")), mk("W3", "2026-09-03", shop("S")),
	}, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if pts(r, FBurst) != PtsBurst {
		t.Fatalf("three payout-linked claims within 90 days are a burst: %+v", r.Factors)
	}
	g2 := scratch(t, "claimgraph_test_scoring_noburst")
	load(t, g2, []*ingest.FNOL{
		mk("T", "2026-09-10", shop("S")), mk("W1", "2026-09-01", shop("S")), mk("W2", "2026-09-02", shop("S")), mk("W3", "2026-09-03", shop("S")),
	}, nil)
	r2, err := Analyze(g2, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if pts(r2, FBurst) != 0 {
		t.Fatalf("claims linked only by a shared shop are not a burst: %+v", r2.Factors)
	}
}

// New-policy cluster: both the claim and a strongly linked claim were filed soon after
// their policies started. An old policy on either side does not trigger it.
func TestNewPolicyCluster(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_newpolicy")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", device("D"), policyFrom("2026-08-20")),
		mk("B", "2026-07-01", device("D"), policyFrom("2026-06-10")),
		mk("OLD", "2026-07-01", device("D")), // policy from 2026-01-01
	}, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	var f *Factor
	for i := range r.Factors {
		if r.Factors[i].Name == FNewPolicy {
			f = &r.Factors[i]
		}
	}
	if f == nil || len(f.Claims) != 1 || f.Claims[0] != "B" {
		t.Fatalf("want a new-policy factor naming B only: %+v", r.Factors)
	}
	g2 := scratch(t, "claimgraph_test_scoring_oldpolicy")
	load(t, g2, []*ingest.FNOL{
		mk("T", "2026-09-10", device("D")), mk("B", "2026-07-01", device("D"), policyFrom("2026-06-10")),
	}, nil)
	r2, err := Analyze(g2, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if pts(r2, FNewPolicy) != 0 {
		t.Fatal("a claim on an old policy does not trigger the new-policy signal")
	}
}
