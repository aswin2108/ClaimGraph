package investigate

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"claimgraph/internal/ingest"
)

// firstDiff shows a short window around the first difference of two long strings.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	win := func(s string) string {
		lo, hi := i-150, i+250
		if lo < 0 {
			lo = 0
		}
		if hi > len(s) {
			hi = len(s)
		}
		return s[lo:hi]
	}
	return fmt.Sprintf("first difference at byte %d\n  FalkorDB:  ...%s\n  reference: ...%s", i, win(a), win(b))
}

// claimHops lists "claim:hops" pairs in a fixed order.
func claimHops(r *Result) string {
	var out []string
	for _, rc := range r.RelatedClaims {
		out = append(out, fmt.Sprintf("%s:%d", rc.ClaimID, rc.Hops))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func normalize(t *testing.T, r *Result) string {
	t.Helper()
	c := *r
	c.Stats = nil // timing differs between runs and engines
	c.RelatedClaims = append([]RelatedClaim(nil), r.RelatedClaims...)
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

// The FalkorDB-side traversal must agree with the independent Go-loop reference on
// randomly wired graphs: shared claimants (same person filing twice), vendor payouts,
// public/private devices and IPs, contacts and addresses, for every depth and exclusion.
func TestTraversalMatchesReferenceOnRandomGraphs(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_random")
	rng := rand.New(rand.NewSource(20261015))
	const claimsN = 18 // enough for multi-hop links now that vendor accounts and kiosk devices never connect claims
	var ids []string
	for i := 0; i < claimsN; i++ {
		id := fmt.Sprintf("R%02d", i)
		ids = append(ids, id)
		load(t, g, fx(id, func(f *ingest.FNOL) {
			f.ClaimantID = fmt.Sprintf("DRV-K%d", rng.Intn(11)) // some claimants file more than once
			if rng.Intn(2) == 0 {
				n := rng.Intn(9)
				f.PayoutAccountID = fmt.Sprintf("PAY-%d", n)
				f.PayoutType = "claimant"
				if n%4 == 0 {
					f.PayoutType = "vendor_direct_settlement"
				}
			}
			if rng.Intn(2) == 0 {
				f.RepairShopID = fmt.Sprintf("SHOP-%d", rng.Intn(5))
			}
			if rng.Intn(3) == 0 {
				n := rng.Intn(8)
				f.DeviceID, f.DevicePublic = fmt.Sprintf("DEV-%d", n), n%3 == 0
			}
			if rng.Intn(4) == 0 {
				f.IPAddress = []string{"203.0.113.7", "203.0.113.8", "10.0.0.5"}[rng.Intn(3)]
			}
			if rng.Intn(3) == 0 {
				f.ContactType, f.ContactValue = "phone", fmt.Sprintf("+1-555-%04d", rng.Intn(9))
			}
			if rng.Intn(3) == 0 {
				f.AddressID = fmt.Sprintf("ADDR-%d", rng.Intn(9))
			}
		}))
	}

	exclusions := append([]string{""}, ScoringCategories...)
	compared, multiHop, capped := 0, 0, 0
	for _, id := range ids {
		for hops := 1; hops <= MaxHopsLimit; hops++ {
			for _, ex := range exclusions {
				got, err := FindRelatedClaimsExcluding(g, id, hops, ex)
				if err != nil {
					t.Fatalf("%s hops=%d ex=%q: %v", id, hops, ex, err)
				}
				want, err := FindRelatedClaimsReference(g, id, hops, ex)
				if err != nil {
					t.Fatal(err)
				}
				if got.Truncated || want.Truncated {
					// A result cap was hit (the reference trims candidate paths in discovery
					// order, the new engine keeps the first five of all shortest paths), so
					// only the connected claims and their distances are comparable.
					if claimHops(got) != claimHops(want) {
						t.Fatalf("truncated results disagree for %s hops=%d exclude=%q", id, hops, ex)
					}
					capped++
				} else if a, b := normalize(t, got), normalize(t, want); a != b {
					t.Fatalf("engines disagree for %s hops=%d exclude=%q: %s", id, hops, ex, firstDiff(a, b))
				}
				compared++
				if ex == "" && !got.Truncated {
					// The one-query, ids-only variant (ConnectedOnly) must reach exactly the same claims.
					only, cut, err := ConnectedOnly(g, id, hops)
					if err != nil || cut {
						t.Fatalf("ConnectedOnly %s hops=%d: %v cut=%v", id, hops, err, cut)
					}
					want := map[string]int{}
					for _, c := range got.Connected {
						want[c.ClaimID] = c.Hops
					}
					if fmt.Sprint(only) != fmt.Sprint(want) {
						t.Fatalf("ConnectedOnly disagrees for %s hops=%d: %v vs %v", id, hops, only, want)
					}
				}
				for _, rc := range got.RelatedClaims {
					if rc.Hops > 1 {
						multiHop++
					}
				}
			}
		}
	}
	if multiHop == 0 {
		t.Fatal("the random graph produced no multi-hop connections, so the comparison proves nothing")
	}
	if capped*2 > compared {
		t.Fatalf("%d of %d comparisons hit a result cap; the graph is too dense for a strict comparison", capped, compared)
	}
	t.Logf("%d comparisons (%d with a result cap), %d multi-hop related claims checked", compared, capped, multiHop)
}

func TestEvidencePathsBetweenTwoClaims(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_pair")
	load(t, g,
		fx("A1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-SHARED"; f.RepairShopID = "SHOP-X" }),
		fx("B1", func(f *ingest.FNOL) {
			f.PayoutAccountID = "PAY-SHARED"
			f.RepairShopID = "SHOP-X"
			f.DeviceID = "DEV-X"
		}),
		fx("C1", func(f *ingest.FNOL) { f.DeviceID = "DEV-X" }),
		fx("Z1", nil),
	)
	direct, err := EvidencePaths(g, "A1", "B1", 3, "")
	if err != nil || len(direct) != 2 || direct[0].Hops != 1 {
		t.Fatalf("A1-B1 should connect directly through payout and shop: %v %+v", err, direct)
	}
	two, err := EvidencePaths(g, "A1", "C1", 3, "")
	if err != nil || len(two) != 2 || two[0].Hops != 2 {
		t.Fatalf("A1-C1 should connect in two transitions (via payout or shop, then device): %v %+v", err, two)
	}
	if want := "DEV-X"; two[0].Steps[1].EntityID != want {
		t.Errorf("second step should go through %s: %s", want, two[0].Description)
	}
	// Ignoring the repair shop leaves exactly one route.
	noShop, _ := EvidencePaths(g, "A1", "C1", 3, CatRepairShop)
	if len(noShop) != 1 || noShop[0].Steps[0].EntityID != "PAY-SHARED" {
		t.Errorf("excluding the repair shop should leave only the payout route: %+v", noShop)
	}
	if none, err := EvidencePaths(g, "A1", "Z1", 3, ""); err != nil || none != nil {
		t.Errorf("unconnected claims must return no path, got %v %v", none, err)
	}
	if _, err := EvidencePaths(g, "A1", "A1", 3, ""); err == nil {
		t.Error("same claim twice must be rejected")
	}
	if _, err := EvidencePaths(g, "A1", "x'}) DETACH DELETE (n", 3, ""); err == nil {
		t.Error("injection-style id must be rejected")
	}
	if _, err := EvidencePaths(g, "A1", "C1", 4, ""); err == nil {
		t.Error("depth over the limit must be rejected")
	}
	// Depth is honored: C1 is two transitions from A1.
	if p, _ := EvidencePaths(g, "A1", "C1", 1, ""); p != nil {
		t.Errorf("max_hops=1 must not reach C1: %+v", p)
	}
}

// A busy repair shop is summarized as infrastructure (exact count, no per-claim rows), so
// it can never crowd a payout link out of the result and nothing is truncated.
func TestBusyRepairShopIsSummarizedAsInfrastructure(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_busyshop")
	load(t, g, fx("T0", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-BUSY"; f.PayoutAccountID = "PAY-RING" }))
	for i := 0; i < MaxRelatedClaims+10; i++ {
		load(t, g, fx(fmt.Sprintf("A%03d", i), func(f *ingest.FNOL) { f.RepairShopID = "SHOP-BUSY" }))
	}
	load(t, g, fx("ZZ9", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-RING" })) // sorts after every shop claim

	res, err := FindRelatedClaims(g, "T0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RelatedClaims) != 1 || res.Truncated {
		t.Fatalf("only the payout-linked claim is a connection; got %d related (truncated=%v)", len(res.RelatedClaims), res.Truncated)
	}
	first := res.RelatedClaims[0]
	if first.ClaimID != "ZZ9" || !strings.Contains(first.Paths[0].Description, "PAY-RING") {
		t.Fatalf("the payout link must be reported, got %s: %s", first.ClaimID, first.Paths[0].Description)
	}
	if len(res.InfrastructureLinks) != 1 {
		t.Fatalf("the busy shop must be summarized once: %+v", res.InfrastructureLinks)
	}
	if l := res.InfrastructureLinks[0]; l.EntityID != "SHOP-BUSY" || l.Category != CatRepairShop || l.OtherClaims != MaxRelatedClaims+10 {
		t.Fatalf("infrastructure summary: %+v", l)
	}
	// A shop at or under the threshold is still an ordinary weak link to each claim.
	g2 := scratch(t, "claimgraph_test_investigate_smallshop")
	load(t, g2, fx("T0", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-SMALL" }))
	for i := 0; i < InfrastructureMinClaims-1; i++ {
		load(t, g2, fx(fmt.Sprintf("B%03d", i), func(f *ingest.FNOL) { f.RepairShopID = "SHOP-SMALL" }))
	}
	small, err := FindRelatedClaims(g2, "T0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(small.RelatedClaims) != InfrastructureMinClaims-1 || small.InfrastructureLinks != nil {
		t.Fatalf("a shop used by %d claims is not infrastructure: %d related, %+v", InfrastructureMinClaims, len(small.RelatedClaims), small.InfrastructureLinks)
	}
	if small.RelatedClaims[0].Paths[0].Steps[0].EntityClaims != InfrastructureMinClaims {
		t.Errorf("steps must report how many claims use the entity: %+v", small.RelatedClaims[0].Paths[0].Steps[0])
	}
}

// Strong identifiers are never treated as infrastructure: a payout account shared by more
// than InfrastructureMinClaims claims stays a direct, expandable connection.
func TestStrongHubIsNotInfrastructure(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_stronghub")
	load(t, g, fx("T0", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-MULE" }))
	for i := 0; i < InfrastructureMinClaims+5; i++ {
		id := fmt.Sprintf("M%03d", i)
		load(t, g, fx(id, func(f *ingest.FNOL) {
			f.PayoutAccountID = "PAY-MULE"
			if i == 0 {
				f.DeviceID = "DEV-NEXT"
			}
		}))
	}
	load(t, g, fx("N1", func(f *ingest.FNOL) { f.DeviceID = "DEV-NEXT" }))
	res, err := FindRelatedClaims(g, "T0", 2)
	if err != nil {
		t.Fatal(err)
	}
	got := related(res)
	if len(got) != InfrastructureMinClaims+6 || got["M000"].Hops != 1 || got["N1"].Hops != 2 || res.InfrastructureLinks != nil {
		t.Fatalf("a mule account must stay a strong, expandable link: %d related, N1 hops %d, infra %+v",
			len(got), got["N1"].Hops, res.InfrastructureLinks)
	}
}

// Excluding a category removes its relationship type from the query itself.
func TestExcludedRelationshipsAreNotTraversed(t *testing.T) {
	if q := directQuery(relSetsFor(CatRepairShop)); strings.Contains(q, "REPAIRED_AT") {
		t.Error("repair_shop exclusion must drop REPAIRED_AT from the traversal")
	}
	if q := deepQuery(relSetsFor(CatContact), 2, 3, false, outAggregated); strings.Contains(q, "HAS_CONTACT") || !strings.Contains(q, "RESIDES_AT") || strings.Contains(q, "|") {
		t.Error("contact exclusion must drop only HAS_CONTACT")
	}
	if q := deepQuery(relSetsFor(""), 2, 3, false, outAggregated); strings.Count(q, "UNION ALL") != 4+8-1 {
		t.Errorf("expected the 4 two-transition and 8 three-transition shapes, got %d branches", strings.Count(q, "UNION ALL")+1)
	}
}

// A hub is never expanded into further transitions; a small group still is.
func TestFanOutGuardStopsHubExpansion(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_fanout")
	load(t, g, fx("T0", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-HUB" }))
	for i := 0; i < maxExpandFanout+2; i++ {
		id := fmt.Sprintf("H%03d", i)
		load(t, g, fx(id, func(f *ingest.FNOL) {
			f.RepairShopID = "SHOP-HUB"
			if i == 0 {
				f.PayoutAccountID = "PAY-BEHIND-HUB"
			}
		}))
	}
	load(t, g, fx("Q1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-BEHIND-HUB" }))

	res, err := FindRelatedClaims(g, "T0", 3)
	if err != nil {
		t.Fatal(err)
	}
	got := related(res)
	if _, listed := got["H000"]; listed || len(res.InfrastructureLinks) != 1 || res.InfrastructureLinks[0].OtherClaims != maxExpandFanout+2 {
		t.Fatalf("a busy shop's users are summarized as infrastructure, not listed: %+v", res.InfrastructureLinks)
	}
	if _, found := got["Q1"]; found {
		t.Fatal("Q1 is only reachable by expanding through a hub, which the guard forbids")
	}
	// The same claim IS reached once the group is small enough to expand.
	load(t, g, fx("S0", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-SMALL" }), fx("S1", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-SMALL"; f.PayoutAccountID = "PAY-S" }),
		fx("S2", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-S" }))
	small, _ := FindRelatedClaims(g, "S0", 3)
	if related(small)["S2"].Hops != 2 {
		t.Fatalf("a small shop must still be expanded: %+v", small.RelatedClaims)
	}
}

// The same person filing twice with their own account and phone/device is not shared-entity
// evidence; a different claimant using that account is.
func TestSameClaimantIsNotSharedEvidenceAtClaimLevel(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_repeat")
	own := func(f *ingest.FNOL) { f.ClaimantID, f.PayoutAccountID, f.DeviceID = "DRV-SAME", "PAY-OWN", "DEV-OWN" }
	load(t, g, fx("R1", own), fx("R2", own), fx("R3", own))
	res, err := FindRelatedClaims(g, "R1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RelatedClaims) != 0 {
		t.Fatalf("a repeat claimant's own account/device must not link their claims: %+v", res.RelatedClaims)
	}
	load(t, g, fx("X1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-OWN" }))
	res, _ = FindRelatedClaims(g, "R1", 3)
	// X1 is a direct link; the repeat claimant's other claims are now reached THROUGH X1
	// (a different claimant), never directly.
	if got := related(res); len(got) != 3 || got["X1"].Hops != 1 || got["R2"].Hops != 2 || got["R3"].Hops != 2 {
		t.Fatalf("another claimant using the account is a real link: %+v", res.RelatedClaims)
	}
}

// Outcome types make "nothing found" an explicit answer.
func TestOutcomeTypes(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_outcome")
	load(t, g, fx("T0", func(f *ingest.FNOL) { f.RepairShopID = "SHOP-BUSY" }))
	for i := 0; i < InfrastructureMinClaims+1; i++ {
		load(t, g, fx(fmt.Sprintf("U%02d", i), func(f *ingest.FNOL) { f.RepairShopID = "SHOP-BUSY" }))
	}
	load(t, g, fx("LONE", nil), fx("P1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-P" }), fx("P2", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-P" }))
	for claim, want := range map[string]string{"T0": OutcomeInfrastructureOnly, "LONE": OutcomeNoConnection, "P1": OutcomeConnected} {
		res, err := FindRelatedClaims(g, claim, 3)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != want || (want != OutcomeConnected && res.Message == "") {
			t.Errorf("%s: outcome %q message %q, want %q", claim, res.Outcome, res.Message, want)
		}
	}
}
