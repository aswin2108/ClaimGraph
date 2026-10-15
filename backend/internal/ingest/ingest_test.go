package ingest

import (
	"errors"
	"strings"
	"testing"

	"claimgraph/internal/graph"
)

func validFNOL() *FNOL {
	return &FNOL{
		ClaimID: "CLM-2001", ClaimantID: "CLM-DRV-20", ClaimantName: "Test Driver",
		PolicyID: "POL-7001", PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: "1HGSYN00000000020", IncidentDate: "2026-09-10", FilingDate: "2026-09-11",
		Amount: 4000, PayoutAccountID: "PAY-T1", PayoutType: "claimant", RepairShopID: "SHOP-T1",
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *FNOL)
		wantErr string // substring; empty = valid
	}{
		{"valid minimal", func(f *FNOL) {}, ""},
		{"missing claim id", func(f *FNOL) { f.ClaimID = "" }, "claim_id is required"},
		{"injection-style id rejected", func(f *FNOL) { f.ClaimID = "x'}) DETACH DELETE (n" }, "claim_id must be"},
		{"bad vin", func(f *FNOL) { f.VIN = "short" }, "vin must be 17"},
		{"zero amount", func(f *FNOL) { f.Amount = 0 }, "amount must be"},
		{"huge amount", func(f *FNOL) { f.Amount = 5_000_000 }, "amount must be"},
		{"bad payout type", func(f *FNOL) { f.PayoutType = "crypto" }, "payout_type"},
		{"bad policy status", func(f *FNOL) { f.PolicyStatus = "weird" }, "policy_status"},
		{"bad date", func(f *FNOL) { f.IncidentDate = "09/10/2026" }, "incident_date must be a valid date"},
		{"filed before incident", func(f *FNOL) { f.FilingDate = "2026-09-01" }, "filing_date must not be before"},
		{"policy end before start", func(f *FNOL) { f.PolicyEnd = "2025-01-01" }, "policy_end must not be before"},
		{"contact needs both", func(f *FNOL) { f.ContactType = "phone" }, "supplied together"},
		{"contact bad type", func(f *FNOL) { f.ContactType, f.ContactValue = "fax", "123" }, "contact_type must be"},
		{"bad ip", func(f *FNOL) { f.IPAddress = "999.1.1.1" }, "ip_address"},
		{"valid with optionals", func(f *FNOL) {
			f.ContactType, f.ContactValue, f.IPAddress, f.DeviceID = "email", "a@example.test", "203.0.113.5", "DEV-T1"
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validFNOL()
			tc.mutate(f)
			err := f.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPolicyChecks(t *testing.T) {
	f := validFNOL()
	if s := PolicyCheckStatus(f.PolicyChecks()); s != "passed" {
		t.Fatalf("clean claim should pass, got %s", s)
	}
	f.PolicyStatus = "lapsed"
	if s := PolicyCheckStatus(f.PolicyChecks()); s != "failed" {
		t.Fatalf("lapsed policy should fail, got %s", s)
	}
	f = validFNOL()
	f.IncidentDate, f.FilingDate = "2025-06-01", "2025-06-02"
	if s := PolicyCheckStatus(f.PolicyChecks()); s != "failed" {
		t.Fatalf("incident outside coverage should fail, got %s", s)
	}
}

func TestIPIsPublic(t *testing.T) {
	for ip, want := range map[string]bool{"203.0.113.5": true, "10.0.0.4": false, "192.168.1.9": false, "127.0.0.1": false} {
		if got := IPIsPublic(ip); got != want {
			t.Errorf("IPIsPublic(%s)=%v want %v", ip, got, want)
		}
	}
}

// Integration test against a scratch graph; skipped when FalkorDB is unreachable.
func TestIngestIdempotent(t *testing.T) {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph("claimgraph_test_ingest")
	defer g.Delete()

	f := validFNOL()
	f.ContactType, f.ContactValue = "phone", "+1-555-0100"
	f.AddressID, f.DeviceID, f.IPAddress, f.TowProviderID = "ADDR-T1", "DEV-T1", "203.0.113.5", "TOW-T1"

	first, err := Ingest(g, f)
	if err != nil || !first.Created || first.PolicyCheckStatus != "passed" {
		t.Fatalf("first ingest: %+v err=%v", first, err)
	}
	second, err := Ingest(g, f)
	if err != nil || second.Created {
		t.Fatalf("second ingest should report existing claim: %+v err=%v", second, err)
	}

	count := func(q string) int64 {
		res, err := g.ROQuery(q, nil, nil)
		if err != nil || !res.Next() {
			t.Fatalf("count query failed: %v", err)
		}
		v, _ := res.Record().GetByIndex(0)
		return v.(int64)
	}
	if n := count(`MATCH (n) WHERE NOT n:GraphMeta RETURN count(n)`); n != 11 { // GraphMeta = version marker
		t.Errorf("want 11 nodes after double ingest, got %d", n)
	}
	if n := count(`MATCH ()-[r]->() RETURN count(r)`); n != 10 {
		t.Errorf("want 10 edges after double ingest, got %d", n)
	}
	if n := count(`MATCH (ip:IPAddress {is_public:true}) RETURN count(ip)`); n != 1 {
		t.Errorf("public IP flag not stored, got %d", n)
	}
}

func TestInvalidFNOLWritesNothing(t *testing.T) {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph("claimgraph_test_ingest_invalid")
	defer g.Delete()

	f := validFNOL()
	f.Amount = -5
	if _, err := Ingest(g, f); err == nil {
		t.Fatal("expected validation error")
	}
	if res, err := g.ROQuery(`MATCH (n) RETURN count(n)`, nil, nil); err == nil && res.Next() {
		if v, _ := res.Record().GetByIndex(0); v.(int64) != 0 {
			t.Fatalf("invalid FNOL wrote %v nodes", v)
		}
	}
}
