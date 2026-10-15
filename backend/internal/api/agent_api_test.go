package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"claimgraph/internal/agent"
	"claimgraph/internal/graph"

	"github.com/FalkorDB/falkordb-go/v2"
)

func mustGraph(t *testing.T, name string) *falkordb.Graph {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	return g
}

// scriptedProvider replays fixed turns; it stands in for the LLM.
type scriptedProvider struct{ turns []*agent.Turn }

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) NewConversation(string, []agent.ToolSpec) agent.Conversation {
	return &scriptedConv{p: p}
}

type scriptedConv struct {
	p *scriptedProvider
	i int
}

func (c *scriptedConv) Send(context.Context, []agent.ToolResult, string) (*agent.Turn, error) {
	if c.i >= len(c.p.turns) {
		return nil, errors.New("script exhausted")
	}
	t := c.p.turns[c.i]
	c.i++
	return t, nil
}

func newAgentEnv(t *testing.T, name string, p agent.Provider) *env {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	lg := log.New(io.Discard, "", 0)
	s := New(g, origin, lg)
	if p != nil {
		s.SetAgent(agent.New(g, p, lg))
	}
	return &env{t: t, h: s.Handler()}
}

func seedCluster(e *env) {
	shared := map[string]any{"payout_account_id": "PAY-S", "device_id": "DEV-S"}
	e.submit(fnol("OLD1", "2026-07-01", shared))
	e.submit(fnol("OLD2", "2026-08-01", shared))
	e.submit(fnol("NEW", "2026-09-10", shared))
}

const apiAnswer = `{"summary":"Claim NEW is connected to OLD1 and OLD2 through payout PAY-S and device DEV-S, which warrants review.",
"key_findings":[{"statement":"Payout PAY-S is shared with OLD1.","claim_ids":["OLD1"],"entities":["PAY-S"]}],
"uncertainty":["A joint account could explain a shared payout."],
"next_step":{"action":"Verify the account holder of payout PAY-S against the claimants of record.","rationale":"Strongest signal.","claim_ids":["OLD1"],"entities":["PAY-S"]}}`

func TestAgentEndpointWithoutLLMReturnsDeterministicResult(t *testing.T) {
	e := newAgentEnv(t, "claimgraph_test_api_agent_nollm", nil)
	seedCluster(e)
	code, b, _ := e.do("POST", "/api/claims/NEW/agent", map[string]any{})
	if code != 200 || b["source"] != "deterministic_fallback" || b["fallback_reason"] != "llm_disabled" {
		t.Fatalf("%d %v", code, b)
	}
	rep := b["report"].(map[string]any)
	exp := b["explanation"].(map[string]any)
	if rep["score"].(float64) != 65 || exp["next_step"].(map[string]any)["action"] == "" {
		t.Fatalf("report/explanation missing: %v", b)
	}
}

func TestAgentEndpointWithModel(t *testing.T) {
	p := &scriptedProvider{turns: []*agent.Turn{
		{Calls: []agent.ToolCall{{ID: "1", Name: agent.ToolFindRelatedClaims, Input: []byte(`{"claim_id":"NEW"}`)}}},
		{Text: apiAnswer},
	}}
	e := newAgentEnv(t, "claimgraph_test_api_agent_llm", p)
	seedCluster(e)
	code, b, _ := e.do("POST", "/api/claims/NEW/agent", map[string]any{"hops": 2})
	if code != 200 || b["source"] != "llm" || b["model"] != "scripted" {
		t.Fatalf("%d %v", code, b)
	}
	if tr := b["tool_trace"].([]any); len(tr) != 1 || tr[0].(map[string]any)["tool"] != "FindRelatedClaims" {
		t.Fatalf("trace: %v", b["tool_trace"])
	}
}

func TestAgentEndpointRelaysHumanDecision(t *testing.T) {
	e := newAgentEnv(t, "claimgraph_test_api_agent_decision", &scriptedProvider{})
	seedCluster(e)
	// The scripted model fails immediately; the human's decision must still be stored.
	body := map[string]any{"investigator_decision": map[string]any{"disposition": "needs_info", "notes": "synthetic"}}
	code, b, _ := e.do("POST", "/api/claims/NEW/agent", body)
	rec, _ := b["recorded_disposition"].(map[string]any)
	if code != 200 || b["source"] != "deterministic_fallback" || rec["disposition"] != "needs_info" {
		t.Fatalf("%d %v", code, b)
	}
	if _, b, _ = e.do("GET", "/api/claims/NEW", nil); b["disposition"] != "needs_info" {
		t.Fatal("decision not persisted")
	}
}

func TestAgentEndpointValidation(t *testing.T) {
	e := newAgentEnv(t, "claimgraph_test_api_agent_val", nil)
	seedCluster(e)
	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"unknown claim":   {"/api/claims/NOPE/agent", map[string]any{}, http.StatusNotFound},
		"bad id":          {"/api/claims/x%27%7D)/agent", map[string]any{}, http.StatusBadRequest},
		"bad hops":        {"/api/claims/NEW/agent", map[string]any{"hops": 9}, http.StatusBadRequest},
		"bad disposition": {"/api/claims/NEW/agent", map[string]any{"investigator_decision": map[string]any{"disposition": "guilty"}}, http.StatusBadRequest},
		"unknown field":   {"/api/claims/NEW/agent", `{"query":"MATCH (n) DELETE n"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, b, _ := e.do("POST", tc.path, tc.body); code != tc.want {
				t.Fatalf("got %d %v want %d", code, b, tc.want)
			}
		})
	}
}

// A body-less cross-site POST is a "simple" browser request (no CORS preflight). The agent
// endpoint spends LLM budget, so it must insist on a JSON content type.
func TestAgentEndpointRejectsBodylessSimpleRequests(t *testing.T) {
	p := &scriptedProvider{}
	e := newAgentEnv(t, "claimgraph_test_api_agent_csrf", p)
	seedCluster(e)
	for name, hdr := range map[string][]string{
		"no content type":   {"Content-Type", ""},
		"form content type": {"Content-Type", "application/x-www-form-urlencoded"},
		"text/plain":        {"Content-Type", "text/plain"},
	} {
		req := append([]string{"Origin", "http://evil.example"}, hdr...)
		if code, b, _ := e.do("POST", "/api/claims/NEW/agent", "", req...); code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: want 415, got %d %v", name, code, b)
		}
	}
	if len(p.turns) != 0 {
		t.Fatal("no model call may happen for a rejected request")
	}
}

func TestHostGuardBlocksRebindingAndAllowsLocalNames(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_hosts")
	for host, want := range map[string]int{
		"localhost:8080": 200, "127.0.0.1:9999": 200, "[::1]:8080": 200, "LOCALHOST": 200,
		"evil.example": 403, "evil.example:8080": 403, "localhost.evil.example": 403, "10.0.0.5:8080": 403, "": 403,
	} {
		if code, _, _ := e.do("GET", "/healthz", nil, "Host", host); code != want && host != "" {
			t.Errorf("Host %q: got %d want %d", host, code, want)
		}
	}
}

func TestExtraAllowedHostsAreConfigurable(t *testing.T) {
	db := mustGraph(t, "claimgraph_test_api_hosts2")
	s := New(db, origin, nil)
	s.SetAllowedHosts([]string{" Claims.Internal ", ""})
	h := &env{t: t, h: s.Handler()}
	if code, _, _ := h.do("GET", "/healthz", nil, "Host", "claims.internal:8080"); code != 200 {
		t.Fatalf("configured host should be allowed, got %d", code)
	}
}

func TestMetaExposesLLMBudget(t *testing.T) {
	e := newAgentEnv(t, "claimgraph_test_api_budget_meta", &scriptedProvider{})
	_, b, _ := e.do("GET", "/api/meta", nil)
	bud, _ := b["agent_budget"].(map[string]any)
	if b["llm_enabled"] != true || bud["max_llm_runs_per_hour"].(float64) != float64(agent.DefaultMaxRunsPerHour) ||
		bud["daily_token_limit"].(float64) != float64(agent.DefaultDailyTokenBudget) {
		t.Fatalf("budget not exposed: %v", b)
	}
}
