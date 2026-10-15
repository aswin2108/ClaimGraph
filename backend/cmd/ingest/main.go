// ingest submits one synthetic FNOL (JSON file) to FalkorDB.
//
//	go run ./cmd/ingest testdata/fnol_sample.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
)

func main() {
	if len(os.Args) != 2 {
		die(fmt.Errorf("usage: ingest <fnol.json>"))
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		die(err)
	}
	var f ingest.FNOL
	if err := json.Unmarshal(raw, &f); err != nil {
		die(fmt.Errorf("parse FNOL: %w", err))
	}

	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	res, err := ingest.Ingest(db.SelectGraph(graph.DefaultGraphName), &f)
	if err != nil {
		die(err)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
