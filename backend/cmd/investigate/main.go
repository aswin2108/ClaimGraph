// investigate prints the related claims and evidence paths for a target claim.
//
//	go run ./cmd/investigate CLM-1001 [max_hops] [-json]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"claimgraph/internal/graph"
	"claimgraph/internal/investigate"
)

func main() {
	args := os.Args[1:]
	asJSON := false
	var pos []string
	for _, a := range args {
		if a == "-json" {
			asJSON = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		die(fmt.Errorf("usage: investigate <claim_id> [max_hops] [-json]"))
	}
	hops := 0
	if len(pos) == 2 {
		n, err := strconv.Atoi(pos[1])
		if err != nil {
			die(fmt.Errorf("max_hops must be a number"))
		}
		hops = n
	}

	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	res, err := investigate.FindRelatedClaims(db.SelectGraph(graph.DefaultGraphName), pos[0], hops)
	if err != nil {
		die(err)
	}

	if asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("Target %s | max hops %d | %d related claim(s)\n", res.TargetClaim, res.MaxHops, len(res.RelatedClaims))
	if res.Message != "" {
		fmt.Println(res.Message)
	}
	for _, rc := range res.RelatedClaims {
		fmt.Printf("\n%s  (hops=%d, filed=%s)\n", rc.ClaimID, rc.Hops, rc.FilingDate)
		for _, p := range rc.Paths {
			fmt.Println("   ", p.Description)
		}
	}
	if res.Truncated {
		fmt.Println("\n(results truncated by limits)")
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
