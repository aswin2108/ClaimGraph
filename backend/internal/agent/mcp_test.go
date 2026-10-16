package agent

import (
	"reflect"
	"sort"
	"testing"
)

// The model is offered exactly what the MCP server listed (names, descriptions, schemas),
// and every allowlisted call reaches the Toolbox through MCP tools/call.
func TestModelSelectsToolsThroughMCP(t *testing.T) {
	e := newEnv(t, "claimgraph_test_agent_mcp", steps(investigationCalls, say(goodAnswer))...)
	r := e.run(nil)

	if r.Source != SourceLLM {
		t.Fatalf("want llm source, got %s/%s", r.Source, r.FallbackReason)
	}
	ch := r.Tools
	if ch == nil || ch.Protocol != "mcp" || ch.Server != MCPServerName || ch.Version != MCPVersion {
		t.Fatalf("tool channel: %+v", ch)
	}
	byName := map[string]ToolSpec{}
	var want []string
	for _, s := range Specs() {
		byName[s.Name] = s
		want = append(want, s.Name)
	}
	listed := append([]string{}, ch.Discovered...)
	sort.Strings(listed)
	sort.Strings(want)
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("tools/list returned %v, want all %d allowlisted tools %v", ch.Discovered, len(want), want)
	}
	if len(e.fp.specs) != len(want) {
		t.Fatalf("model was offered %d tools, want %d", len(e.fp.specs), len(want))
	}
	for i, got := range e.fp.specs {
		orig, ok := byName[got.Name]
		if !ok || got.Name != ch.Discovered[i] || got.Description != orig.Description {
			t.Errorf("tool %d offered as %q, listed as %q", i, got.Name, ch.Discovered[i])
			continue
		}
		gotReq, wantReq := append([]string{}, got.Required...), append([]string{}, orig.Required...)
		sort.Strings(gotReq)
		sort.Strings(wantReq)
		if len(got.Properties) != len(orig.Properties) || !reflect.DeepEqual(gotReq, wantReq) {
			t.Errorf("tool %s schema changed over MCP: %v %v", got.Name, got.Properties, got.Required)
		}
	}
	if ch.Calls != len(r.Trace) || ch.Calls != 5 {
		t.Fatalf("server handled %d tools/call requests, trace has %d entries", ch.Calls, len(r.Trace))
	}
}

// A tool the server never listed is not sent over MCP; the Toolbox still counts it as an
// allowlist violation, and three of them end the run (same guard as before MCP).
func TestUnlistedToolIsNeverSentOverMCP(t *testing.T) {
	bad := call("x", "RunCypher", `{"query":"MATCH (n) DETACH DELETE n"}`)
	e := newEnv(t, "claimgraph_test_agent_mcp_unlisted", calls(bad, bad, bad), say(goodAnswer))
	before := e.fingerprint()
	r := e.run(nil)

	if r.Source != SourceFallback || r.FallbackReason != ReasonViolations {
		t.Fatalf("want violation fallback, got %s/%s", r.Source, r.FallbackReason)
	}
	if r.Tools == nil || r.Tools.Calls != 0 {
		t.Fatalf("unlisted tools must not reach the MCP server: %+v", r.Tools)
	}
	if len(r.Trace) != 3 {
		t.Fatalf("violations must still be traced: %d entries", len(r.Trace))
	}
	if e.fingerprint() != before {
		t.Fatal("graph changed")
	}
}

// The gated disposition tool works over MCP with a human decision, and the deterministic
// path (no model) opens no tool channel at all.
func TestDecisionOverMCPAndNoChannelWithoutModel(t *testing.T) {
	e := newEnv(t, "claimgraph_test_agent_mcp_decision",
		calls(call("d1", ToolRecordDisposition, `{"claim_id":"NEW","disposition":"needs_info","notes":""}`)),
		say(goodAnswer))
	r := e.run(&Decision{Disposition: "needs_info", Notes: "call back"})
	if r.Recorded == nil || r.Recorded.Disposition != "needs_info" || r.Tools == nil || r.Tools.Calls != 1 {
		t.Fatalf("decision over MCP: recorded=%+v tools=%+v", r.Recorded, r.Tools)
	}

	e2 := newEnv(t, "claimgraph_test_agent_mcp_off")
	e2.a.Provider = nil
	if r2 := e2.run(nil); r2.Tools != nil || r2.Source != SourceFallback {
		t.Fatalf("no model, no tool channel: %+v %s", r2.Tools, r2.Source)
	}
}
