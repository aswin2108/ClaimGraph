package demo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

func setup(t *testing.T, name string) (*falkordb.FalkorDB, *falkordb.Graph) {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	for _, n := range []string{name, SnapshotName(name), name + restoreSuffix} {
		_ = db.Conn.Del(context.Background(), n).Err()
	}
	t.Cleanup(func() {
		for _, n := range []string{name, SnapshotName(name), name + restoreSuffix} {
			_ = db.Conn.Del(context.Background(), n).Err()
		}
	})
	return db, db.SelectGraph(name)
}

func submit(t *testing.T, g *falkordb.Graph, id, payout string) {
	t.Helper()
	f := &ingest.FNOL{ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id, PolicyID: "POL-" + id,
		PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active", VIN: fmt.Sprintf("VIN%014d", len(id)),
		IncidentDate: "2026-09-01", FilingDate: "2026-09-02", Amount: 1000, PayoutAccountID: payout, PayoutType: "claimant",
		RepairShopID: "SHOP-" + id}
	if _, err := ingest.Ingest(g, f); err != nil {
		t.Fatalf("ingest %s: %v", id, err)
	}
}

func claimIDs(t *testing.T, g *falkordb.Graph) []string {
	t.Helper()
	r, err := g.ROQuery(`MATCH (c:Claim) RETURN c.claim_id AS id ORDER BY id`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for r.Next() {
		v, _ := r.Record().Get("id")
		ids = append(ids, v.(string))
	}
	return ids
}

func ringOf(t *testing.T, g *falkordb.Graph, id string) []string {
	t.Helper()
	ring, err := graphalgo.RingOf(g, id)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ring.Members {
		ids = append(ids, m.ClaimID)
	}
	if ring.Size != len(ids) { // the size comes from the algorithm result, the members from live facts
		t.Fatalf("ring of %s: size %d but members %v", id, ring.Size, ids)
	}
	return ids
}

func TestRestoreDiscardsEverythingWrittenAfterTheSnapshot(t *testing.T) {
	db, g := setup(t, "claimgraph_test_demo_restore")
	submit(t, g, "A", "PAY-AB")
	submit(t, g, "B", "PAY-AB")
	if err := Snapshot(db, g.Id); err != nil {
		t.Fatal(err)
	}
	submit(t, g, "C", "PAY-AB") // a visitor's claim joins the ring
	if _, err := db.SelectGraph(g.Id).Query(`MATCH (c:Claim {claim_id:'A'}) CREATE (:InvestigationCase {case_id:'X', disposition:'confirmed_fraud'})-[:INVESTIGATES]->(c)`, nil, nil); err != nil {
		t.Fatal(err)
	}

	res, err := Restore(db, g.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimIDs(t, g); !slices.Equal(got, []string{"A", "B"}) || res.Claims != 2 {
		t.Fatalf("after restore: claims %v (reported %d), want [A B]", got, res.Claims)
	}
	r, _ := g.ROQuery(`MATCH (ic:InvestigationCase) RETURN count(ic) AS n`, nil, nil)
	if r.Next() {
		if n, _ := r.Record().Get("n"); n.(int64) != 0 {
			t.Fatalf("a recorded outcome survived the restore")
		}
	}
	// The snapshot itself is untouched, so the demo can be reset again and again.
	submit(t, g, "D", "PAY-AB")
	if _, err := Restore(db, g.Id); err != nil {
		t.Fatal(err)
	}
	if got := claimIDs(t, g); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("second restore: %v", got)
	}
	if ok, _ := db.Conn.Exists(context.Background(), g.Id+restoreSuffix).Result(); ok != 0 {
		t.Fatal("the staging graph must be gone after the swap")
	}
}

// Without a new epoch, the restored graph would repeat evidence versions that a reader has
// already cached for pre-reset data, and a ring lookup would return members that no longer exist.
func TestRestoreNeverServesACachedRingFromBeforeTheReset(t *testing.T) {
	db, g := setup(t, "claimgraph_test_demo_cache")
	submit(t, g, "A", "PAY-AB")
	submit(t, g, "B", "PAY-AB")
	if err := ingest.EnsureOverlay(g); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(db, g.Id); err != nil {
		t.Fatal(err)
	}
	submit(t, g, "C", "PAY-AB")
	if got := ringOf(t, g, "A"); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("before reset: %v", got)
	}
	if _, err := Restore(db, g.Id); err != nil {
		t.Fatal(err)
	}
	submit(t, g, "LONE", "PAY-LONE") // the same number of writes as before the reset
	if got := ringOf(t, g, "A"); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("after reset the ring must not contain C: %v", got)
	}
}

func TestRestoreRefusals(t *testing.T) {
	db, g := setup(t, "claimgraph_test_demo_refuse")
	submit(t, g, "A", "PAY-A")
	if _, err := Restore(db, g.Id); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("restore without a snapshot: %v", err)
	}
	if got := claimIDs(t, g); !slices.Equal(got, []string{"A"}) {
		t.Fatalf("a refused restore changed the graph: %v", got)
	}
	for _, bad := range []string{"", SnapshotName(g.Id), g.Id + restoreSuffix} {
		if _, err := Restore(db, bad); err == nil {
			t.Errorf("restore %q must be refused", bad)
		}
		if err := Snapshot(db, bad); err == nil {
			t.Errorf("snapshot %q must be refused", bad)
		}
	}
	if err := Snapshot(db, "claimgraph_test_demo_missing"); err == nil {
		t.Error("snapshot of a missing graph must fail")
	}
	if ok, err := HasSnapshot(db, g.Id); ok || err != nil {
		t.Fatalf("HasSnapshot = %v, %v", ok, err)
	}
}
