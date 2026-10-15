// mcp serves ClaimGraph's read-only investigation tools over the Model Context Protocol
// (stdio transport), for MCP clients such as Claude Desktop, Claude Code or Cursor.
//
//	go run ./cmd/mcp
//
// Example client registration (Claude Code):
//
//	claude mcp add claimgraph -- go run ./cmd/mcp        (from the backend directory)
//
// Environment:
//
//	FALKORDB_ADDR   FalkorDB address          (default localhost:6379)
//	GRAPH_NAME      FalkorDB graph to use     (default claimgraph)
//
// stdout carries the protocol; logs go to stderr. The server never writes to the graph.
// Run the seed or the API once first so the graph and its analytics overlay exist.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	logger := log.New(os.Stderr, "claimgraph-mcp ", log.LstdFlags)
	db, err := graph.Connect()
	if err != nil {
		logger.Fatalf("FalkorDB: %v", err)
	}
	name := os.Getenv("GRAPH_NAME")
	if name == "" {
		name = graph.DefaultGraphName
	}
	g := db.SelectGraph(name)
	if err := graphalgo.CheckOverlay(g); err != nil {
		logger.Printf("warning: %v (ring and strongest-link tools will report this until it is built)", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("serving MCP on stdio (graph %q)", name)
	if err := mcpserver.New(g, logger).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logger.Fatalf("mcp: %v", err)
	}
}
