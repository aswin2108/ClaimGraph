// Package agent implements ClaimGraph's single, bounded investigation agent.
//
// Division of labour:
//   - Go computes the investigation score and evidence deterministically (always).
//   - The LLM may only (a) call six allowlisted, validated, budgeted tools to inspect
//     the graph and (b) write a grounded explanation and next verification step.
//   - Every LLM output is validated against what the tools actually returned; anything
//     that is not grounded, or that uses accusatory language, is discarded and replaced
//     by a deterministic fallback explanation.
//
// The agent is stateless: nothing is persisted between runs. Its only memory is the
// investigation history already stored in FalkorDB (see package disposition).
package agent

import "context"

// ToolSpec describes one tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID    string
	Name  string
	Input []byte // raw JSON object
}

// ToolResult is the outcome returned to the model for one ToolCall.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Turn is one model response.
type Turn struct {
	Text      string
	Calls     []ToolCall
	Refused   bool // the model's safety system declined the request
	Truncated bool // the model hit its output limit
	InTokens  int64
	OutTokens int64
}

// Conversation is one model conversation. It owns the message history so providers
// can preserve provider-specific content (e.g. thinking blocks) exactly as returned.
type Conversation interface {
	// Send appends a user message made of tool results followed by optional text, then
	// returns the model's response.
	Send(ctx context.Context, results []ToolResult, text string) (*Turn, error)
}

// Provider creates conversations with a language model.
type Provider interface {
	Name() string
	NewConversation(system string, tools []ToolSpec) Conversation
}
