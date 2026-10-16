package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/scoring"
)

// runReport runs the (deterministic) agent on id and returns its run id and score.
func (e *env) runReport(id string) (string, float64) {
	e.t.Helper()
	code, b, _ := e.do("POST", "/api/claims/"+id+"/agent", map[string]any{})
	if code != 200 {
		e.t.Fatalf("agent %s: %d %v", id, code, b)
	}
	run, _ := b["run_id"].(string)
	if len(run) != 32 {
		e.t.Fatalf("agent report has no run id: %v", b["run_id"])
	}
	rep, _ := b["report"].(map[string]any)
	return run, rep["score"].(float64)
}

func TestClosingSealsTheReportAndLocksTheInvestigation(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_close")
	shared := map[string]any{"payout_account_id": "PAY-SHARED", "device_id": "DEV-SHARED"}
	e.submit(fnol("K1", "2026-09-01", shared))
	e.submit(fnol("K2", "2026-09-03", shared))
	run, score := e.runReport("K1")
	other, _ := e.runReport("K2")

	for _, c := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no run id", map[string]any{"outcome": "rejected", "reason": "x"}, 400},
		{"bad outcome", map[string]any{"run_id": run, "outcome": "deny", "reason": "x"}, 400},
		{"no reason", map[string]any{"run_id": run, "outcome": "rejected", "reason": "  "}, 400},
		{"another claim's report", map[string]any{"run_id": other, "outcome": "rejected", "reason": "x"}, 409},
		{"unknown report", map[string]any{"run_id": "00000000000000000000000000000000", "outcome": "rejected", "reason": "x"}, 409},
		{"unknown field", map[string]any{"run_id": run, "outcome": "rejected", "reason": "x", "report": "{}"}, 400},
	} {
		if code, b, _ := e.do("POST", "/api/claims/K1/close", c.body); code != c.want {
			t.Errorf("%s: want %d, got %d %v", c.name, c.want, code, b)
		}
	}
	if _, b, _ := e.do("GET", "/api/claims/K1", nil); b["closure"] != nil {
		t.Fatalf("rejected requests must not close: %v", b["closure"])
	}

	code, c, _ := e.do("POST", "/api/claims/K1/close", map[string]any{"run_id": run, "outcome": "rejected", "reason": "Payout holder is not the claimant"})
	if code != 201 || c["outcome"] != "rejected" || c["disposition"] != "confirmed_fraud" || c["report_score"] != score || c["closed_at"] == "" {
		t.Fatalf("close: %d %v", code, c)
	}

	// The claim shows the closure, the queue shows the outcome.
	_, d, _ := e.do("GET", "/api/claims/K1", nil)
	cl, _ := d["closure"].(map[string]any)
	if cl == nil || cl["outcome"] != "rejected" || cl["reason"] != "Payout holder is not the claimant" || d["disposition"] != "confirmed_fraud" {
		t.Fatalf("claim detail: %v", d)
	}
	_, q, _ := e.do("GET", "/api/claims?q=K1", nil)
	if rows, _ := q["claims"].([]any); len(rows) != 1 || rows[0].(map[string]any)["investigation_outcome"] != "rejected" {
		t.Fatalf("queue outcome: %v", q["claims"])
	}

	// The sealed report is the one the investigator read.
	_, sealed, _ := e.do("GET", "/api/claims/K1/sealed-report", nil)
	if sealed["run_id"] != run || sealed["report"].(map[string]any)["score"] != score {
		t.Fatalf("sealed report: run %v", sealed["run_id"])
	}
	if code, _, _ := e.do("GET", "/api/claims/K2/sealed-report", nil); code != 404 {
		t.Fatalf("open claim has no sealed report: %d", code)
	}

	// Locked: no new runs, decisions or closures; reading still works.
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/api/claims/K1/agent", map[string]any{}},
		{"/api/claims/K1/agent", map[string]any{"investigator_decision": map[string]any{"disposition": "needs_info"}}},
		{"/api/claims/K1/disposition", map[string]any{"disposition": "not_substantiated"}},
		{"/api/claims/K1/close", map[string]any{"run_id": run, "outcome": "approved", "reason": "changed my mind"}},
	} {
		if code, b, _ := e.do("POST", c.path, c.body); code != 409 {
			t.Errorf("POST %s on a closed investigation: want 409, got %d %v", c.path, code, b)
		}
	}
	if code, _, _ := e.do("GET", "/api/claims/K1/investigation", nil); code != 200 {
		t.Fatalf("a closed investigation stays viewable: %d", code)
	}
	if _, d, _ := e.do("GET", "/api/claims/K1", nil); d["closure"].(map[string]any)["outcome"] != "rejected" {
		t.Fatal("closure changed after a refused request")
	}

	// Memory: a later claim linked to the rejected one gets the confirmed-case points.
	e.submit(fnol("K3", "2026-09-20", shared))
	_, inv, _ := e.do("GET", "/api/claims/K3/investigation", nil)
	found := false
	for _, f := range inv["factors"].([]any) {
		if f.(map[string]any)["name"] == scoring.FConfirmedFraud {
			found = true
		}
	}
	if !found {
		t.Fatalf("a rejected closure must feed later investigations: %v", inv["factors"])
	}
}

func TestApprovedClosureEarnsNoConfirmedCasePoints(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_close_approved")
	shared := map[string]any{"payout_account_id": "PAY-HH"}
	e.submit(fnol("A1", "2026-09-01", shared))
	run, _ := e.runReport("A1")
	code, c, _ := e.do("POST", "/api/claims/A1/close", map[string]any{"run_id": run, "outcome": "approved", "reason": "Joint account of a married couple, verified"})
	if code != 201 || c["disposition"] != "not_substantiated" {
		t.Fatalf("approve: %d %v", code, c)
	}
	e.submit(fnol("A2", "2026-09-10", shared))
	_, inv, _ := e.do("GET", "/api/claims/A2/investigation", nil)
	for _, f := range inv["factors"].([]any) {
		if f.(map[string]any)["name"] == scoring.FConfirmedFraud {
			t.Fatal("an approved claim must not raise later scores")
		}
	}
}

func TestKeptReportsExpireAreCappedAndCleared(t *testing.T) {
	rs := newReportStore()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rs.now = func() time.Time { return now }
	var first string
	for i := 0; i < maxKeptReports+5; i++ {
		res := &agent.Result{Report: &scoring.Report{Score: i}}
		if err := rs.put(fmt.Sprintf("CLM-%d", i), res); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = res.RunID
		}
		now = now.Add(time.Second)
	}
	if len(rs.runs) != maxKeptReports {
		t.Fatalf("cap: %d kept", len(rs.runs))
	}
	if _, ok := rs.get(first, "CLM-0"); ok {
		t.Fatal("the oldest report must be dropped first")
	}
	var last string
	for id, k := range rs.runs {
		if k.claimID == fmt.Sprintf("CLM-%d", maxKeptReports+4) {
			last = id
		}
	}
	if s, ok := rs.get(last, fmt.Sprintf("CLM-%d", maxKeptReports+4)); !ok || s.Score != maxKeptReports+4 || !json.Valid(s.JSON) {
		t.Fatalf("latest report: %v %+v", ok, s)
	}
	if _, ok := rs.get(last, "CLM-OTHER"); ok {
		t.Fatal("a report must only seal its own claim")
	}
	now = now.Add(reportTTL + time.Minute)
	if _, ok := rs.get(last, fmt.Sprintf("CLM-%d", maxKeptReports+4)); ok || len(rs.runs) != 0 {
		t.Fatal("reports must expire")
	}
	_ = rs.put("CLM-X", &agent.Result{})
	rs.clear()
	if len(rs.runs) != 0 {
		t.Fatal("clear")
	}
}
