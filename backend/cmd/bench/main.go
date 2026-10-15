// bench measures investigation latency and detection quality on the scaled synthetic
// book produced by cmd/seedscale, and writes a Markdown report.
//
//	go run ./cmd/bench                       # graph claimgraph_scale, manifest testdata/scale/manifest.json
//
// Latency: every investigation operation on three samples (planted-ring claims, claims of
// the busiest repair shop, random claims). Quality: exact ring recovery by WCC, where the
// planted rings rank in the book-wide ring list, and the score bands of ring claims vs
// other claims. Results describe this synthetic book only, not real-world accuracy.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/investigate"
	"claimgraph/internal/scoring"
	"claimgraph/internal/synth"

	"github.com/FalkorDB/falkordb-go/v2"
)

type manifest struct {
	Graph   string `json:"graph"`
	Dataset struct {
		Rings    []synth.PlantedRing `json:"planted_rings"`
		Stats    synth.Stats         `json:"stats"`
		Showcase synth.Showcase      `json:"showcase"`
	} `json:"dataset"`
	Load synth.LoadReport `json:"load"`
}

type timings map[string][]time.Duration

func (t timings) add(op string, d time.Duration) { t[op] = append(t[op], d) }

func main() {
	name := flag.String("graph", "claimgraph_scale", "graph to benchmark")
	manPath := flag.String("manifest", "testdata/scale/manifest.json", "manifest written by cmd/seedscale")
	randomN := flag.Int("random", 200, "random claims to sample")
	out := flag.String("out", "testdata/scale/benchmark.md", "report path ('' = stdout only)")
	flag.Parse()

	var man manifest
	b, err := os.ReadFile(*manPath)
	if err != nil {
		log.Fatalf("read manifest: %v (run go run ./cmd/seedscale first)", err)
	}
	if err := json.Unmarshal(b, &man); err != nil {
		log.Fatal(err)
	}
	db, err := graph.Connect()
	if err != nil {
		log.Fatal(err)
	}
	g := db.SelectGraph(*name)
	if err := graphalgo.CheckOverlay(g); err != nil {
		log.Fatal(err)
	}

	ringOf := map[string]int{}
	var ringClaims []string
	for _, r := range man.Dataset.Rings {
		for _, c := range r.Claims {
			ringOf[c] = r.Index
			ringClaims = append(ringClaims, c)
		}
	}
	busy := column(g, `MATCH (c:Claim)-[:REPAIRED_AT]->(:RepairShop {shop_id:$s}) RETURN c.claim_id AS id ORDER BY id LIMIT 50`,
		map[string]any{"s": man.Dataset.Stats.BusiestShop})
	all := column(g, `MATCH (c:Claim) RETURN c.claim_id AS id ORDER BY id`, nil)
	rnd := rand.New(rand.NewSource(7))
	rnd.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	var random []string
	for _, c := range all {
		if _, inRing := ringOf[c]; !inRing && len(random) < *randomN {
			random = append(random, c)
		}
	}

	var r strings.Builder
	fmt.Fprintf(&r, "# ClaimGraph scale benchmark\n\nGraph `%s`: %d claims, %d claimants, %d nodes / %d relationships, %d planted rings. ",
		*name, man.Dataset.Stats.Claims, man.Dataset.Stats.Claimants, sum(man.Load.Nodes), sum(man.Load.Edges), len(man.Dataset.Rings))
	fmt.Fprintf(&r, "Bulk load %.1f s. Busiest repair shop %s serves %d claims. Synthetic data; results describe this book only.\n\n",
		float64(man.Load.TotalMs)/1000, man.Dataset.Stats.BusiestShop, man.Dataset.Stats.BusiestShopClaims)

	// ---- latency ----
	type sample struct {
		label  string
		claims []string
	}
	samples := []sample{{"planted-ring claims", ringClaims}, {"busiest-shop claims", busy}, {"random claims", random}}
	r.WriteString("## Latency (ms, wall clock from Go, FalkorDB in local Docker)\n\n")
	r.WriteString("| sample | operation | n | p50 | p95 | max | errors |\n|---|---|---|---|---|---|---|\n")
	bands := map[string]map[string]int{}
	truncated := map[string]int{}
	withInfra := map[string]int{}
	trimmed := map[string]int{}
	factorHits := map[string]map[string]int{} // sample -> factor -> claims where it fired
	scoreSum := map[string]int{}
	household := map[string]int{}
	// The first ring lookup in a process runs algo.WCC and algo.MSF over the whole book;
	// later lookups reuse those results until the evidence version changes.
	t0 := time.Now()
	if _, err := graphalgo.RingOf(g, ringClaims[0]); err != nil {
		log.Fatal(err)
	}
	coldRing := time.Since(t0)
	for _, s := range samples {
		t := timings{}
		errs := map[string]int{}
		bands[s.label] = map[string]int{}
		for _, c := range s.claims {
			for hops := 1; hops <= 3; hops++ {
				op := fmt.Sprintf("FindRelatedClaims hops=%d", hops)
				t0 := time.Now()
				res, err := investigate.FindRelatedClaims(g, c, hops)
				t.add(op, time.Since(t0))
				if err != nil {
					errs[op]++
				} else if hops == 3 {
					if res.Truncated {
						truncated[s.label]++
					}
					if res.PathsTrimmed {
						trimmed[s.label]++
					}
					if len(res.InfrastructureLinks) > 0 {
						withInfra[s.label]++
					}
				}
			}
			t0 := time.Now()
			rep, err := scoring.Analyze(g, c, 3)
			t.add("Analyze (traversal + score + prior cases)", time.Since(t0))
			if err != nil {
				errs["Analyze (traversal + score + prior cases)"]++
			} else {
				bands[s.label][rep.Band]++
				scoreSum[s.label] += rep.Score
				if rep.HouseholdExplained {
					household[s.label]++
				}
				if factorHits[s.label] == nil {
					factorHits[s.label] = map[string]int{}
				}
				for _, f := range rep.Factors {
					factorHits[s.label][f.Name]++
				}
			}
			t0 = time.Now()
			_, err = scoring.RunCounterfactual(g, c, 3, investigate.CatRepairShop)
			t.add("Counterfactual (exclude repair_shop)", time.Since(t0))
			if err != nil {
				errs["Counterfactual (exclude repair_shop)"]++
			}
			t0 = time.Now()
			_, err = graphalgo.RingOf(g, c)
			t.add("GetClaimRing (cached WCC/MSF + live facts)", time.Since(t0))
			if err != nil {
				errs["GetClaimRing (cached WCC/MSF + live facts)"]++
			}
		}
		for _, op := range sortedOps(t) {
			p50, p95, mx := pct(t[op])
			fmt.Fprintf(&r, "| %s | %s | %d | %d | %d | %d | %d |\n", s.label, op, len(t[op]), p50, p95, mx, errs[op])
		}
		log.Printf("latency sample %q done", s.label)
	}
	// Book-wide algorithms and strongest links.
	t := timings{}
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		if _, err := graphalgo.ListRings(g, 3, 50); err != nil {
			log.Fatal(err)
		}
		t.add("ListRings (algo.WCC over the whole book)", time.Since(t0))
	}
	linkErrs := 0
	for _, pr := range man.Dataset.Rings {
		t0 := time.Now()
		l, err := graphalgo.FindStrongestLink(g, pr.Claims[0], pr.Claims[len(pr.Claims)-1])
		if err != nil || !l.Found {
			linkErrs++
			log.Printf("strongest link across ring %d: found=%v err=%v", pr.Index, l != nil && l.Found, err)
		}
		t.add("GetStrongestLink (algo.SPpaths) across a ring", time.Since(t0))
	}
	for _, op := range sortedOps(t) {
		p50, p95, mx := pct(t[op])
		errs := 0
		if strings.HasPrefix(op, "GetStrongestLink") {
			errs = linkErrs
		}
		fmt.Fprintf(&r, "| book-wide | %s | %d | %d | %d | %d | %d |\n", op, len(t[op]), p50, p95, mx, errs)
	}
	fmt.Fprintf(&r, "| book-wide | GetClaimRing first call (cold: algo.WCC + algo.MSF over the whole book) | 1 | %d | %d | %d | 0 |\n",
		coldRing.Milliseconds(), coldRing.Milliseconds(), coldRing.Milliseconds())
	fmt.Fprintf(&r, "\nResults cut by a limit at hops=3 (flagged `truncated`): ")
	for _, s := range samples {
		fmt.Fprintf(&r, "%s %d/%d; ", s.label, truncated[s.label], len(s.claims))
	}
	fmt.Fprintf(&r, "\n\nClaims showing only %d example paths for some multi-hop connection (presentation cap, no evidence lost): ", investigate.MaxPathsPerClaim)
	for _, s := range samples {
		fmt.Fprintf(&r, "%s %d/%d; ", s.label, trimmed[s.label], len(s.claims))
	}
	fmt.Fprintf(&r, "\n\nClaims that use shared infrastructure (a shop, carrier IP, kiosk or vendor account used by more than %d claims; counted once, not as connections): ", investigate.InfrastructureMinClaims)
	for _, s := range samples {
		fmt.Fprintf(&r, "%s %d/%d; ", s.label, withInfra[s.label], len(s.claims))
	}
	r.WriteString("\n\n")

	// ---- ring recovery ----
	r.WriteString("## Ring recovery (algo.WCC over strong evidence)\n\n| ring | shape | planted claims | WCC claims | exact | confirmed case | rank in ring list |\n|---|---|---|---|---|---|---|\n")
	list, err := graphalgo.ListRings(g, 3, graphalgo.MaxListRings)
	if err != nil {
		log.Fatal(err)
	}
	rankOf := map[string]int{}
	for i, rs := range list.Rings {
		rankOf[rs.RingID] = i + 1
	}
	exact := 0
	for _, pr := range man.Dataset.Rings {
		ring, err := graphalgo.RingOf(g, pr.Claims[0])
		if err != nil {
			log.Fatal(err)
		}
		var got []string
		for _, m := range ring.Members {
			got = append(got, m.ClaimID)
		}
		ok := strings.Join(got, ",") == strings.Join(pr.Claims, ",")
		if ok {
			exact++
		}
		rank := "-"
		if n := rankOf[ring.RingID]; n > 0 {
			rank = fmt.Sprint(n)
		}
		fmt.Fprintf(&r, "| %d | %s | %d | %d | %v | %v | %s |\n", pr.Index, pr.Shape, len(pr.Claims), ring.Size, ok, pr.ConfirmedCase != "", rank)
	}
	inTop := func(k int) int {
		n := 0
		for i, rs := range list.Rings {
			if i < k && ringOf[strings.TrimPrefix(rs.RingID, "RING-")] >= 0 {
				if _, planted := ringOf[strings.TrimPrefix(rs.RingID, "RING-")]; planted {
					n++
				}
			}
		}
		return n
	}
	k := len(man.Dataset.Rings)
	fmt.Fprintf(&r, "\nExact recovery: %d/%d planted rings. Book-wide WCC found %d components with >= 3 claims (households and repeat claimants form legitimate ones). ",
		exact, len(man.Dataset.Rings), list.TotalRings)
	fmt.Fprintf(&r, "Planted rings in the top %d of the ring list: %d; in the top %d: %d.\n\n", k, inTop(k), graphalgo.MaxListRings, inTop(graphalgo.MaxListRings))
	r.WriteString("Top of the ring list:\n\n| rank | ring | size | span days | confirmed | planted? |\n|---|---|---|---|---|---|\n")
	for i, rs := range list.Rings {
		if i == 15 {
			break
		}
		_, planted := ringOf[strings.TrimPrefix(rs.RingID, "RING-")]
		fmt.Fprintf(&r, "| %d | %s | %d | %d | %d | %v |\n", i+1, rs.RingID, rs.Size, rs.SpanDays, rs.ConfirmedFraudCases, planted)
	}

	// ---- score bands ----
	r.WriteString("\n## Investigation-priority bands (deterministic score, hops=3)\n\n| sample | Low | Review | High Priority |\n|---|---|---|---|\n")
	for _, s := range samples {
		bb := bands[s.label]
		fmt.Fprintf(&r, "| %s | %d | %d | %d |\n", s.label, bb[scoring.BandLow], bb[scoring.BandReview], bb[scoring.BandHigh])
	}
	r.WriteString("\nThe score uses direct (1-transition) evidence only; ring membership beyond that is shown by the ring tools and never changes the score.\n")

	// What actually drives the scores: share of claims where each factor fired.
	r.WriteString("\n## What drives the score (share of claims where each factor fired)\n\n| factor | points |")
	for _, s := range samples {
		fmt.Fprintf(&r, " %s |", s.label)
	}
	r.WriteString("\n|---|---|")
	for range samples {
		r.WriteString("---|")
	}
	r.WriteString("\n")
	factorOrder := []struct {
		name string
		pts  int
	}{
		{scoring.FSharedPayout, scoring.PtsPayout}, {scoring.FSharedDevice, scoring.PtsDevice}, {scoring.FSharedContact, scoring.PtsContact},
		{scoring.FSharedAddress, scoring.PtsAddress}, {scoring.FSharedRepair, scoring.PtsRepairShop}, {scoring.FSharedPublicIP, scoring.PtsPublicIP},
		{scoring.FClaimBreadth, scoring.PtsClaimsCap}, {scoring.FTemporal, scoring.PtsTemporal}, {scoring.FBurst, scoring.PtsBurst},
		{scoring.FNewPolicy, scoring.PtsNewPolicy}, {scoring.FConfirmedFraud, scoring.PtsConfirmed}, {scoring.FReinforcement, scoring.PtsReinforce},
	}
	for _, f := range factorOrder {
		fmt.Fprintf(&r, "| %s | %d |", f.name, f.pts)
		for _, s := range samples {
			fmt.Fprintf(&r, " %d%% |", 100*factorHits[s.label][f.name]/len(s.claims))
		}
		r.WriteString("\n")
	}
	r.WriteString("| household-explained (bonus withheld) | - |")
	for _, s := range samples {
		fmt.Fprintf(&r, " %d%% |", 100*household[s.label]/len(s.claims))
	}
	r.WriteString("\n| average score | - |")
	for _, s := range samples {
		fmt.Fprintf(&r, " %d |", scoreSum[s.label]/len(s.claims))
	}
	r.WriteString("\n")

	fmt.Print(r.String())
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*out, []byte(r.String()), 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s", *out)
	}
}

func column(g *falkordb.Graph, q string, p map[string]any) []string {
	res, err := g.ROQuery(q, p, nil)
	if err != nil {
		log.Fatal(err)
	}
	var out []string
	for res.Next() {
		v, _ := res.Record().Get("id")
		out = append(out, v.(string))
	}
	return out
}

func pct(ds []time.Duration) (p50, p95, mx int64) {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) int64 { return s[int(q*float64(len(s)-1))].Milliseconds() }
	return at(0.5), at(0.95), s[len(s)-1].Milliseconds()
}

func sortedOps(t timings) []string {
	var ops []string
	for op := range t {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
