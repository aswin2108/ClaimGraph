// seedscale generates a large synthetic claim book and bulk-loads it into FalkorDB.
//
//	go run ./cmd/seedscale                       # 20,000 claims, 12 planted rings -> graph claimgraph_scale
//	go run ./cmd/seedscale -claims 50000 -rings 20
//	seedscale -if-missing -out ""                # Docker: load only if the graph or its snapshot is missing, write no files
//
// The load goes into a staging graph that is atomically renamed over the target, so the
// API never reads a half-loaded graph. The ground truth (planted rings) and the showcase
// demo FNOL are written to files; they are never stored in the graph.
//
// After loading, a pristine snapshot (<graph>__pristine) is saved; POST /api/demo/reset restores
// it, so every visitor starts from the same book.
//
// claimgraph_scale is the API's default graph (graph.AppGraphName).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"claimgraph/internal/demo"
	"claimgraph/internal/graph"
	"claimgraph/internal/synth"
)

func main() {
	def := synth.DefaultConfig()
	claims := flag.Int("claims", def.Claims, "number of claims to load")
	rings := flag.Int("rings", def.Rings, "number of planted rings (ring 0 is the showcase)")
	seed := flag.Int64("seed", def.Seed, "random seed")
	name := flag.String("graph", graph.AppGraphName, "target graph (replaced atomically)")
	out := flag.String("out", "testdata/scale", `directory for the manifest and the demo FNOL ("" = write no files)`)
	ifMissing := flag.Bool("if-missing", false, "do nothing when the graph and its demo snapshot already exist (keeps visitors' data across restarts)")
	flag.Parse()
	if *name == graph.DefaultGraphName {
		log.Fatalf("refusing to replace the curated demo graph %q; choose another -graph", *name)
	}

	db, err := graph.Connect()
	if err != nil {
		log.Fatalf("FalkorDB: %v", err)
	}
	if *ifMissing {
		has, err := demo.HasSnapshot(db, *name)
		if err != nil {
			log.Fatalf("snapshot check: %v", err)
		}
		live, err := db.Conn.Exists(context.Background(), *name).Result()
		if err != nil {
			log.Fatalf("graph check: %v", err)
		}
		if has && live == 1 {
			fmt.Printf("graph %q and its demo snapshot exist; nothing to do (POST /api/demo/reset restores the snapshot)\n", *name)
			return
		}
	}

	cfg := def
	cfg.Claims, cfg.Rings, cfg.Seed = *claims, *rings, *seed
	t0 := time.Now()
	ds := synth.Generate(cfg)
	fmt.Printf("generated %d claims, %d claimants, %d planted rings in %v\n",
		ds.Stats.Claims, ds.Stats.Claimants, len(ds.Rings), time.Since(t0).Round(time.Millisecond))

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
	if err := demo.Snapshot(db, *name); err != nil {
		log.Fatalf("demo snapshot: %v", err)
	}
	fmt.Printf("saved demo snapshot %q (POST /api/demo/reset restores it)\n", demo.SnapshotName(*name))
	if *out == "" {
		return
	}

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
