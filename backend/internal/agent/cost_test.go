package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests count the HTTP requests that actually reach a fake Gemini server, using the
// production client configuration, to prove the worst case is bounded.

func fnCall(name, args string) string {
	return `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"` + name + `","args":` + args + `},"thoughtSignature":"c2ln"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`
}

func textJSON(t *testing.T, text string) string {
	b, _ := json.Marshal(text)
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + string(b) + `}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`
}

// envWithGemini builds the standard scenario but wires the REAL Gemini provider to a fake server.
func envWithGemini(t *testing.T, name string, f *fakeGemini) *env {
	e := newEnv(t, name)
	e.a.Provider = newTestGemini(t, f)
	return e
}

func TestWorstCaseLoopingModelIsCappedAtMaxTurns(t *testing.T) {
	// The model asks for a tool on every single turn and never answers.
	f := &fakeGemini{repeat: jsonResp(fnCall("GetClaim", `{"claim_id":"NEW"}`))}
	e := envWithGemini(t, "claimgraph_test_cost_loop", f)
	r := e.run(nil)
	if len(f.bodies) != MaxTurns {
		t.Fatalf("a looping model made %d requests; the cap is MaxTurns=%d", len(f.bodies), MaxTurns)
	}
	if r.Source != SourceFallback || r.FallbackReason != ReasonTurnLimit || r.Report.Score != 73 {
		t.Fatalf("got %s/%s", r.Source, r.FallbackReason)
	}
	if okTools := countOK(r.Trace); okTools > MaxToolCalls {
		t.Fatalf("%d tool calls executed, cap is %d", okTools, MaxToolCalls)
	}
}

func TestServerErrorsAreNotAmplified(t *testing.T) {
	for name, status := range map[string]int{"500": 500, "503": 503, "429": 429} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGemini{repeat: statusResp(status, `{"error":{"code":`+name+`,"message":"down","status":"UNAVAILABLE"}}`)}
			e := envWithGemini(t, "claimgraph_test_cost_err"+name, f)
			r := e.run(nil)
			if len(f.bodies) > requestAttempts {
				t.Fatalf("%d requests for one failing call; the cap is %d attempts", len(f.bodies), requestAttempts)
			}
			if r.Source != SourceFallback || r.FallbackReason != ReasonLLMError || r.Usage.Turns != 1 {
				t.Fatalf("a failed call must end the run immediately: %s/%s turns=%d", r.Source, r.FallbackReason, r.Usage.Turns)
			}
		})
	}
}

func TestClientErrorsAreNeverRetried(t *testing.T) {
	f := &fakeGemini{repeat: statusResp(400, `{"error":{"code":400,"message":"bad request","status":"INVALID_ARGUMENT"}}`)}
	e := envWithGemini(t, "claimgraph_test_cost_400", f)
	e.run(nil)
	if len(f.bodies) != 1 {
		t.Fatalf("a 400 must not be retried, got %d requests", len(f.bodies))
	}
}

func TestRunStopsWhenTokenCapIsReached(t *testing.T) {
	big := strings.Replace(fnCall("GetClaim", `{"claim_id":"NEW"}`), `"promptTokenCount":100`, `"promptTokenCount":100000`, 1)
	f := &fakeGemini{repeat: jsonResp(big)}
	e := envWithGemini(t, "claimgraph_test_cost_tokens", f)
	r := e.run(nil)
	// Turn 1 uses ~100k (under the cap), turn 2 pushes the total over it; no turn 3.
	if len(f.bodies) != 2 || r.FallbackReason != ReasonTokenLimit {
		t.Fatalf("requests=%d reason=%s; want 2 requests then %s", len(f.bodies), r.FallbackReason, ReasonTokenLimit)
	}
	if r.Usage.InputTokens+r.Usage.OutputTokens <= MaxRunTokens {
		t.Fatalf("usage accounting wrong: %+v", r.Usage)
	}
}

func TestHungServerEndsAtDeadlineWithBoundedRequests(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	e := newEnv(t, "claimgraph_test_cost_hang")
	cfg := geminiConfig(testKey)
	cfg.HTTPOptions.BaseURL = srv.URL
	g, err := newGemini(context.Background(), "m", cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a.Provider = g

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := e.a.Run(ctx, Request{ClaimID: "NEW"})
	if err != nil || r.Source != SourceFallback || r.FallbackReason != ReasonTimeout {
		t.Fatalf("got %+v err=%v", r, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("a hung server held the request for %v", time.Since(start))
	}
	if hits.Load() > requestAttempts {
		t.Fatalf("%d requests to a hung server; cap is %d", hits.Load(), requestAttempts)
	}
}

func TestBudgetStopsAllModelCallsWhenExhausted(t *testing.T) {
	// Every run gets un-parseable text: one repair retry, then fallback = 2 requests per run.
	f := &fakeGemini{repeat: jsonResp(textJSON(t, "not json"))}
	e := envWithGemini(t, "claimgraph_test_cost_budget", f)
	e.a.Budget = NewBudget(2, 0) // two LLM runs per hour

	var reasons []string
	var after []int
	for i := 0; i < 5; i++ {
		r := e.run(nil)
		reasons = append(reasons, r.FallbackReason)
		after = append(after, len(f.bodies))
		if r.Report.Score != 73 || r.Explanation == nil {
			t.Fatalf("run %d must still return the deterministic result", i)
		}
	}
	if after[1] != 4 || after[4] != 4 {
		t.Fatalf("no model request may happen once the budget is spent; cumulative requests per run: %v", after)
	}
	for _, i := range []int{2, 3, 4} {
		if reasons[i] != ReasonBudget {
			t.Fatalf("run %d reason=%s want %s", i, reasons[i], ReasonBudget)
		}
	}
}

func TestDailyTokenBudgetStopsFurtherRuns(t *testing.T) {
	f := &fakeGemini{repeat: jsonResp(textJSON(t, "not json"))} // 220 tokens per run (2 requests)
	e := envWithGemini(t, "claimgraph_test_cost_daily", f)
	e.a.Budget = NewBudget(100, 400)
	for i := 0; i < 6; i++ {
		e.run(nil)
	}
	st := e.a.Budget.Status()
	if st.TokensToday < 400 || len(f.bodies) != 4 {
		t.Fatalf("daily token cap not enforced: %d requests, status %+v", len(f.bodies), st)
	}
}

func TestBudgetWindowsAndDefaults(t *testing.T) {
	now := time.Date(2026, 10, 15, 22, 30, 0, 0, time.UTC)
	b := NewBudget(2, 1000)
	b.now = func() time.Time { return now }

	if !b.Allow() || !b.Allow() || b.Allow() {
		t.Fatal("exactly two runs per hour must be allowed")
	}
	now = now.Add(61 * time.Minute) // the hourly window slides...
	if !b.Allow() {
		t.Fatal("a run must be allowed again after the hour passes")
	}
	b.Commit(900)
	b.Commit(200) // ...but the daily token cap is separate
	if b.Allow() {
		t.Fatal("daily token budget exhausted: no run allowed")
	}
	now = now.Add(2 * time.Hour) // crosses UTC midnight
	if !b.Allow() || b.Status().TokensToday != 0 {
		t.Fatalf("the token counter must reset at UTC midnight: %+v", b.Status())
	}

	d := NewBudget(0, -5) // nonsense limits can never disable the guard
	if st := d.Status(); st.MaxRunsPerHour != DefaultMaxRunsPerHour || st.DailyTokenLimit != DefaultDailyTokenBudget {
		t.Fatalf("defaults not applied: %+v", st)
	}
	d.Commit(-100)
	if d.Status().TokensToday != 0 {
		t.Fatal("negative commits must be ignored")
	}
}

func TestInvestigatorNotesNeverReachTheModelButAreStored(t *testing.T) {
	const secret = "SECRET-NOTE-DO-NOT-LEAK-123"
	f := &fakeGemini{responses: []func(http.ResponseWriter){
		jsonResp(fnCall(ToolRecordDisposition, `{"claim_id":"NEW","disposition":"needs_info"}`)),
		jsonResp(textJSON(t, goodAnswer)),
	}}
	e := envWithGemini(t, "claimgraph_test_cost_notes", f)
	r := e.run(&Decision{Disposition: "needs_info", Notes: secret})

	if r.Source != SourceLLM || r.Recorded == nil {
		t.Fatalf("got %s/%s recorded=%v", r.Source, r.FallbackReason, r.Recorded)
	}
	for i, body := range f.bodies {
		if strings.Contains(toJSON(body), secret) {
			t.Fatalf("request %d sent the investigator's note to the model", i)
		}
	}
	res, err := e.g.ROQuery(`MATCH (ic:InvestigationCase {case_id:'CASE-NEW'}) RETURN ic.notes`, nil, nil)
	if err != nil || !res.Next() {
		t.Fatalf("case not stored: %v", err)
	}
	if v, _ := res.Record().GetByIndex(0); v != secret {
		t.Fatalf("the system must store the investigator's notes itself, got %v", v)
	}
}

func countOK(tr []TraceEntry) int {
	n := 0
	for _, t := range tr {
		if t.OK {
			n++
		}
	}
	return n
}
