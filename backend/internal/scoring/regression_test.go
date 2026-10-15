package scoring

import (
	"fmt"
	"testing"

	"claimgraph/internal/ingest"
)

func tow(id string) mod { return func(f *ingest.FNOL) { f.TowProviderID = id } }

// Tow provider is context-only: it must not create connected claims or any points.
func TestTowProviderIsContextOnly(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_tow")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", tow("TOW-1")), mk("B", "2026-09-11", tow("TOW-1")),
		mk("C", "2026-09-12", tow("TOW-1")), mk("D1", "2026-09-13", tow("TOW-1")),
	}, nil)
	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 || r.ConnectedClaims != 0 {
		t.Fatalf("shared tow provider must earn nothing: score=%d connected=%d", r.Score, r.ConnectedClaims)
	}
}

// Contact evidence goes through two claimants; the reported path must show them.
func TestContactPathShowsClaimantHop(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_claimanthop")
	load(t, g, []*ingest.FNOL{
		mk("T", "2026-09-10", phone("+1-555-0100")), mk("B", "2026-09-11", phone("+1-555-0100"))}, nil)
	r, _ := Analyze(g, "T", 3)
	got := r.Investigation.RelatedClaims[0].Paths[0].Description
	want := "T -[FILED_BY]-> Claimant(DRV-T) -[HAS_CONTACT]-> ContactPoint(+1-555-0100) <-[HAS_CONTACT]- Claimant(DRV-B) <-[FILED_BY]- B"
	if got != want {
		t.Fatalf("path description:\n got %s\nwant %s", got, want)
	}
}

// Dense layered graph: path fan-out must stay bounded and the result must say so.
func TestDenseLayersStayBounded(t *testing.T) {
	g := scratch(t, "claimgraph_test_scoring_layers")
	var fs []*ingest.FNOL
	add := func(prefix string, n int, mods ...mod) {
		for i := 0; i < n; i++ {
			fs = append(fs, mk(fmt.Sprintf("%s%02d", prefix, i), "2026-01-01", mods...))
		}
	}
	fs = append(fs, mk("T", "2026-09-10", device("DX")))
	add("A", 25, device("DX"), payout("PY"))       // layer 1: shares device with T
	add("B", 25, payout("PY"), shop("SZ"))         // layer 2: shares payout with layer 1
	add("C", 25, shop("SZ"), pubIP("203.0.113.9")) // layer 3: shares shop with layer 2
	load(t, g, fs, nil)

	r, err := Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range r.Investigation.RelatedClaims {
		if rc.Hops > 1 && len(rc.Paths) > 5 {
			t.Fatalf("%s kept %d paths, cap is 5", rc.ClaimID, len(rc.Paths))
		}
	}
	if !r.Investigation.Truncated {
		t.Error("truncation must be reported when fan-out limits are hit")
	}
	if r.Score > 100 {
		t.Fatalf("score above cap: %d", r.Score)
	}
}

// Hitting the row limit must be reported, never silent.
func TestRowLimitReported(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	g := scratch(t, "claimgraph_test_scoring_rowlimit")
	for i := 0; i < 520; i++ {
		if _, err := ingest.Ingest(g, mk(fmt.Sprintf("R%03d", i), "2026-01-01", shop("SHOP-BIG"))); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Analyze(g, "R000", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Investigation.Truncated || r.Score > 100 {
		t.Fatalf("expected truncation flag on a 519-neighbour shop; truncated=%v score=%d", r.Investigation.Truncated, r.Score)
	}
}
