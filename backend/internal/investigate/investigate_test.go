package investigate

import (
	"errors"
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

// fx builds a minimal valid FNOL; each claim gets unique claimant/vehicle/shop/payout by default.
func fx(id string, mod func(f *ingest.FNOL)) *ingest.FNOL {
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: "VIN" + padTo14(id), IncidentDate: "2026-09-01", FilingDate: "2026-09-02",
		Amount: 1000, PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	if mod != nil {
		mod(f)
	}
	return f
}

func padTo14(s string) string {
	out := ""
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			out += string(r)
		}
	}
	for len(out) < 14 {
		out += "0"
	}
	return out[:14]
}

func scratch(t *testing.T, name string) *falkordb.Graph {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	return g
}

func load(t *testing.T, g *falkordb.Graph, fs ...*ingest.FNOL) {
	for _, f := range fs {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatalf("ingest %s: %v", f.ClaimID, err)
		}
	}
}

func related(r *Result) map[string]RelatedClaim {
	m := map[string]RelatedClaim{}
	for _, rc := range r.RelatedClaims {
		m[rc.ClaimID] = rc
	}
	return m
}

// Chain: A-B payout (1 hop), B-C device (2 hops), C-D phone (3 hops), D-E address (4 hops, out of range).
func TestMultiHopChainAndBounds(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_chain")
	load(t, g,
		fx("A1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-SHARED" }),
		fx("B1", func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-SHARED"; f.DeviceID = "DEV-X" }),
		fx("C1", func(f *ingest.FNOL) { f.DeviceID = "DEV-X"; f.ContactType, f.ContactValue = "phone", "+1-555-0001" }),
		fx("D1", func(f *ingest.FNOL) { f.ContactType, f.ContactValue = "phone", "+1-555-0001"; f.AddressID = "ADDR-X" }),
		fx("E1", func(f *ingest.FNOL) { f.AddressID = "ADDR-X" }),
	)

	res, err := FindRelatedClaims(g, "A1", 3)
	if err != nil {
		t.Fatal(err)
	}
	got := related(res)
	wantHops := map[string]int{"B1": 1, "C1": 2, "D1": 3}
	if len(got) != len(wantHops) {
		t.Fatalf("want %d related claims, got %d: %+v", len(wantHops), len(got), res.RelatedClaims)
	}
	for id, h := range wantHops {
		if got[id].Hops != h {
			t.Errorf("%s hops=%d want %d", id, got[id].Hops, h)
		}
	}
	if _, ok := got["E1"]; ok {
		t.Error("E1 is 4 transitions away and must not be returned")
	}
	// The 3-hop path must be complete and name the real entities in order.
	steps := got["D1"].Paths[0].Steps
	if len(steps) != 3 || steps[0].EntityID != "PAY-SHARED" || steps[1].EntityID != "DEV-X" || steps[2].EntityID != "+1-555-0001" {
		t.Errorf("unexpected D1 path: %s", got["D1"].Paths[0].Description)
	}
	if steps[0].FromEvidence == "" || steps[0].FromAt == "" {
		t.Error("evidence id / timestamp missing on claim-level edge")
	}

	// Depth limit is honored.
	res1, _ := FindRelatedClaims(g, "A1", 1)
	if len(res1.RelatedClaims) != 1 || res1.RelatedClaims[0].ClaimID != "B1" {
		t.Errorf("max_hops=1 should return only B1, got %+v", res1.RelatedClaims)
	}
}

func TestEntityDetailsAndMultipleEvidence(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_detail")
	load(t, g,
		fx("T1", func(f *ingest.FNOL) {
			f.PayoutAccountID, f.PayoutType = "PAY-V", "vendor_direct_settlement"
			f.DeviceID, f.DevicePublic, f.IPAddress = "DEV-Y", false, "203.0.113.9"
		}),
		fx("T2", func(f *ingest.FNOL) {
			f.PayoutAccountID, f.PayoutType = "PAY-V", "vendor_direct_settlement"
			f.DeviceID, f.IPAddress = "DEV-Y", "203.0.113.9"
		}),
	)
	res, err := FindRelatedClaims(g, "T1", 1)
	if err != nil {
		t.Fatal(err)
	}
	rc := related(res)["T2"]
	if len(rc.Paths) != 3 {
		t.Fatalf("T2 should connect through payout, device and IP, got %d paths", len(rc.Paths))
	}
	seen := map[string]Step{}
	for _, p := range rc.Paths {
		seen[p.Steps[0].EntityType] = p.Steps[0]
	}
	if s := seen["PayoutDestination"]; s.EntitySubtype != "vendor_direct_settlement" {
		t.Errorf("payout type not reported: %+v", s)
	}
	if s := seen["Device"]; s.EntityPublic == nil || *s.EntityPublic {
		t.Errorf("device should be non-public: %+v", s)
	}
	if s := seen["IPAddress"]; s.EntityPublic == nil || !*s.EntityPublic {
		t.Errorf("203.0.113.9 should be public: %+v", s)
	}
}

func TestNoConnectionsReturnsNothingInvented(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_none")
	load(t, g, fx("L1", nil), fx("L2", nil))
	res, err := FindRelatedClaims(g, "L1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RelatedClaims) != 0 || res.Message == "" {
		t.Fatalf("want no related claims and an explicit message, got %+v", res)
	}
}

func TestSameClaimantIsNotASharedContact(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_sameclaimant")
	load(t, g,
		fx("S1", func(f *ingest.FNOL) { f.ContactType, f.ContactValue = "email", "same@example.test" }),
		fx("S2", func(f *ingest.FNOL) {
			f.ClaimantID = "DRV-S1" // same person files again
			f.ContactType, f.ContactValue = "email", "same@example.test"
		}),
	)
	res, _ := FindRelatedClaims(g, "S1", 3)
	if len(res.RelatedClaims) != 0 {
		t.Fatalf("same claimant must not create a contact link: %+v", res.RelatedClaims)
	}
}

func TestValidationAndNotFound(t *testing.T) {
	g := scratch(t, "claimgraph_test_investigate_errors")
	load(t, g, fx("Z1", nil))
	if _, err := FindRelatedClaims(g, "NOPE-1", 3); !errors.Is(err, ErrClaimNotFound) {
		t.Errorf("want ErrClaimNotFound, got %v", err)
	}
	if _, err := FindRelatedClaims(g, "x'}) DETACH DELETE (n", 3); err == nil {
		t.Error("injection-style id must be rejected")
	}
	for _, h := range []int{-1, 4, 99} {
		if _, err := FindRelatedClaims(g, "Z1", h); err == nil {
			t.Errorf("max_hops=%d must be rejected", h)
		}
	}
}
