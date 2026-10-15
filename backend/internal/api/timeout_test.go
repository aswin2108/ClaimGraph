package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claimgraph/internal/agent"
)

// slowProvider answers after a delay, like a real model doing several tool-calling turns.
type slowProvider struct{ delay time.Duration }

func (p slowProvider) Name() string { return "slow" }
func (p slowProvider) NewConversation(string, []agent.ToolSpec) agent.Conversation {
	return slowConv{p.delay}
}

type slowConv struct{ d time.Duration }

func (c slowConv) Send(ctx context.Context, _ []agent.ToolResult, _ string) (*agent.Turn, error) {
	select {
	case <-time.After(c.d):
		return &agent.Turn{Text: "not json"}, nil // forces a repair attempt, so a run makes two slow calls
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A real LLM run takes tens of seconds, far longer than the server-wide write timeout. The agent
// endpoint extends its own write deadline; this must keep working through the whole middleware chain
// (the response writers are wrapped, so they have to expose the underlying connection).
// httptest.NewRecorder cannot catch this: only a real server enforces timeouts.
func TestAgentRunMayOutlastTheServerWriteTimeout(t *testing.T) {
	e := newAgentEnv(t, "claimgraph_test_api_writetimeout", slowProvider{delay: 450 * time.Millisecond})
	seedCluster(e)

	srv := httptest.NewUnstartedServer(e.h)
	srv.Config.ReadTimeout = 200 * time.Millisecond  // main.go uses 10s
	srv.Config.WriteTimeout = 300 * time.Millisecond // main.go uses 30s; a run takes 2 x 450ms here
	srv.Start()
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/claims/NEW/agent", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("connection cut after %v: the write deadline was not extended: %v", time.Since(start), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Source string `json:"source"`
		Report struct {
			Score int `json:"score"`
		} `json:"report"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("response truncated: %v", err)
	}
	if out.Source != agent.SourceFallback || out.Report.Score == 0 {
		t.Fatalf("unexpected result: %+v", out)
	}
	if time.Since(start) < 800*time.Millisecond {
		t.Fatalf("the run did not outlast the write timeout (%v); the test proves nothing", time.Since(start))
	}
}
