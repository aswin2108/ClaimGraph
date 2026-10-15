package api

import (
	"fmt"
	"testing"
)

func TestRingLinkAndRingListEndpoints(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_algo")
	e.submit(fnol("RA", "2026-09-01", map[string]any{"payout_account_id": "PAY-R"}))
	e.submit(fnol("RB", "2026-09-02", map[string]any{"payout_account_id": "PAY-R", "device_id": "DEV-R"}))
	e.submit(fnol("RC", "2026-09-03", map[string]any{"device_id": "DEV-R"}))
	e.submit(fnol("LONE", "2026-09-04", nil))

	code, ring, _ := e.do("GET", "/api/claims/RA/ring", nil)
	if code != 200 || ring["ring_id"] != "RING-RA" || ring["size"] != float64(3) {
		t.Fatalf("ring: %d %v", code, ring)
	}
	if bb, _ := ring["backbone"].([]any); len(bb) == 0 {
		t.Fatalf("ring backbone missing: %v", ring)
	}
	if code, lone, _ := e.do("GET", "/api/claims/LONE/ring", nil); code != 200 || lone["size"] != float64(1) || lone["message"] == "" {
		t.Fatalf("lone claim: %d %v", code, lone)
	}

	code, link, _ := e.do("GET", "/api/claims/RA/link/RC", nil)
	if code != 200 || link["found"] != true || link["total_evidence_cost"] != float64(18) {
		t.Fatalf("link: %d %v", code, link)
	}

	code, list, _ := e.do("GET", "/api/rings?min_size=2&limit=5", nil)
	rings, _ := list["rings"].([]any)
	if code != 200 || len(rings) != 1 {
		t.Fatalf("rings: %d %v", code, list)
	}

	for path, want := range map[string]int{
		"/api/claims/NOPE-1/ring":      404,
		"/api/claims/RA/link/NOPE-1":   404,
		"/api/claims/RA/link/RA":       400,
		"/api/claims/RA/link/bad%20id": 400,
		"/api/rings?min_size=1":        400,
		"/api/rings?limit=500":         400,
	} {
		if code, b, _ := e.do("GET", path, nil); code != want {
			t.Errorf("%s: got %d %v, want %d", path, code, b, want)
		}
	}
}

func TestIntakeRingAlertActionsOutcomeAndStats(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_intake")
	for i := 0; i < 4; i++ {
		e.submit(fnol(fmt.Sprintf("Q%d", i), fmt.Sprintf("2026-08-%02d", 1+i*5), map[string]any{
			"payout_account_id": "PAY-MULE", "address_id": fmt.Sprintf("ADDR-%d", i), "policy_start": "2026-07-20"}))
	}
	// The new FNOL looks clean on its own; the response says it joins a ring.
	code, b, _ := e.do("POST", "/api/fnol", fnol("Q9", "2026-08-25", map[string]any{
		"payout_account_id": "PAY-MULE", "address_id": "ADDR-9", "policy_start": "2026-07-20"}))
	alert, _ := b["ring_alert"].(map[string]any)
	if code != 201 || b["policy_check_status"] != "passed" || alert == nil || alert["level"] != "review_recommended" || alert["size"] != float64(5) {
		t.Fatalf("intake alert: %d %v", code, b)
	}
	if code, b, _ := e.do("POST", "/api/fnol", fnol("SOLO", "2026-08-25", nil)); code != 201 || b["ring_alert"] != nil {
		t.Fatalf("an unconnected claim has no ring alert: %v", b)
	}

	code, inv, _ := e.do("GET", "/api/claims/Q9/investigation", nil)
	plan, _ := inv["recommended_actions"].(map[string]any)
	acts, _ := plan["actions"].([]any)
	invest, _ := inv["investigation"].(map[string]any)
	if code != 200 || len(acts) == 0 || inv["score"] == nil || invest["outcome"] != "connected" || inv["ring_alert"] == nil {
		t.Fatalf("investigation response: %d %v", code, inv)
	}
	if _, solo, _ := e.do("GET", "/api/claims/SOLO/investigation", nil); solo["investigation"].(map[string]any)["outcome"] != "no_supporting_connection" {
		t.Fatalf("unconnected outcome: %v", solo["investigation"])
	}

	code, st, _ := e.do("GET", "/api/graph/stats", nil)
	labels, _ := st["labels"].(map[string]any)
	if code != 200 || labels["Claim"] != float64(6) || st["analytics_overlay_ready"] != true || st["relationships"].(float64) == 0 {
		t.Fatalf("graph stats: %d %v", code, st)
	}
}
