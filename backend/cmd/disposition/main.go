// disposition records a simulated investigator decision on a claim.
//
//	go run ./cmd/disposition CLM-1002 confirmed_fraud "optional notes"
//
// Allowed dispositions: confirmed_fraud, not_substantiated, needs_info.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"claimgraph/internal/disposition"
	"claimgraph/internal/graph"
)

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		die(fmt.Errorf("usage: disposition <claim_id> <confirmed_fraud|not_substantiated|needs_info> [notes]"))
	}
	notes := ""
	if len(os.Args) == 4 {
		notes = os.Args[3]
	}
	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	c, err := disposition.New(db.SelectGraph(graph.DefaultGraphName)).Record(os.Args[1], os.Args[2], notes)
	if err != nil {
		die(err)
	}
	out, _ := json.MarshalIndent(c, "", "  ")
	fmt.Println(string(out))
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
