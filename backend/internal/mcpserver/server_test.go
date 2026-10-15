package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"claimgraph/internal/agent"
	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fnol(id, filed string, mod func(f *ingest.FNOL)) *ingest.FNOL {
	f := &ingest.FNOL{
		ClaimID: id, ClaimantID: "DRV-" + id, ClaimantName: "Driver " + id,
		PolicyID: "POL-" + id, PolicyStart: "2026-01-01", PolicyEnd: "2026-12-31", PolicyStatus: "active",
		VIN: ("VIN" + id + "00000000000000")[:17], IncidentDate: filed, FilingDate: filed, Amount: 1000,
		PayoutAccountID: "PAY-" + id, PayoutType: "claimant", RepairShopID: "SHOP-" + id,
	}
	if mod != nil {
		mod(f)
	}
	return f
}

type env struct {
	t  *testing.T
	g  *falkordb.Graph
	cs *mcp.ClientSession
}

// connect starts the server and a client over in-memory transports, against a scratch
// graph where NEW shares payout and device with OLD1 and OLD2, and FAR joins the ring.
func connect(t *testing.T) *env {
	t.Helper()
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph("claimgraph_test_mcp")
	_ = g.Delete()
	t.Cleanup(func() { _ = g.Delete() })
	shared := func(f *ingest.FNOL) { f.PayoutAccountID, f.DeviceID = "PAY-S", "DEV-S" }
	for _, f := range []*ingest.FNOL{
		fnol("OLD1", "2026-07-01", shared), fnol("OLD2", "2026-08-01", shared), fnol("NEW", "2026-09-10", shared),
		fnol("FAR", "2026-06-01", func(f *ingest.FNOL) { f.DeviceID = "DEV-S" }),
		fnol("LONE", "2026-09-10", nil),
	} {
		if _, err := ingest.Ingest(g, f); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	srv := New(g, nil)
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &env{t: t, g: g, cs: cs}
}

// call returns the text content and whether the tool reported an error.
func (e *env) call(name string, args map[string]any) (string, bool) {
	e.t.Helper()
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func (e *env) count() string {
	e.t.Helper()
	r, err := e.g.ROQuery(`MATCH (n) OPTIONAL MATCH (n)-[r]->() RETURN count(DISTINCT n) AS n, count(r) AS r`, nil, nil)
	if err != nil || !r.Next() {
		e.t.Fatal(err)
	}
	n, _ := r.Record().Get("n")
	rr, _ := r.Record().Get("r")
	return fmt.Sprint(n, "/", rr)
}

func TestToolListIsTheReadOnlyAllowlist(t *testing.T) {
	e := connect(t)
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", tool.Name)
		}
		schema := fmt.Sprint(tool.InputSchema)
		if strings.Contains(strings.ToLower(schema), "cypher") || strings.Contains(strings.ToLower(schema), "query") {
			t.Errorf("%s must not accept query text: %s", tool.Name, schema)
		}
	}
	sort.Strings(names)
	want := []string{ToolBeginInvestigation, agent.ToolFindRelatedClaims, agent.ToolGetClaim, agent.ToolGetClaimRing,
		agent.ToolGetEvidencePaths, agent.ToolGetPriorCases, agent.ToolGetStrongestLink, ToolListClaimRings, agent.ToolRunCounterfactual}
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools %v, want %v", names, want)
	}
}

func TestInvestigationOverMCP(t *testing.T) {
	e := connect(t)
	if out, isErr := e.call(agent.ToolGetClaim, map[string]any{"claim_id": "NEW"}); !isErr || !strings.Contains(out, "BeginInvestigation") {
		t.Fatalf("tools must require BeginInvestigation first: %q", out)
	}
	if out, isErr := e.call(ToolBeginInvestigation, map[string]any{"claim_id": "NOPE-1"}); !isErr || !strings.Contains(out, "not found") {
		t.Fatalf("unknown claim: %q", out)
	}
	open, isErr := e.call(ToolBeginInvestigation, map[string]any{"claim_id": "NEW"})
	// payout 25 + device 20 + three connected claims 15 + corroboration 10 = 70 (no claim within 7 days)
	if isErr || !strings.Contains(open, `"investigation_priority_score":70`) || !strings.Contains(open, `"band":"High Priority"`) {
		t.Fatalf("opening: %s", open)
	}
	if out, isErr := e.call(agent.ToolFindRelatedClaims, map[string]any{"claim_id": "NEW", "max_hops": 1}); isErr || !strings.Contains(out, "OLD1") {
		t.Fatalf("related: %s", out)
	}
	if out, isErr := e.call(agent.ToolGetEvidencePaths, map[string]any{"claim_id": "NEW", "related_claim_id": "OLD1"}); isErr || !strings.Contains(out, "PAY-S") {
		t.Fatalf("evidence: %s", out)
	}
	ring, isErr := e.call(agent.ToolGetClaimRing, map[string]any{"claim_id": "NEW"})
	if isErr || !strings.Contains(ring, `"FAR"`) {
		t.Fatalf("ring: %s", ring)
	}
	if out, isErr := e.call(agent.ToolGetStrongestLink, map[string]any{"claim_id": "NEW", "other_claim_id": "FAR"}); isErr || !strings.Contains(out, `"found":true`) {
		t.Fatalf("link: %s", out)
	}
	if out, isErr := e.call(agent.ToolRunCounterfactual, map[string]any{"claim_id": "NEW", "exclude_signal_type": "payout"}); isErr || !strings.Contains(out, "counterfactual_score") {
		t.Fatalf("counterfactual: %s", out)
	}
	rings, isErr := e.call(ToolListClaimRings, map[string]any{"min_size": 2})
	if isErr || !strings.Contains(rings, "RING-FAR") {
		t.Fatalf("rings: %s", rings)
	}
}

func TestMCPRejectsQueriesWritesAndOutOfScopeCalls(t *testing.T) {
	e := connect(t)
	before := e.count()
	if _, isErr := e.call(ToolBeginInvestigation, map[string]any{"claim_id": "NEW"}); isErr {
		t.Fatal("begin failed")
	}
	// Smuggled query text is an unknown field: rejected, nothing runs.
	out, isErr := e.call(agent.ToolGetClaim, map[string]any{"claim_id": "NEW", "cypher": "MATCH (n) DETACH DELETE n"})
	if !isErr || !strings.Contains(out, "invalid tool input") {
		t.Fatalf("query smuggling must be rejected: %q", out)
	}
	// The disposition tool does not exist on this surface.
	if res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: agent.ToolRecordDisposition,
		Arguments: map[string]any{"claim_id": "NEW", "disposition": "confirmed_fraud"}}); err == nil && !res.IsError {
		t.Fatal("RecordInvestigatorDisposition must not be callable over MCP")
	}
	// LONE was never revealed: out of scope (second violation).
	if _, isErr := e.call(agent.ToolGetClaim, map[string]any{"claim_id": "LONE"}); !isErr {
		t.Fatal("unrevealed claim must be rejected")
	}
	// Third violation closes the investigation.
	if out, _ := e.call(agent.ToolGetClaimRing, map[string]any{"claim_id": "OLD1"}); !strings.Contains(out, "closed") {
		t.Fatalf("third violation should close the investigation: %q", out)
	}
	if out, isErr := e.call(agent.ToolGetClaim, map[string]any{"claim_id": "NEW"}); !isErr || !strings.Contains(out, "BeginInvestigation") {
		t.Fatalf("a closed investigation must refuse further calls: %q", out)
	}
	// Starting over works.
	if _, isErr := e.call(ToolBeginInvestigation, map[string]any{"claim_id": "NEW"}); isErr {
		t.Fatal("begin after close failed")
	}
	if _, isErr := e.call(agent.ToolGetClaim, map[string]any{"claim_id": "NEW"}); isErr {
		t.Fatal("a new investigation must accept in-scope calls")
	}
	if after := e.count(); after != before {
		t.Fatalf("MCP calls changed the graph: %s -> %s", before, after)
	}
}
