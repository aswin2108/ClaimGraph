// score prints the deterministic investigation-priority report for a claim,
// optionally with a counterfactual that ignores one evidence category.
//
//	go run ./cmd/score CLM-1001
//	go run ./cmd/score CLM-1001 -exclude repair_shop
//	go run ./cmd/score CLM-1001 -json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"claimgraph/internal/graph"
	"claimgraph/internal/scoring"
)

func main() {
	exclude := flag.String("exclude", "", "evidence category to ignore (payout, device, contact, address, repair_shop, public_ip)")
	asJSON := flag.Bool("json", false, "print full JSON")
	hops := flag.Int("hops", 0, "max claim-to-claim hops (1-3, default 3)")
	// Allow the claim ID before the flags.
	args := os.Args[1:]
	claim := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		claim, args = args[0], args[1:]
	}
	_ = flag.CommandLine.Parse(args)
	if claim == "" && flag.NArg() > 0 {
		claim = flag.Arg(0)
	}
	if claim == "" {
		die(fmt.Errorf("usage: score <claim_id> [-exclude category] [-hops n] [-json]"))
	}

	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	g := db.SelectGraph(graph.DefaultGraphName)

	if *exclude != "" {
		cf, err := scoring.RunCounterfactual(g, claim, *hops, *exclude)
		if err != nil {
			die(err)
		}
		if *asJSON {
			dump(cf)
			return
		}
		fmt.Printf("COUNTERFACTUAL for %s: ignore %q\n", claim, cf.ExcludedCategory)
		fmt.Printf("  original:       %3d  %s\n", cf.Original.Score, cf.Original.Band)
		fmt.Printf("  counterfactual: %3d  %s   (delta %+d)\n", cf.Counterfactual.Score, cf.Counterfactual.Band, cf.Delta)
		for _, c := range cf.RemovedFactors {
			fmt.Printf("  - %-28s %3d -> %d\n", c.Name, c.OriginalPoints, c.CounterfactualPoints)
		}
		fmt.Println("  " + cf.Explanation)
		fmt.Printf("  surviving connected claims: %d\n", cf.Counterfactual.ConnectedClaims)
		return
	}

	r, err := scoring.Analyze(g, claim, *hops)
	if err != nil {
		die(err)
	}
	if *asJSON {
		dump(r)
		return
	}
	fmt.Printf("%s  score %d  [%s]  %s\n", r.TargetClaim, r.Score, r.Band, r.BandMeaning)
	fmt.Printf("connected claims: %d\n\nfactors:\n", r.ConnectedClaims)
	for _, f := range r.Factors {
		fmt.Printf("  +%-3d %-28s %s\n", f.Points, f.Name, f.Reason)
		if len(f.Entities) > 0 {
			fmt.Printf("       entities=%v claims=%v\n", f.Entities, f.Claims)
		}
	}
	if len(r.PriorCases) > 0 {
		fmt.Println("\nprior cases:")
		for _, c := range r.PriorCases {
			fmt.Printf("  %s on %s: %s\n", c.CaseID, c.ClaimID, c.Disposition)
		}
	}
	fmt.Println("\nconsider:")
	for _, n := range r.Uncertainty {
		fmt.Println("  -", n)
	}
	fmt.Println("\n" + r.Disclaimer)
}

func dump(v any) {
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
