package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimgraph/internal/graph"
)

const origin = "http://localhost:5173"

type env struct {
	t *testing.T
	h http.Handler
}

func newEnv(t *testing.T, name string) *env {
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	g := db.SelectGraph(name)
	t.Cleanup(func() { _ = g.Delete() })
	return &env{t: t, h: New(g, origin, log.New(io.Discard, "", 0)).Handler()}
}

func (e *env) do(method, path string, body any, hdr ...string) (int, map[string]any, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "localhost:8080" // httptest defaults to example.com, which the host guard rejects
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" { // Go ignores a Host header; the request's Host field is what the server sees
			req.Host = hdr[i+1]
			continue
		}
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

func fnol(id, filed string, extra map[string]any) map[string]any {
	m := map[string]any{
		"claim_id": id, "claimant_id": "DRV-" + id, "claimant_name": "Driver " + id,
		"policy_id": "POL-" + id, "policy_start": "2026-01-01", "policy_end": "2026-12-31", "policy_status": "active",
		"vin": ("VIN" + id + "00000000000000")[:17], "incident_date": filed, "filing_date": filed,
		"amount": 1000, "payout_account_id": "PAY-" + id, "payout_type": "claimant", "repair_shop_id": "SHOP-" + id,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (e *env) submit(f map[string]any) {
	e.t.Helper()
	if code, body, _ := e.do("POST", "/api/fnol", f); code != 201 && code != 200 {
		e.t.Fatalf("submit %v: %d %v", f["claim_id"], code, body)
	}
}

func TestHealthAndMeta(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_meta")
	if code, b, _ := e.do("GET", "/healthz", nil); code != 200 || b["status"] != "ok" {
		t.Fatalf("health: %d %v", code, b)
	}
	code, b, _ := e.do("GET", "/api/meta", nil)
	cats, _ := b["counterfactual_categories"].([]any)
	if code != 200 || len(cats) != 6 || b["disclaimer"] == "" {
		t.Fatalf("meta: %d %v", code, b)
	}
}

func TestSubmitFNOL(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_fnol")
	f := fnol("A1", "2026-09-10", nil)

	code, b, _ := e.do("POST", "/api/fnol", f)
	if code != 201 || b["created"] != true || b["policy_check_status"] != "passed" {
		t.Fatalf("create: %d %v", code, b)
	}
	if code, b, _ = e.do("POST", "/api/fnol", f); code != 200 || b["created"] != false {
		t.Fatalf("resubmit should be 200/created=false: %d %v", code, b)
	}

	bad := fnol("A2", "2026-09-10", map[string]any{"amount": -5, "vin": "short"})
	code, b, _ = e.do("POST", "/api/fnol", bad)
	problems, _ := b["problems"].([]any)
	if code != 400 || len(problems) != 2 {
		t.Fatalf("invalid FNOL should list both problems: %d %v", code, b)
	}
	if code, _, _ := e.do("GET", "/api/claims/A2", nil); code != 404 {
		t.Fatal("invalid FNOL must not create a claim")
	}
}

func TestRequestHardening(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_harden")
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		hdr    []string
		want   int
	}{
		{"unknown field", "POST", "/api/fnol", `{"claim_id":"X1","evil":1}`, nil, 400},
		{"malformed json", "POST", "/api/fnol", `{not json`, nil, 400},
		{"trailing data", "POST", "/api/fnol", `{} {}`, nil, 400},
		{"wrong content type", "POST", "/api/fnol", `{}`, []string{"Content-Type", "text/plain"}, 415},
		{"oversized body", "POST", "/api/fnol", `{"claim_id":"` + strings.Repeat("a", 2<<20) + `"}`, nil, 413},
		{"wrong method", "DELETE", "/api/claims/X1", nil, nil, 405},
		{"unknown route", "GET", "/api/nope", nil, nil, 404},
		{"injection id", "GET", "/api/claims/x%27%7D)%20DETACH%20DELETE%20(n", nil, nil, 400},
		{"bad hops", "GET", "/api/claims/X1/investigation?hops=9", nil, nil, 400},
		{"bad limit", "GET", "/api/claims?limit=0", nil, nil, 400},
		{"bad exclude on graph", "GET", "/api/claims/X1/graph?exclude=nope", nil, nil, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := e.do(tc.method, tc.path, tc.body, tc.hdr...)
			if code != tc.want {
				t.Fatalf("got %d (%v), want %d", code, body, tc.want)
			}
		})
	}
}

func TestClaimQueueAndDetail(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_queue")
	if code, b, _ := e.do("GET", "/api/claims", nil); code != 200 || len(b["claims"].([]any)) != 0 {
		t.Fatalf("empty queue: %d %v", code, b)
	}
	e.submit(fnol("Q1", "2026-09-01", nil))
	e.submit(fnol("Q2", "2026-09-05", map[string]any{"device_id": "DEV-Q", "ip_address": "203.0.113.5", "tow_provider_id": "TOW-Q"}))

	_, b, _ := e.do("GET", "/api/claims", nil)
	list := b["claims"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["claim_id"] != "Q2" {
		t.Fatalf("queue should list newest first: %v", list)
	}
	if code, b, _ := e.do("GET", "/api/claims?limit=1", nil); code != 200 || len(b["claims"].([]any)) != 1 {
		t.Fatalf("limit: %d %v", code, b)
	}

	code, b, _ := e.do("GET", "/api/claims/Q2", nil)
	dev, _ := b["device"].(map[string]any)
	ip, _ := b["ip_address"].(map[string]any)
	tow, _ := b["tow_provider"].(map[string]any)
	if code != 200 || b["policy_check_status"] != "passed" || dev["id"] != "DEV-Q" || ip["public"] != true || tow["id"] != "TOW-Q" {
		t.Fatalf("detail: %d %v", code, b)
	}
	if code, _, _ := e.do("GET", "/api/claims/NOPE", nil); code != 404 {
		t.Fatalf("unknown claim should be 404, got %d", code)
	}
}

// The complete 7-step user journey, driven purely through HTTP.
func TestFullInvestigationJourney(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_journey")
	shared := map[string]any{"payout_account_id": "PAY-S", "device_id": "DEV-S", "repair_shop_id": "SHOP-S"}
	e.submit(fnol("OLD1", "2026-07-01", shared))
	e.submit(fnol("OLD2", "2026-08-01", shared))
	// 1-2. Receive FNOL + connect evidence: ordinary checks pass.
	code, b, _ := e.do("POST", "/api/fnol", fnol("NEW", "2026-09-10", shared))
	if code != 201 || b["policy_check_status"] != "passed" {
		t.Fatalf("new FNOL: %d %v", code, b)
	}

	// 3-4. Investigate + explain: payout 25 + device 20 + shop 8 + breadth 10 + reinforce 10 = 73.
	code, b, _ = e.do("GET", "/api/claims/NEW/investigation", nil)
	if code != 200 || b["score"].(float64) != 73 || b["band"] != "High Priority" || b["connected_claims"].(float64) != 2 {
		t.Fatalf("investigation: %d score=%v band=%v", code, b["score"], b["band"])
	}
	if len(b["uncertainty"].([]any)) == 0 || b["disclaimer"] == "" {
		t.Fatal("report must carry uncertainty notes and the disclaimer")
	}

	// 5. Challenge the finding: ignore the repair shop => exactly -8, still High Priority.
	code, b, _ = e.do("POST", "/api/claims/NEW/counterfactual", map[string]any{"exclude": "repair_shop"})
	if code != 200 || b["delta"].(float64) != -8 || b["still_justifies_investigation"] != true {
		t.Fatalf("counterfactual: %d %v", code, b)
	}
	if code, _, _ := e.do("POST", "/api/claims/NEW/counterfactual", map[string]any{"exclude": "tow_provider"}); code != 400 {
		t.Fatalf("non-scoring category must be rejected, got %d", code)
	}
	// The counterfactual did not change the stored result.
	_, b, _ = e.do("GET", "/api/claims/NEW/investigation", nil)
	if b["score"].(float64) != 73 {
		t.Fatal("counterfactual mutated the original analysis")
	}

	// 7. Record outcome on an older claim; the next investigation retrieves it (+15).
	code, b, _ = e.do("POST", "/api/claims/OLD1/disposition", map[string]any{"disposition": "confirmed_fraud", "notes": "synthetic"})
	if code != 201 || b["case_id"] != "CASE-OLD1" {
		t.Fatalf("disposition: %d %v", code, b)
	}
	_, b, _ = e.do("GET", "/api/claims/NEW/investigation", nil)
	cases := b["prior_cases"].([]any)
	if b["score"].(float64) != 88 || len(cases) != 1 {
		t.Fatalf("memory loop: score=%v prior_cases=%v", b["score"], cases)
	}
	// Changing the outcome updates the same case and removes the bonus.
	code, b, _ = e.do("POST", "/api/claims/OLD1/disposition", map[string]any{"disposition": "not_substantiated"})
	if code != 200 || b["previous_disposition"] != "confirmed_fraud" {
		t.Fatalf("update: %d %v", code, b)
	}
	_, b, _ = e.do("GET", "/api/claims/NEW/investigation", nil)
	if b["score"].(float64) != 73 || len(b["prior_cases"].([]any)) != 1 {
		t.Fatalf("not_substantiated must be retrieved but earn nothing: %v", b["score"])
	}
	// Queue shows the recorded disposition.
	_, b, _ = e.do("GET", "/api/claims", nil)
	found := false
	for _, c := range b["claims"].([]any) {
		if m := c.(map[string]any); m["claim_id"] == "OLD1" && m["disposition"] == "not_substantiated" {
			found = true
		}
	}
	if !found {
		t.Fatal("queue should show the recorded disposition")
	}
}

func TestDispositionValidation(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_disp")
	e.submit(fnol("D1", "2026-09-01", nil))
	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"bad disposition": {"/api/claims/D1/disposition", map[string]any{"disposition": "guilty"}, 400},
		"missing":         {"/api/claims/D1/disposition", map[string]any{}, 400},
		"long notes":      {"/api/claims/D1/disposition", map[string]any{"disposition": "needs_info", "notes": strings.Repeat("n", 501)}, 400},
		"unknown claim":   {"/api/claims/NOPE/disposition", map[string]any{"disposition": "needs_info"}, 404},
	} {
		t.Run(name, func(t *testing.T) {
			if code, b, _ := e.do("POST", tc.path, tc.body); code != tc.want {
				t.Fatalf("got %d %v want %d", code, b, tc.want)
			}
		})
	}
	if _, b, _ := e.do("GET", "/api/claims/D1", nil); b["disposition"] != nil {
		t.Fatal("rejected input must not create a case")
	}
}

func TestInvestigationOfUnknownAndIsolatedClaim(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_isolated")
	e.submit(fnol("L1", "2026-09-01", nil))
	if code, _, _ := e.do("GET", "/api/claims/NOPE/investigation", nil); code != 404 {
		t.Fatal("unknown claim should be 404")
	}
	code, b, _ := e.do("GET", "/api/claims/L1/investigation", nil)
	if code != 200 || b["score"].(float64) != 0 || b["connected_claims"].(float64) != 0 {
		t.Fatalf("isolated claim: %d %v", code, b)
	}
	inv := b["investigation"].(map[string]any)
	if inv["message"] == "" || len(inv["related_claims"].([]any)) != 0 {
		t.Fatal("must state explicitly that no connection was found and invent no claims")
	}
}

func TestGraphView(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_graph")
	e.submit(fnol("G1", "2026-09-10", map[string]any{"payout_account_id": "PAY-S", "repair_shop_id": "SHOP-OWN", "contact_type": "phone", "contact_value": "+1-555-0100", "tow_provider_id": "TOW-G"}))
	e.submit(fnol("G2", "2026-09-11", map[string]any{"payout_account_id": "PAY-S", "contact_type": "phone", "contact_value": "+1-555-0100", "repair_shop_id": "SHOP-G1", "tow_provider_id": "TOW-G"}))
	e.submit(fnol("G3", "2026-09-12", map[string]any{"repair_shop_id": "SHOP-G1"}))

	code, b, _ := e.do("GET", "/api/claims/G1/graph", nil)
	if code != 200 {
		t.Fatalf("graph: %d %v", code, b)
	}
	nodes := map[string]map[string]any{}
	for _, n := range b["nodes"].([]any) {
		m := n.(map[string]any)
		nodes[m["id"].(string)] = m
	}
	for _, id := range []string{"claim:G1", "claim:G2", "claim:G3", "entity:PayoutDestination:PAY-S",
		"entity:ContactPoint:+1-555-0100", "claimant:DRV-G1", "claimant:DRV-G2", "entity:RepairShop:SHOP-G1", "entity:TowProvider:TOW-G"} {
		if nodes[id] == nil {
			t.Errorf("missing node %s", id)
		}
	}
	if nodes["claim:G1"]["target"] != true || nodes["claim:G3"]["hops"].(float64) != 2 {
		t.Errorf("target/hops wrong: %v %v", nodes["claim:G1"], nodes["claim:G3"])
	}
	if nodes["entity:TowProvider:TOW-G"]["context"] != true {
		t.Error("tow provider must be flagged context-only")
	}
	nodeIDs := nodes
	for _, ed := range b["edges"].([]any) {
		m := ed.(map[string]any)
		if nodeIDs[m["source"].(string)] == nil || nodeIDs[m["target"].(string)] == nil {
			t.Errorf("edge references a missing node: %v", m)
		}
	}

	// Counterfactual view drops the excluded evidence and anything only it connected.
	_, b, _ = e.do("GET", "/api/claims/G1/graph?exclude=repair_shop", nil)
	for _, n := range b["nodes"].([]any) {
		if id := n.(map[string]any)["id"]; id == "claim:G3" || id == "entity:RepairShop:SHOP-G1" {
			t.Errorf("excluded evidence leaked into the graph: %v", id)
		}
	}
	if b["excluded_category"] != "repair_shop" {
		t.Error("excluded category should be echoed")
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_cors")
	_, _, h := e.do("GET", "/healthz", nil, "Origin", origin)
	if h.Get("Access-Control-Allow-Origin") != origin {
		t.Error("configured origin should be allowed")
	}
	_, _, h = e.do("GET", "/healthz", nil, "Origin", "http://evil.example")
	if h.Get("Access-Control-Allow-Origin") != "" {
		t.Error("other origins must receive no CORS headers")
	}
	code, _, h := e.do("OPTIONS", "/api/fnol", nil, "Origin", origin, "Access-Control-Request-Method", "POST")
	if code != 204 || !strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("preflight: %d %v", code, h)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing")
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_leak")
	e.submit(fnol("Z1", "2026-09-01", nil))
	_, b, _ := e.do("GET", "/api/claims/Z1/investigation?hops=abc", nil)
	if s, _ := b["error"].(string); strings.Contains(strings.ToLower(s), "cypher") || strings.Contains(s, "falkor") {
		t.Fatalf("error leaks internals: %q", s)
	}
}

func TestClaimQueuePagingAndSearch(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_queue_page")
	e.submit(fnol("CLM-Q1", "2026-09-01", map[string]any{"vin": "VINQ1000000000000"}))
	e.submit(fnol("CLM-Q2", "2026-09-02", map[string]any{"vin": "VINQ2000000000000"}))
	e.submit(fnol("CLM-X3", "2026-09-03", map[string]any{"vin": "VINX3000000000000"}))

	ids := func(b map[string]any) []string {
		var out []string
		for _, c := range b["claims"].([]any) {
			out = append(out, c.(map[string]any)["claim_id"].(string))
		}
		return out
	}
	if code, b, _ := e.do("GET", "/api/claims?limit=2", nil); code != 200 || fmt.Sprint(ids(b)) != "[CLM-X3 CLM-Q2]" || b["total"] != float64(3) {
		t.Fatalf("first page: %d %v", code, b)
	}
	if code, b, _ := e.do("GET", "/api/claims?limit=2&offset=2", nil); code != 200 || fmt.Sprint(ids(b)) != "[CLM-Q1]" || b["total"] != float64(3) {
		t.Fatalf("second page: %d %v", code, b)
	}
	// search: case-insensitive, matches anywhere in the id; total counts the matches
	if code, b, _ := e.do("GET", "/api/claims?q=q", nil); code != 200 || fmt.Sprint(ids(b)) != "[CLM-Q2 CLM-Q1]" || b["total"] != float64(2) {
		t.Fatalf("search: %d %v", code, b)
	}
	if code, b, _ := e.do("GET", "/api/claims?q=NOPE", nil); code != 200 || len(b["claims"].([]any)) != 0 || b["total"] != float64(0) {
		t.Fatalf("no match: %d %v", code, b)
	}
	for _, bad := range []string{"offset=-1", "offset=x", "q=a%20b", "q=a'%20OR%201=1", "limit=500"} {
		if code, _, _ := e.do("GET", "/api/claims?"+bad, nil); code != 400 {
			t.Errorf("%s: got %d, want 400", bad, code)
		}
	}
}

func TestClaimDetailShowsTheRecordedDecisionWithNotesAndDate(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_decision")
	e.submit(fnol("D1", "2026-09-01", nil))
	if _, b, _ := e.do("GET", "/api/claims/D1", nil); b["recorded_decision"] != nil {
		t.Fatalf("no decision yet: %v", b["recorded_decision"])
	}
	if code, b, _ := e.do("POST", "/api/claims/D1/disposition", map[string]any{"disposition": "needs_info", "notes": "asked the bank for the account holder"}); code != 201 {
		t.Fatalf("record: %d %v", code, b)
	}
	_, b, _ := e.do("GET", "/api/claims/D1", nil)
	d, _ := b["recorded_decision"].(map[string]any)
	if d == nil || d["disposition"] != "needs_info" || d["notes"] != "asked the bank for the account holder" || d["recorded_at"] == "" || b["disposition"] != "needs_info" {
		t.Fatalf("decision on the claim detail: %v", b)
	}
	// Re-recording replaces it: the detail shows the latest decision.
	e.do("POST", "/api/claims/D1/disposition", map[string]any{"disposition": "not_substantiated", "notes": "holder confirmed"})
	_, b, _ = e.do("GET", "/api/claims/D1", nil)
	if d, _ := b["recorded_decision"].(map[string]any); d["disposition"] != "not_substantiated" || d["notes"] != "holder confirmed" {
		t.Fatalf("updated decision: %v", b["recorded_decision"])
	}
}
