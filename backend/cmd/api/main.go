// api serves the ClaimGraph HTTP API.
//
//	go run ./cmd/api
//
// Environment:
//
//	API_ADDR        listen address            (default 127.0.0.1:8080)
//	CORS_ORIGIN     allowed frontend origin   (default http://localhost:5173)
//	FALKORDB_ADDR   FalkorDB address          (default localhost:6379)
//	AGENT_ENABLED   "false" disables the LLM  (default true)
//	AGENT_MODEL     Gemini model id           (default gemini-3.8-flash)
//	AGENT_MAX_RUNS_PER_HOUR   LLM runs per hour       (default 30)
//	AGENT_DAILY_TOKEN_BUDGET  LLM tokens per UTC day  (default 1000000)
//	API_ALLOWED_HOSTS  extra Host names to accept, comma separated (localhost is always accepted)
//	GRAPH_NAME      FalkorDB graph to use     (default claimgraph_scale, the 20,000-claim book; e2e tests use a scratch graph)
//	DEMO_RESET      "false" disables POST /api/demo/reset (default enabled when the graph has a snapshot)
//	API_Gemini      Gemini API key, normally read from Secrets/.env
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/api"
	"claimgraph/internal/config"
	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"
)

func main() {
	logger := log.New(os.Stderr, "api ", log.LstdFlags)

	if path, names, err := config.LoadSecrets(); err != nil {
		logger.Printf("reading %s failed: %v", path, err)
	} else if path != "" {
		logger.Printf("loaded %d variable(s) from %s", len(names), path) // names/values never logged
	}

	db, err := graph.Connect()
	if err != nil {
		logger.Fatalf("FalkorDB: %v", err)
	}

	g := db.SelectGraph(env("GRAPH_NAME", graph.AppGraphName))
	if r, err := g.ROQuery(`MATCH (c:Claim) RETURN count(c) AS n`, nil, nil); err != nil || !r.Next() || r.Record().Values()[0] == int64(0) {
		logger.Printf("graph %q has no claims yet; load it with `go run ./cmd/seedscale` (or set GRAPH_NAME)", g.Id)
	}
	// Bring the analytics overlay (labels + evidence costs used by the graph algorithms) up to
	// date for data written before it existed. Idempotent; a failure only disables algorithms.
	if err := ingest.EnsureIndexes(g); err != nil {
		logger.Printf("lookup indexes not ensured: %v", err)
	}
	if err := ingest.EnsureOverlay(g); err != nil {
		logger.Printf("analytics overlay not refreshed: %v", err)
	}
	apiServer := api.New(g, env("CORS_ORIGIN", "http://localhost:5173"), logger)
	apiServer.SetAllowedHosts(strings.Split(env("API_ALLOWED_HOSTS", ""), ","))
	// "Reset demo" restores the graph's pristine snapshot (saved by cmd/seedscale). It discards
	// every submitted claim and recorded outcome, so a deployment shared by strangers can turn it off.
	if env("DEMO_RESET", "true") != "false" {
		apiServer.EnableDemoReset(db)
	}

	// The LLM is optional: with AGENT_ENABLED=false or no key, agent runs return the
	// deterministic explanation. Only variable names are ever logged, never values.
	switch key := config.GeminiAPIKey(); {
	case env("AGENT_ENABLED", "true") == "false":
		logger.Printf("agent LLM disabled (AGENT_ENABLED=false); using deterministic explanations")
	case key == "":
		logger.Printf("no Gemini API key found (set API_Gemini in Secrets/.env); using deterministic explanations")
	default:
		p, err := agent.NewGemini(context.Background(), "", key)
		if err != nil {
			logger.Printf("agent LLM unavailable (%v); using deterministic explanations", err)
			break
		}
		a := agent.New(g, p, logger)
		a.Budget = agent.NewBudget(envInt("AGENT_MAX_RUNS_PER_HOUR"), int64(envInt("AGENT_DAILY_TOKEN_BUDGET")))
		apiServer.SetAgent(a)
		st := a.Budget.Status()
		logger.Printf("agent enabled (model %s); LLM budget: %d runs/hour, %d tokens/day", p.Name(), st.MaxRunsPerHour, st.DailyTokenLimit)
	}

	srv := &http.Server{
		Addr:              env("API_ADDR", "127.0.0.1:8080"),
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	logger.Printf("listening on http://%s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
	logger.Println("stopped")
}

// envInt reads a positive integer from the environment; anything else yields 0,
// which the budget constructor turns into its safe default.
func envInt(key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
