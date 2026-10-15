// dbcheck verifies the Go backend can reach FalkorDB, write, read, and clean up.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/FalkorDB/falkordb-go/v2"
)

func main() {
	addr := os.Getenv("FALKORDB_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	db, err := falkordb.FalkorDBNew(&falkordb.ConnectionOption{Addr: addr})
	if err != nil {
		fail("connect", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Conn.Ping(ctx).Err(); err != nil {
		fail("ping", err)
	}
	fmt.Println("PING ok:", addr)

	g := db.SelectGraph("claimgraph_dbcheck")
	defer g.Delete() // scratch graph, never touches real data

	if _, err := g.Query("MERGE (c:Claim {claim_id: $id})", map[string]interface{}{"id": "CHECK-001"}, nil); err != nil {
		fail("write", err)
	}

	res, err := g.ROQuery("MATCH (c:Claim {claim_id: $id}) RETURN c.claim_id", map[string]interface{}{"id": "CHECK-001"}, nil)
	if err != nil {
		fail("read", err)
	}
	if !res.Next() {
		fail("read", fmt.Errorf("claim not found after write"))
	}
	v, _ := res.Record().Get("c.claim_id")
	fmt.Println("Read back claim_id:", v)
	fmt.Println("FalkorDB connection OK")
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "FAILED at %s: %v\n", step, err)
	os.Exit(1)
}
