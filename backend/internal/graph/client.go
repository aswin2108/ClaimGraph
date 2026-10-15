// Package graph wraps the FalkorDB connection used by the rest of the backend.
package graph

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/FalkorDB/falkordb-go/v2"
)

// DefaultGraphName is the graph that holds all ClaimGraph data.
const DefaultGraphName = "claimgraph"

// Connect opens a FalkorDB connection (FALKORDB_ADDR, default localhost:6379)
// and verifies it with a PING.
func Connect() (*falkordb.FalkorDB, error) {
	addr := os.Getenv("FALKORDB_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	// ReadTimeout is above every server-side query timeout we set (at most 6 s), so FalkorDB
	// always aborts a slow query first and reports it, instead of the client giving up blind.
	db, err := falkordb.FalkorDBNew(&falkordb.ConnectionOption{Addr: addr, ReadTimeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Conn.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping %s: %w", addr, err)
	}
	return db, nil
}
