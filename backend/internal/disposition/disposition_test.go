package disposition_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"claimgraph/internal/disposition"
	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

func claim(id, filed string, mods ...func(*ingest.FNOL)) *ingest.FNOL {
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: ("VIN" + id + strings.Repeat("0", 14))[:17], IncidentDate: filed, FilingDate: filed, Amount: 1000,
		PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	for _, m := range mods {
		m(f)
	}
	return f
}

func setup(t *testing.T, name string, fs ...*ingest.FNOL) (*falkordb.Graph, *disposition.Recorder) {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	for _, f := range fs {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatal(err)
		}
	}
	rec := disposition.New(g)
	rec.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
	return g, rec
}

func count(t *testing.T, g *falkordb.Graph, q string) int64 {
	r, err := g.ROQuery(q, nil, nil)
	if err != nil || !r.Next() {
		t.Fatalf("count %q: %v", q, err)
	}
	v, _ := r.Record().GetByIndex(0)
	return v.(int64)
}

func TestRecordCreatesCaseLinkedToClaim(t *testing.T) {
	g, rec := setup(t, "claimgraph_test_disp_create", claim("C1", "2026-09-01"))
	c, err := rec.Record("C1", disposition.ConfirmedFraud, "  synthetic reviewer note ")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Created || c.CaseID != "CASE-C1" || c.RecordedAt != "2026-10-15T12:00:00Z" || c.Notes != "synthetic reviewer note" {
		t.Fatalf("unexpected case: %+v", c)
	}
	if n := count(t, g, `MATCH (:InvestigationCase {disposition:'confirmed_fraud'})-[:INVESTIGATES]->(:Claim {claim_id:'C1'}) RETURN count(*)`); n != 1 {
		t.Fatalf("case not linked to claim, count=%d", n)
	}
}

func TestIdempotentAndUpdatable(t *testing.T) {
	g, rec := setup(t, "claimgraph_test_disp_idem", claim("C1", "2026-09-01"))
	if _, err := rec.Record("C1", disposition.NeedsInfo, "first look"); err != nil {
		t.Fatal(err)
	}
	again, err := rec.Record("C1", disposition.NeedsInfo, "first look")
	if err != nil || again.Created || again.Previous != "" {
		t.Fatalf("re-recording same outcome: %+v err=%v", again, err)
	}
	upd, err := rec.Record("C1", disposition.NotSubstantiated, "documents checked")
	if err != nil || upd.Created || upd.Previous != disposition.NeedsInfo {
		t.Fatalf("update should report previous disposition: %+v err=%v", upd, err)
	}
	if n := count(t, g, `MATCH (n:InvestigationCase) RETURN count(n)`); n != 1 {
		t.Fatalf("want exactly one case node, got %d", n)
	}
	if n := count(t, g, `MATCH ()-[r:INVESTIGATES]->() RETURN count(r)`); n != 1 {
		t.Fatalf("want exactly one INVESTIGATES edge, got %d", n)
	}
	if n := count(t, g, `MATCH (n:InvestigationCase {disposition:'not_substantiated'}) RETURN count(n)`); n != 1 {
		t.Fatal("latest disposition not stored")
	}
}

func TestRejectsBadInput(t *testing.T) {
	g, rec := setup(t, "claimgraph_test_disp_bad", claim("C1", "2026-09-01"))
	cases := []struct {
		name            string
		claim, disp, nt string
		wantNotFound    bool
	}{
		{"bad disposition", "C1", "guilty", "", false},
		{"empty disposition", "C1", "", "", false},
		{"injection id", "C1'}) DETACH DELETE (n", disposition.NeedsInfo, "", false},
		{"empty id", "", disposition.NeedsInfo, "", false},
		{"long notes", "C1", disposition.NeedsInfo, strings.Repeat("x", 501), false},
		{"unknown claim", "NOPE", disposition.NeedsInfo, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rec.Record(tc.claim, tc.disp, tc.nt)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantNotFound && !errors.Is(err, disposition.ErrClaimNotFound) {
				t.Fatalf("want ErrClaimNotFound, got %v", err)
			}
		})
	}
	if n := count(t, g, `MATCH (n:InvestigationCase) RETURN count(n)`); n != 0 {
		t.Fatalf("rejected inputs must write nothing, found %d cases", n)
	}
}

func TestNotesSanitized(t *testing.T) {
	_, rec := setup(t, "claimgraph_test_disp_notes", claim("C1", "2026-09-01"))
	c, err := rec.Record("C1", disposition.NeedsInfo, "ok\x00\x07 text")
	if err != nil || c.Notes != "ok text" {
		t.Fatalf("control characters should be stripped: %+v err=%v", c, err)
	}
}

// End to end memory loop: record -> later related claim retrieves it -> only
// confirmed_fraud earns points -> changing the outcome changes the next score.
func TestMemoryLoopFeedsLaterInvestigation(t *testing.T) {
	shared := func(f *ingest.FNOL) { f.PayoutAccountID = "PAY-SHARED"; f.DeviceID = "DEV-SHARED" }
	g, rec := setup(t, "claimgraph_test_disp_loop",
		claim("OLD", "2026-07-01", shared), // older claim, already investigated
		claim("NEW", "2026-09-10", shared), // new claim being triaged
	)

	base, err := scoring.Analyze(g, "NEW", 3)
	if err != nil || base.Score != 60 || len(base.PriorCases) != 0 {
		t.Fatalf("baseline want 60 and no prior cases: %+v err=%v", base, err)
	}

	step := func(disp string, wantScore int, wantCases int) {
		t.Helper()
		if _, err := rec.Record("OLD", disp, "synthetic"); err != nil {
			t.Fatal(err)
		}
		r, err := scoring.Analyze(g, "NEW", 3)
		if err != nil {
			t.Fatal(err)
		}
		if r.Score != wantScore || len(r.PriorCases) != wantCases {
			t.Fatalf("after %s: score=%d cases=%d, want %d/%d", disp, r.Score, len(r.PriorCases), wantScore, wantCases)
		}
		if r.PriorCases[0].Disposition != disp || r.PriorCases[0].ClaimID != "OLD" {
			t.Fatalf("prior case not retrieved correctly: %+v", r.PriorCases)
		}
	}
	step(disposition.ConfirmedFraud, 75, 1)   // +15 historical bonus (T06)
	step(disposition.NotSubstantiated, 60, 1) // retrieved, but no bonus (T23/T25)
	step(disposition.NeedsInfo, 60, 1)        // unresolved, no bonus (T24)

	// Scoring rules are unchanged by outcomes: an unrelated claim is unaffected.
	if n := count(t, g, `MATCH (c:Claim) RETURN count(c)`); n != 2 {
		t.Fatalf("recording dispositions must not create or remove claims, have %d", n)
	}
}
