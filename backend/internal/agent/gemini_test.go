package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testKey = "AIzaSy-TEST-KEY-do-not-leak"

// fakeGemini is a minimal generateContent endpoint that records what it receives.
type fakeGemini struct {
	mu        sync.Mutex
	responses []func(w http.ResponseWriter) // one per request
	repeat    func(w http.ResponseWriter)   // used for every request after the scripted ones
	bodies    []map[string]any
	headers   []http.Header
	urls      []string
}

func (f *fakeGemini) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.urls = append(f.urls, r.URL.String())
		i := len(f.bodies) - 1
		if i >= len(f.responses) {
			if f.repeat != nil {
				f.repeat(w)
				return
			}
			http.Error(w, `{"error":{"code":500,"message":"script exhausted"}}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		f.responses[i](w)
	})
}

func jsonResp(s string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = io.WriteString(w, s) }
}

func statusResp(code int, s string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code); _, _ = io.WriteString(w, s) }
}

// newTestGemini uses the PRODUCTION client configuration (same attempts and timeout),
// pointed at the fake server, with only the retry delay shortened.
func newTestGemini(t *testing.T, f *fakeGemini) *Gemini {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cfg := geminiConfig(testKey)
	cfg.HTTPOptions.BaseURL = srv.URL
	delay := 0.01
	cfg.HTTPOptions.RetryOptions.InitialDelay = &delay
	g, err := newGemini(context.Background(), "test-model", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const callResp = `{"candidates":[{"content":{"role":"model","parts":[
  {"functionCall":{"name":"GetClaim","args":{"claim_id":"NEW"}},"thoughtSignature":"c2lnLTEyMw=="}]},"finishReason":"STOP"}],
  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3}}`

const textResp = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello "},{"text":"world"}]},"finishReason":"STOP"}]}`

func TestGeminiRequestShapeAndFunctionCallRoundTrip(t *testing.T) {
	f := &fakeGemini{responses: []func(http.ResponseWriter){jsonResp(callResp), jsonResp(textResp), jsonResp(textResp)}}
	g := newTestGemini(t, f)
	if g.Name() != "test-model" {
		t.Fatalf("model name %q", g.Name())
	}
	conv := g.NewConversation("SYSTEM PROMPT", Specs())

	turn, err := conv.Send(context.Background(), nil, "brief")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Calls) != 1 || turn.Calls[0].Name != "GetClaim" || turn.Calls[0].ID != "call_1" ||
		!strings.Contains(string(turn.Calls[0].Input), `"claim_id":"NEW"`) {
		t.Fatalf("function call not parsed: %+v", turn.Calls)
	}
	if turn.InTokens != 10 || turn.OutTokens != 8 {
		t.Fatalf("usage should include thinking tokens: in=%d out=%d", turn.InTokens, turn.OutTokens)
	}

	// --- first request: tools, system prompt, AUTO mode, key only in a header ---
	req := f.bodies[0]
	decls := req["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 6 {
		t.Fatalf("want 6 declared tools, got %d", len(decls))
	}
	d0 := decls[0].(map[string]any)
	schema := d0["parametersJsonSchema"].(map[string]any)
	if schema["additionalProperties"] != false || schema["type"] != "object" {
		t.Fatalf("tool schema must forbid extra properties: %v", schema)
	}
	if !strings.Contains(toJSON(req["systemInstruction"]), "SYSTEM PROMPT") {
		t.Fatal("system instruction missing")
	}
	if toJSON(req["toolConfig"]) == "" || !strings.Contains(toJSON(req["toolConfig"]), "AUTO") {
		t.Fatalf("function calling mode should be AUTO: %v", req["toolConfig"])
	}
	if f.headers[0].Get("x-goog-api-key") != testKey {
		t.Fatal("API key must be sent in the x-goog-api-key header")
	}
	if strings.Contains(f.urls[0], testKey) || strings.Contains(f.urls[0], "key=") {
		t.Fatalf("API key must never appear in the URL: %s", f.urls[0])
	}

	// --- second request: tool result goes back as a functionResponse; the model's own
	//     turn (with its thought signature) is replayed unchanged ---
	_, err = conv.Send(context.Background(), []ToolResult{{CallID: "call_1", Content: `{"claim_id":"NEW","amount":4200}`}}, "")
	if err != nil {
		t.Fatal(err)
	}
	contents := f.bodies[1]["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("history should be user, model, user(functionResponse): %d", len(contents))
	}
	model := contents[1].(map[string]any)
	if model["role"] != "model" || !strings.Contains(toJSON(model), "c2lnLTEyMw==") {
		t.Fatalf("model turn / thought signature not replayed: %s", toJSON(model))
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	out := fr["response"].(map[string]any)["output"].(map[string]any)
	if fr["name"] != "GetClaim" || out["amount"].(float64) != 4200 {
		t.Fatalf("functionResponse wrong: %v", fr)
	}

	// --- error results use the "error" key ---
	if _, err = conv.Send(context.Background(), nil, "again"); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiToolErrorResultUsesErrorKey(t *testing.T) {
	f := &fakeGemini{responses: []func(http.ResponseWriter){jsonResp(callResp), jsonResp(textResp)}}
	conv := newTestGemini(t, f).NewConversation("s", Specs())
	if _, err := conv.Send(context.Background(), nil, "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), []ToolResult{{CallID: "call_1", Content: "claim not found", IsError: true}}, ""); err != nil {
		t.Fatal(err)
	}
	fr := f.bodies[1]["contents"].([]any)[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["response"].(map[string]any)["error"] != "claim not found" {
		t.Fatalf("error result should use the error key: %v", fr)
	}
}

func TestGeminiTextAndThoughtParts(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"private reasoning","thought":true},{"text":"final "},{"text":"answer"}]},"finishReason":"STOP"}]}`
	f := &fakeGemini{responses: []func(http.ResponseWriter){jsonResp(body)}}
	turn, err := newTestGemini(t, f).NewConversation("s", Specs()).Send(context.Background(), nil, "go")
	if err != nil || turn.Text != "final answer" || len(turn.Calls) != 0 {
		t.Fatalf("thought summaries must be excluded: %+v err=%v", turn, err)
	}
}

func TestGeminiFinishReasons(t *testing.T) {
	mk := func(reason string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"` + reason + `"}]}`
	}
	cases := map[string]struct {
		body           string
		refused, trunc bool
		wantErr        bool
	}{
		"max tokens":     {mk("MAX_TOKENS"), false, true, false},
		"safety":         {mk("SAFETY"), true, false, false},
		"prohibited":     {mk("PROHIBITED_CONTENT"), true, false, false},
		"blocked prompt": {`{"promptFeedback":{"blockReason":"SAFETY"}}`, true, false, false},
		"malformed call": {mk("MALFORMED_FUNCTION_CALL"), false, false, true},
		"no candidates":  {`{}`, false, false, true},
		"normal stop":    {mk("STOP"), false, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeGemini{responses: []func(http.ResponseWriter){jsonResp(tc.body)}}
			turn, err := newTestGemini(t, f).NewConversation("s", Specs()).Send(context.Background(), nil, "go")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil || turn.Refused != tc.refused || turn.Truncated != tc.trunc {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
		})
	}
}

func TestGeminiRetriesTransientErrorsButNotClientErrors(t *testing.T) {
	f := &fakeGemini{responses: []func(http.ResponseWriter){
		statusResp(429, `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`),
		jsonResp(textResp),
	}}
	turn, err := newTestGemini(t, f).NewConversation("s", Specs()).Send(context.Background(), nil, "go")
	if err != nil || turn.Text != "hello world" || len(f.bodies) != 2 {
		t.Fatalf("429 should be retried once: requests=%d turn=%+v err=%v", len(f.bodies), turn, err)
	}

	bad := &fakeGemini{responses: []func(http.ResponseWriter){
		statusResp(400, `{"error":{"code":400,"message":"API key not valid: `+testKey+`","status":"INVALID_ARGUMENT"}}`),
		jsonResp(textResp),
	}}
	_, err = newTestGemini(t, bad).NewConversation("s", Specs()).Send(context.Background(), nil, "go")
	if err == nil || len(bad.bodies) != 1 {
		t.Fatalf("400 must fail without retry: requests=%d err=%v", len(bad.bodies), err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("API key leaked into the error text: %v", err)
	}
}

func TestGeminiRequiresKey(t *testing.T) {
	if _, err := NewGemini(context.Background(), "", "  "); err == nil {
		t.Fatal("an empty key must be rejected")
	}
	g, err := NewGemini(context.Background(), "", "some-key")
	if err != nil || g.Name() != DefaultModel {
		t.Fatalf("default model should be %s, got %v err=%v", DefaultModel, g, err)
	}
	t.Setenv("AGENT_MODEL", "gemini-custom")
	if g, _ := NewGemini(context.Background(), "", "some-key"); g.Name() != "gemini-custom" {
		t.Fatal("AGENT_MODEL should override the default")
	}
	if g, _ := NewGemini(context.Background(), "explicit", "some-key"); g.Name() != "explicit" {
		t.Fatal("an explicit model should win")
	}
}

func TestScrub(t *testing.T) {
	if got := scrub("bad key "+testKey+" and again "+testKey, testKey); strings.Contains(got, testKey) {
		t.Fatalf("not scrubbed: %s", got)
	}
	if scrub("unchanged", "") != "unchanged" {
		t.Fatal("empty key must be a no-op")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
