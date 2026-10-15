package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"
)

// DefaultModel is used unless AGENT_MODEL overrides it (a stable Gemini Flash model).
const DefaultModel = "gemini-3.8-flash"

const maxOutputTokens = 16384

// Cost controls on the HTTP layer. Worst case per run: MaxTurns x requestAttempts HTTP
// requests (8 x 2 = 16), each limited to requestTimeout, all inside RunTimeout.
const (
	requestAttempts = 2 // 1 original + 1 retry, only for 408/429/5xx
	requestTimeout  = 45 * time.Second
)

// Gemini is a Provider backed by the Gemini API (Google's official Go SDK).
// The API key is sent by the SDK in a request header, never in the URL, and is
// scrubbed from any error text before it can reach logs.
type Gemini struct {
	client *genai.Client
	model  string
	key    string
}

// NewGemini builds a provider. model "" selects AGENT_MODEL or DefaultModel.
func NewGemini(ctx context.Context, model, apiKey string) (*Gemini, error) {
	return newGemini(ctx, model, geminiConfig(apiKey))
}

// geminiConfig is the production client configuration (tests derive theirs from it).
func geminiConfig(apiKey string) *genai.ClientConfig {
	attempts, delay := int32(requestAttempts), 1.0
	timeout := requestTimeout
	return &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			Timeout: &timeout,
			// Transient failures (429, 5xx) get one short retry; the run's own deadline still applies.
			RetryOptions: &genai.HTTPRetryOptions{Attempts: &attempts, InitialDelay: &delay},
		},
	}
}

func newGemini(ctx context.Context, model string, cc *genai.ClientConfig) (*Gemini, error) {
	if strings.TrimSpace(cc.APIKey) == "" {
		return nil, errors.New("no Gemini API key configured")
	}
	if model == "" {
		model = os.Getenv("AGENT_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("create Gemini client: %s", scrub(err.Error(), cc.APIKey))
	}
	return &Gemini{client: client, model: model, key: cc.APIKey}, nil
}

func (g *Gemini) Name() string { return g.model }

func (g *Gemini) NewConversation(system string, specs []ToolSpec) Conversation {
	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	for _, s := range specs {
		schema := map[string]any{"type": "object", "properties": s.Properties, "additionalProperties": false}
		if len(s.Required) > 0 {
			schema["required"] = s.Required
		}
		decls = append(decls, &genai.FunctionDeclaration{Name: s.Name, Description: s.Description, ParametersJsonSchema: schema})
	}
	return &geminiConv{
		g: g,
		config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
			Tools:             []*genai.Tool{{FunctionDeclarations: decls}},
			// AUTO: forced tool use is not needed; the Go-side allowlist is what bounds the agent.
			ToolConfig:      &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}},
			MaxOutputTokens: maxOutputTokens,
		},
		names: map[string]string{}, apiIDs: map[string]bool{},
	}
}

type geminiConv struct {
	g      *Gemini
	config *genai.GenerateContentConfig
	// contents is the history in the SDK's native form. Model turns are appended exactly
	// as returned, which preserves thought signatures across function-calling turns.
	contents []*genai.Content
	names    map[string]string // call id -> function name
	apiIDs   map[string]bool   // call ids that came from the API (vs. synthesised)
	n        int
}

func (cv *geminiConv) Send(ctx context.Context, results []ToolResult, text string) (*Turn, error) {
	parts := make([]*genai.Part, 0, len(results)+1)
	for _, r := range results {
		resp := map[string]any{}
		if r.IsError {
			resp["error"] = r.Content
		} else {
			var v any
			if json.Unmarshal([]byte(r.Content), &v) == nil {
				resp["output"] = v
			} else {
				resp["output"] = r.Content
			}
		}
		p := genai.NewPartFromFunctionResponse(cv.names[r.CallID], resp)
		if cv.apiIDs[r.CallID] {
			p.FunctionResponse.ID = r.CallID
		}
		parts = append(parts, p)
	}
	if text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	contents := append(append([]*genai.Content{}, cv.contents...), genai.NewContentFromParts(parts, genai.RoleUser))

	resp, err := cv.g.client.Models.GenerateContent(ctx, cv.g.model, contents, cv.config)
	if err != nil {
		return nil, errors.New("model request failed: " + scrub(err.Error(), cv.g.key))
	}

	turn := &Turn{}
	if u := resp.UsageMetadata; u != nil {
		turn.InTokens = int64(u.PromptTokenCount)
		turn.OutTokens = int64(u.CandidatesTokenCount) + int64(u.ThoughtsTokenCount)
	}
	if len(resp.Candidates) == 0 {
		if pf := resp.PromptFeedback; pf != nil && pf.BlockReason != "" {
			turn.Refused = true
			return turn, nil
		}
		return nil, errors.New("model returned no candidates")
	}
	cand := resp.Candidates[0]
	switch cand.FinishReason {
	case genai.FinishReasonMaxTokens:
		turn.Truncated = true
		return turn, nil
	case genai.FinishReasonSafety, genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII, genai.FinishReasonRecitation, genai.FinishReasonImageSafety:
		turn.Refused = true
		return turn, nil
	case genai.FinishReasonMalformedFunctionCall, genai.FinishReasonUnexpectedToolCall,
		genai.FinishReasonTooManyToolCalls, genai.FinishReasonOther:
		return nil, fmt.Errorf("model stopped abnormally: %s", cand.FinishReason)
	}

	if cand.Content != nil {
		cv.contents = append(contents, cand.Content)
		for _, p := range cand.Content.Parts {
			switch {
			case p == nil || p.Thought:
				// thought summaries are never part of the answer
			case p.FunctionCall != nil:
				fc := p.FunctionCall
				id := fc.ID
				if id == "" {
					cv.n++
					id = fmt.Sprintf("call_%d", cv.n)
				} else {
					cv.apiIDs[id] = true
				}
				cv.names[id] = fc.Name
				args := []byte("{}")
				if fc.Args != nil {
					if b, err := json.Marshal(fc.Args); err == nil {
						args = b
					}
				}
				turn.Calls = append(turn.Calls, ToolCall{ID: id, Name: fc.Name, Input: args})
			case p.Text != "":
				turn.Text += p.Text
			}
		}
	}
	return turn, nil
}

// scrub removes the API key from text before it is logged or returned.
func scrub(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}
