package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The built-in agent selects and calls its tools over the Model Context Protocol. Each
// run starts an in-process MCP server bound to that run's Toolbox and connects an MCP
// client to it. The tools offered to the model are exactly the ones the server lists in
// tools/list, and every call the model makes is sent as tools/call. External clients
// reach the same tools through cmd/mcp (internal/mcpserver).
//
// MCP is the transport; the Toolbox stays the authority. The server-side handler runs
// Toolbox.Execute, so the allowlist, strict decoding, scope rules, limits and the
// violation count are unchanged. The run's server also offers RecordInvestigatorDisposition,
// which stays gated by the human decision in the request. The external server never offers it.

// MCP identity of the tool server.
const (
	MCPServerName = "claimgraph"
	MCPVersion    = "0.2.0"
	mcpClientName = "claimgraph-agent"
)

// ToolChannel reports how the model reached its tools in a run.
type ToolChannel struct {
	Protocol   string   `json:"protocol"`   // "mcp"
	Server     string   `json:"server"`     // MCP server name
	Version    string   `json:"version"`    // MCP server version
	Transport  string   `json:"transport"`  // "in-process"
	Discovered []string `json:"discovered"` // tool names from tools/list, in the server's order
	Calls      int      `json:"calls"`      // tool calls the server received as tools/call
}

// InputSchema is the JSON Schema advertised for spec over MCP. Unknown fields are not
// allowed; the Toolbox rejects them anyway.
func InputSchema(spec ToolSpec) map[string]any {
	required := spec.Required
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": spec.Properties, "required": required, "additionalProperties": false}
}

// mcpTools is one run's MCP client session to its tool server.
type mcpTools struct {
	cs      *mcp.ClientSession
	tb      *Toolbox
	specs   []ToolSpec
	listed  map[string]bool
	channel *ToolChannel

	mu     sync.Mutex
	served int   // tools/call requests handled by the server
	abort  error // set by the server-side handler when the Toolbox ends the run
}

// openMCPTools serves specs from tb over an in-memory MCP connection and lists them
// through the client, so the model is offered what the protocol advertised.
func openMCPTools(ctx context.Context, tb *Toolbox, specs []ToolSpec) (*mcpTools, error) {
	m := &mcpTools{tb: tb, listed: map[string]bool{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: MCPServerName, Title: "ClaimGraph investigation tools", Version: MCPVersion}, nil)
	for _, s := range specs {
		name := s.Name
		srv.AddTool(&mcp.Tool{Name: name, Description: s.Description, InputSchema: InputSchema(s)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				content, isErr, abort := tb.Execute(ToolCall{Name: name, Input: req.Params.Arguments})
				m.mu.Lock()
				m.served++
				if abort != nil {
					m.abort = abort
				}
				m.mu.Unlock()
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: content}}, IsError: isErr}, nil
			})
	}
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return nil, fmt.Errorf("start MCP tool server: %w", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: mcpClientName, Version: MCPVersion}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	m.cs = cs
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		_ = cs.Close()
		return nil, fmt.Errorf("MCP tools/list: %w", err)
	}
	m.channel = &ToolChannel{Protocol: "mcp", Server: MCPServerName, Version: MCPVersion, Transport: "in-process", Discovered: []string{}}
	if info := cs.InitializeResult(); info != nil && info.ServerInfo != nil {
		m.channel.Server, m.channel.Version = info.ServerInfo.Name, info.ServerInfo.Version
	}
	for _, t := range list.Tools {
		spec, err := specFromMCP(t)
		if err != nil {
			_ = cs.Close()
			return nil, err
		}
		m.specs = append(m.specs, spec)
		m.listed[t.Name] = true
		m.channel.Discovered = append(m.channel.Discovered, t.Name)
	}
	return m, nil
}

// specFromMCP turns a listed MCP tool back into the provider-neutral ToolSpec.
func specFromMCP(t *mcp.Tool) (ToolSpec, error) {
	b, err := json.Marshal(t.InputSchema)
	if err != nil {
		return ToolSpec{}, fmt.Errorf("MCP tool %s: schema: %w", t.Name, err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		return ToolSpec{}, fmt.Errorf("MCP tool %s: schema: %w", t.Name, err)
	}
	return ToolSpec{Name: t.Name, Description: t.Description, Properties: schema.Properties, Required: schema.Required}, nil
}

// Specs are the tools the server listed, in its order (the SDK lists them by name).
func (m *mcpTools) Specs() []ToolSpec { return m.specs }

// Call sends one model tool call as an MCP tools/call and returns the Toolbox's answer
// (content, isErr, abort as from Toolbox.Execute). A name the server never listed is not
// sent: the Toolbox records it as the allowlist violation it is. err is a transport
// failure, never a tool rejection.
func (m *mcpTools) Call(ctx context.Context, c ToolCall) (content string, isErr bool, abort, err error) {
	if !m.listed[c.Name] {
		content, isErr, abort = m.tb.Execute(c)
		return content, isErr, abort, nil
	}
	var args json.RawMessage
	if len(c.Input) > 0 {
		args = json.RawMessage(c.Input)
	}
	r, err := m.cs.CallTool(ctx, &mcp.CallToolParams{Name: c.Name, Arguments: args})
	if err != nil {
		return "", true, nil, fmt.Errorf("MCP tools/call %s: %w", c.Name, err)
	}
	var text string
	for _, ct := range r.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	m.mu.Lock()
	abort = m.abort
	m.mu.Unlock()
	return text, r.IsError, abort, nil
}

// Close ends the MCP session (the in-process server stops with it) and records how many
// calls the server handled.
func (m *mcpTools) Close() {
	_ = m.cs.Close()
	m.mu.Lock()
	m.channel.Calls = m.served
	m.mu.Unlock()
}
