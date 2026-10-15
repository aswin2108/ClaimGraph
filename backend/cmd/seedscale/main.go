// seedscale generates a large synthetic claim book and bulk-loads it into FalkorDB.
//
//	go run ./cmd/seedscale                       # 20,000 claims, 12 planted rings -> graph claimgraph_scale
//	go run ./cmd/seedscale -claims 50000 -rings 20
//
// The load goes into a staging graph that is atomically renamed over the target, so the
// API never reads a half-loaded graph. The ground truth (planted rings) and the showcase
// demo FNOL are written to files; they are never stored in the graph.
//
// Point the API / MCP server at the result with GRAPH_NAME=claimgraph_scale.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"claimgraph/internal/graph"
	"claimgraph/internal/synth"
)

func main() {
	def := synth.DefaultConfig()
	claims := flag.Int("claims", def.Claims, "number of claims to load")
	rings := flag.Int("rings", def.Rings, "number of planted rings (ring 0 is the showcase)")
	seed := flag.Int64("seed", def.Seed, "random seed")
	name := flag.String("graph", "claimgraph_scale", "target graph (replaced atomically)")
	out := flag.String("out", "testdata/scale", "directory for the manifest and the demo FNOL")
	flag.Parse()
	if *name == graph.DefaultGraphName {
		log.Fatalf("refusing to replace the curated demo graph %q; choose another -graph", *name)
	}

	cfg := def
	cfg.Claims, cfg.Rings, cfg.Seed = *claims, *rings, *seed
	t0 := time.Now()
	ds := synth.Generate(cfg)
	fmt.Printf("generated %d claims, %d claimants, %d planted rings in %v\n",
		ds.Stats.Claims, ds.Stats.Claimants, len(ds.Rings), time.Since(t0).Round(time.Millisecond))

	db, err := graph.Connect()
	if err != nil {
		log.Fatalf("FalkorDB: %v", err)
	}
	rep, err := synth.Load(db, *name, ds, func(s string) { fmt.Println("  " + s) })
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	nodes, edges := 0, 0
	for _, n := range rep.Nodes {
		nodes += n
	}
	for _, n := range rep.Edges {
		edges += n
	}
	fmt.Printf("loaded graph %q: %d nodes, %d relationships, %d prior cases in %.1fs (atomic swap: %v)\n",
		*name, nodes, edges, rep.Cases, float64(rep.TotalMs)/1000, rep.Swapped)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	write := func(file string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		path := filepath.Join(*out, file)
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
	write("manifest.json", map[string]any{"graph": *name, "dataset": ds, "load": rep})
	write("demo_fnol.json", ds.Showcase.DemoFNOL)
	fmt.Printf("\nshowcase: submit %s (testdata/scale/demo_fnol.json); it joins a %d-claim ring that has a confirmed prior case.\n",
		ds.Showcase.DemoClaimID, ds.Showcase.RingSize)
	fmt.Printf("busiest repair shop: %s with %d claims (hard negative)\n", ds.Stats.BusiestShop, ds.Stats.BusiestShopClaims)
}
