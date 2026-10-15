// agent runs the bounded investigation agent on one claim and prints the result.
//
//	go run ./cmd/agent CLM-1001
//	go run ./cmd/agent CLM-1001 -no-llm              deterministic explanation only
//	go run ./cmd/agent CLM-1001 -decision needs_info -notes "synthetic note"
//	go run ./cmd/agent CLM-1001 -json
//
// The LLM needs the Gemini key (API_Gemini in Secrets/.env); AGENT_MODEL overrides the model.
// GRAPH_NAME selects the graph (default claimgraph).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"claimgraph/internal/agent"
	"claimgraph/internal/config"
	"claimgraph/internal/graph"
)

func main() {
	noLLM := flag.Bool("no-llm", false, "skip the LLM and print the deterministic explanation")
	decision := flag.String("decision", "", "human investigator decision to record: confirmed_fraud | not_substantiated | needs_info")
	notes := flag.String("notes", "", "notes for the decision")
	asJSON := flag.Bool("json", false, "print the full JSON result")

	args := os.Args[1:]
	claim := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		claim, args = args[0], args[1:]
	}
	_ = flag.CommandLine.Parse(args)
	if claim == "" {
		fmt.Fprintln(os.Stderr, "usage: agent <claim_id> [-no-llm] [-decision d] [-notes n] [-json]")
		os.Exit(2)
	}

	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	var provider agent.Provider
	if !*noLLM {
		if _, _, err := config.LoadSecrets(); err != nil {
			die(err)
		}
		key := config.GeminiAPIKey()
		if key == "" {
			die(fmt.Errorf("no Gemini API key found: set API_Gemini in Secrets/.env, or use -no-llm"))
		}
		p, err := agent.NewGemini(context.Background(), "", key)
		if err != nil {
			die(err)
		}
		provider = p
	}
	name := os.Getenv("GRAPH_NAME") // e.g. claimgraph_scale; default: the curated demo graph
	if name == "" {
		name = graph.DefaultGraphName
	}
	a := agent.New(db.SelectGraph(name), provider, log.New(os.Stderr, "agent ", log.LstdFlags))

	req := agent.Request{ClaimID: claim}
	if *decision != "" {
		req.Decision = &agent.Decision{Disposition: *decision, Notes: *notes}
	}
	res, err := a.Run(context.Background(), req)
	if err != nil {
		die(err)
	}
	if *asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return
	}

	r := res.Report
	fmt.Printf("%s  score %d  [%s]   (explanation source: %s", r.TargetClaim, r.Score, r.Band, res.Source)
	if res.FallbackReason != "" {
		fmt.Printf(" - %s", res.FallbackReason)
	}
	fmt.Println(")")
	fmt.Println("\n" + res.Explanation.Summary)
	for _, f := range res.Explanation.KeyFindings {
		fmt.Printf("  - %s\n", f.Statement)
	}
	fmt.Printf("\nNEXT VERIFICATION STEP: %s\n  why: %s\n", res.Explanation.NextStep.Action, res.Explanation.NextStep.Rationale)
	if len(res.Trace) > 0 {
		fmt.Println("\ntool calls:")
		for _, t := range res.Trace {
			status := "ok"
			if !t.OK {
				status = "REJECTED: " + t.Error
			}
			fmt.Printf("  %d. %s %s -> %s\n", t.Seq, t.Tool, t.Input, status)
		}
	}
	if res.Recorded != nil {
		fmt.Printf("\nrecorded: %s = %s\n", res.Recorded.CaseID, res.Recorded.Disposition)
	}
	fmt.Printf("\nturns=%d tool_calls=%d tokens in/out=%d/%d\n", res.Usage.Turns, res.Usage.ToolCalls, res.Usage.InputTokens, res.Usage.OutputTokens)
	fmt.Println("\n" + r.Disclaimer)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
