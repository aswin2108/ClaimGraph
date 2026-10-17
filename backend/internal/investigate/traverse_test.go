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
	c.Stats = nil   // timing differs between runs and engines
	c.Queries = nil // only the production engine logs its Cypher (for "Show Cypher")
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
	if res.OwnClaims == nil || res.OwnClaims.ClaimantID != "DRV-SAME" || res.OwnClaims.Total != 2 || len(res.OwnClaims.Claims) != 2 {
		t.Fatalf("the claimant's other claims must be listed as context: %+v", res.OwnClaims)
	}
	load(t, g, fx("X1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-OWN" }))
	res, _ = FindRelatedClaims(g, "R1", 3)
	// X1 is a direct link. The repeat claimant's other claims are NOT reached through X1 either:
	// R1 -> PAY-OWN -> X1 -> PAY-OWN -> R2 only walks out and back to the same person.
	if got := related(res); len(got) != 1 || got["X1"].Hops != 1 {
		t.Fatalf("only the other claimant's claim is a link: %+v", res.RelatedClaims)
	}
	if res.OwnClaims == nil || res.OwnClaims.Total != 2 {
		t.Fatalf("R2 and R3 stay listed as the claimant's own claims: %+v", res.OwnClaims)
	}
	// A recorded decision on one of them is shown with it (context; it adds no points here).
	if _, err := g.Query(`MATCH (c:Claim {claim_id:'R2'}) CREATE (:InvestigationCase {case_id:'CASE-R2', disposition:'confirmed_fraud'})-[:INVESTIGATES]->(c)`, nil, nil); err != nil {
		t.Fatal(err)
	}
	res, _ = FindRelatedClaims(g, "R1", 3)
	decisions := map[string]string{}
	for _, c := range res.OwnClaims.Claims {
		decisions[c.ClaimID] = c.Decision
	}
	if decisions["R2"] != "confirmed_fraud" || decisions["R3"] != "" {
		t.Fatalf("own claims must carry their recorded decision: %+v", res.OwnClaims.Claims)
	}
}

// No person appears twice on a path. The live case (CLM-119990): the target's claimant K
// lives at an address shared with neighbour N; K also filed an older claim. The older claim
// must not appear at 2 hops through N's claim (out and back through the same address), nor
// through a DIFFERENT identifier N shares with it; the same holds for a person in the middle
// of a path (N's other claim is not reached by passing through N twice).
func TestNoClaimantAppearsTwiceOnAPath(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_one_person")
	load(t, g,
		// K: the target and K's older claim, both at ADDR-K; the older claim uses device DEV-N.
		fx("T1", func(f *ingest.FNOL) { f.ClaimantID, f.AddressID = "DRV-K", "ADDR-K" }),
		fx("T2", func(f *ingest.FNOL) { f.ClaimantID, f.AddressID, f.DeviceID = "DRV-K", "ADDR-K", "DEV-N" }),
		// N: a neighbour at ADDR-K whose first claim also uses DEV-N; N's second claim shares
		// a payout account with M2.
		fx("N1", func(f *ingest.FNOL) { f.ClaimantID, f.AddressID, f.DeviceID = "DRV-N", "ADDR-K", "DEV-N" }),
		fx("N2", func(f *ingest.FNOL) { f.ClaimantID, f.PayoutAccountID = "DRV-N", "PAY-M" }),
		fx("M2", func(f *ingest.FNOL) { f.ClaimantID, f.PayoutAccountID = "DRV-M2", "PAY-M" }),
	)
	filer := map[string]string{}
	r, err := g.ROQuery("MATCH (c:Claim)-[:FILED_BY]->(k:Claimant) RETURN c.claim_id AS c, k.claimant_id AS k", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		c, _ := r.Record().Get("c")
		k, _ := r.Record().Get("k")
		filer[c.(string)] = k.(string)
	}
	onePersonOnce := func(target string, res *Result) {
		t.Helper()
		for _, rc := range res.RelatedClaims {
			for _, p := range rc.Paths {
				seen := map[string]bool{filer[target]: true}
				for _, s := range p.Steps {
					if seen[filer[s.ToClaim]] {
						t.Fatalf("%s: claimant %s appears twice on %s", target, filer[s.ToClaim], p.Description)
					}
					seen[filer[s.ToClaim]] = true
				}
			}
		}
	}
	for hops := 1; hops <= MaxHopsLimit; hops++ {
		res, err := FindRelatedClaims(g, "T1", hops)
		if err != nil {
			t.Fatal(err)
		}
		got := related(res)
		if _, ok := got["T2"]; ok {
			t.Fatalf("hops=%d: the target claimant's own claim must never be a related claim: %+v", hops, got["T2"])
		}
		if got["N1"].Hops != 1 || got["N2"].Hops != 1 {
			t.Fatalf("hops=%d: both of the neighbour's claims are direct (shared address): %+v", hops, res.RelatedClaims)
		}
		if hops >= 2 && got["M2"].Hops != 2 {
			t.Fatalf("hops=%d: M2 is reached through N2's payout account: %+v", hops, res.RelatedClaims)
		}
		onePersonOnce("T1", res)
		if res.OwnClaims == nil || len(res.OwnClaims.Claims) != 1 || res.OwnClaims.Claims[0].ClaimID != "T2" {
			t.Fatalf("hops=%d: T2 must be listed as the claimant's other claim: %+v", hops, res.OwnClaims)
		}
		want, err := FindRelatedClaimsReference(g, "T1", hops, "")
		if err != nil {
			t.Fatal(err)
		}
		if a, b := normalize(t, res), normalize(t, want); a != b {
			t.Fatalf("hops=%d: engines disagree: %s", hops, firstDiff(a, b))
		}
	}
	// A person in the middle: from M2, N2 is direct and K's claims are 2 hops away through N's
	// address; N1 would only be reached by passing through N twice (N2 -> ADDR-K -> T2 -> DEV-N -> N1).
	res, err := FindRelatedClaims(g, "M2", 3)
	if err != nil {
		t.Fatal(err)
	}
	got := related(res)
	if got["N2"].Hops != 1 || got["T1"].Hops != 2 || got["T2"].Hops != 2 {
		t.Fatalf("M2: unexpected links: %+v", res.RelatedClaims)
	}
	if _, ok := got["N1"]; ok {
		t.Fatalf("M2: N1 is only reachable by revisiting claimant N: %+v", got["N1"].Paths)
	}
	onePersonOnce("M2", res)
	if want, err := FindRelatedClaimsReference(g, "M2", 3, ""); err != nil {
		t.Fatal(err)
	} else if a, b := normalize(t, res), normalize(t, want); a != b {
		t.Fatalf("M2: engines disagree: %s", firstDiff(a, b))
	}
	// EvidencePaths (the pair query behind the GetEvidencePaths tool) applies the same rule.
	if paths, err := EvidencePaths(g, "T1", "T2", 3, ""); err != nil || len(paths) != 0 {
		t.Fatalf("no evidence path may lead from a claim to its own claimant's other claim: %v %+v", err, paths)
	}
}
