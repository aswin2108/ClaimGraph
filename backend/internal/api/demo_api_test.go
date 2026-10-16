package api

import (
	"context"
	"io"
	"log"
	"testing"

	"claimgraph/internal/demo"
	"claimgraph/internal/graph"
)

func TestDemoResetEndpoint(t *testing.T) {
	const name = "claimgraph_test_api_reset"
	db, err := graph.Connect()
	if err != nil {
		t.Skipf("FalkorDB unavailable: %v", err)
	}
	cleanup := func() {
		for _, n := range []string{name, demo.SnapshotName(name), name + "__restore"} {
			_ = db.Conn.Del(context.Background(), n).Err()
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	srv := New(db.SelectGraph(name), origin, log.New(io.Discard, "", 0))
	srv.EnableDemoReset(db)
	e := &env{t: t, h: srv.Handler()}

	e.submit(fnol("BASE", "2026-09-01", nil))
	if _, meta, _ := e.do("GET", "/api/meta", nil); meta["demo_reset_available"] != false || meta["graph"] != name {
		t.Fatalf("no snapshot yet: %v", meta)
	}
	if code, b, _ := e.do("POST", "/api/demo/reset", map[string]any{"confirm": "reset"}); code != 409 {
		t.Fatalf("reset without a snapshot: %d %v", code, b)
	}
	if err := demo.Snapshot(db, name); err != nil {
		t.Fatal(err)
	}
	if _, meta, _ := e.do("GET", "/api/meta", nil); meta["demo_reset_available"] != true {
		t.Fatalf("snapshot saved: %v", meta)
	}

	e.submit(fnol("VISITOR", "2026-09-02", nil))
	for _, body := range []any{map[string]any{}, map[string]any{"confirm": "yes"}, `{"confirm":"reset","extra":1}`} {
		if code, b, _ := e.do("POST", "/api/demo/reset", body); code != 400 {
			t.Fatalf("reset %v must be refused: %d %v", body, code, b)
		}
	}
	if code, _, _ := e.do("GET", "/api/claims/VISITOR", nil); code != 200 {
		t.Fatal("a refused reset changed the graph")
	}

	code, b, _ := e.do("POST", "/api/demo/reset", map[string]any{"confirm": "reset"})
	if code != 200 || b["claims"] != float64(1) || b["restored_from"] != demo.SnapshotName(name) {
		t.Fatalf("reset: %d %v", code, b)
	}
	if code, _, _ := e.do("GET", "/api/claims/VISITOR", nil); code != 404 {
		t.Fatalf("the visitor's claim must be gone after the reset: %d", code)
	}
	if code, _, _ := e.do("GET", "/api/claims/BASE", nil); code != 200 {
		t.Fatalf("the seeded claim must survive the reset: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/demo/reset", map[string]any{"confirm": "reset"}); code != 429 {
		t.Fatalf("an immediate second reset must be throttled: %d", code)
	}
}

func TestDemoResetDisabledByDefault(t *testing.T) {
	e := newEnv(t, "claimgraph_test_api_reset_off")
	if code, b, _ := e.do("POST", "/api/demo/reset", map[string]any{"confirm": "reset"}); code != 404 {
		t.Fatalf("reset must be off unless enabled: %d %v", code, b)
	}
	if _, meta, _ := e.do("GET", "/api/meta", nil); meta["demo_reset_available"] != false {
		t.Fatalf("meta: %v", meta)
	}
}
