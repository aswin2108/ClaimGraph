package ingest

import (
	"strings"
	"testing"

	"claimgraph/internal/graph"
)

// A corrected FNOL must replace single-valued evidence, not accumulate it.
func TestResubmitReplacesStaleEvidence(t *testing.T) {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph("claimgraph_test_resubmit")
	defer g.Delete()

	f := validFNOL()
	f.TowProviderID, f.DeviceID, f.IPAddress = "TOW-A", "DEV-A", "203.0.113.5"
	if _, err := Ingest(g, f); err != nil {
		t.Fatal(err)
	}

	// Correction: new payout, new shop, and tow/device/IP no longer reported.
	g2 := validFNOL()
	g2.PayoutAccountID, g2.RepairShopID = "PAY-NEW", "SHOP-NEW"
	res, err := Ingest(g, g2)
	if err != nil || res.Created {
		t.Fatalf("resubmission: %+v err=%v", res, err)
	}

	count := func(q string) int64 {
		r, err := g.ROQuery(q, map[string]any{"id": f.ClaimID}, nil)
		if err != nil || !r.Next() {
			t.Fatalf("query %q: %v", q, err)
		}
		v, _ := r.Record().GetByIndex(0)
		return v.(int64)
	}
	for rel, want := range map[string]int64{"PAID_TO": 1, "REPAIRED_AT": 1, "TOWED_BY": 0, "USED_DEVICE": 0, "FILED_FROM": 0, "COVERED_BY": 1, "FILED_BY": 1, "INVOLVES": 1} {
		q := `MATCH (:Claim {claim_id:$id})-[r:` + rel + `]->() RETURN count(r)`
		if got := count(q); got != want {
			t.Errorf("%s edges after correction = %d, want %d", rel, got, want)
		}
	}
	if got := count(`MATCH (:Claim {claim_id:$id})-[:PAID_TO]->(p:PayoutDestination {account_id:'PAY-NEW'}) RETURN count(p)`); got != 1 {
		t.Error("new payout destination not linked")
	}
}

func TestFieldLengthLimits(t *testing.T) {
	f := validFNOL()
	f.ClaimantName = strings.Repeat("x", 101)
	f.IncidentLocation = strings.Repeat("y", 201)
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "claimant_name must be at most 100") ||
		!strings.Contains(err.Error(), "incident_location must be at most 200") {
		t.Fatalf("length limits not enforced: %v", err)
	}
}

func TestValidationMessagesAreDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 20; i++ {
		f := validFNOL()
		f.ClaimID, f.PolicyID, f.PayoutAccountID = "bad id", "bad id", "bad id"
		f.PolicyStart, f.IncidentDate = "x", "y"
		msg := f.Validate().Error()
		if i == 0 {
			first = msg
		} else if msg != first {
			t.Fatalf("validation output order varies:\n%s\n%s", first, msg)
		}
	}
}
