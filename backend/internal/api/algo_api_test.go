package api

import "testing"

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
