// Package mcpserver exposes ClaimGraph's allowlisted investigation tools over the Model
// Context Protocol, so any MCP client (Claude Desktop, Claude Code, Cursor, ...) can
// investigate a claim against the live FalkorDB graph.
//
// It is a second transport for the SAME tools, not a second agent: every call goes
// through agent.Session, i.e. the built-in agent's Toolbox with its allowlist, strict
// input decoding (unknown fields such as "query" or "cypher" are violations), scope rules
// (target claim + claims revealed in this investigation), per-investigation limits and
// read-only queries. No tool accepts query text. The disposition tool is not exposed.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"claimgraph/internal/agent"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names that exist only on the MCP surface.
const (
	ToolBeginInvestigation = "BeginInvestigation"
	ToolListClaimRings     = "ListClaimRings"
)

// Version is reported to MCP clients.
const Version = "0.1.0"

const instructions = `ClaimGraph helps insurance Special Investigations Unit (SIU) investigators prioritise auto-insurance claims using a FalkorDB graph of synthetic claims and shared entities.
Start with BeginInvestigation(claim_id): it returns the deterministic investigation-priority score, band and contributing factors. Then use the investigation tools on that claim. ListClaimRings gives a book-wide overview of rings (connected components of strong evidence) and needs no investigation.
The score is an investigation priority, not a probability of fraud. Use investigation-oriented language; never state that fraud occurred or recommend denying, approving or paying a claim. A human investigator makes every decision.`

// Server holds the current investigation of one MCP connection.
type Server struct {
	g   *falkordb.Graph
	log *log.Logger

	mu      sync.Mutex
	session *agent.Session
}

// New builds the MCP server with its tools registered.
func New(g *falkordb.Graph, lg *log.Logger) *mcp.Server {
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	s := &Server{g: g, log: lg}
	srv := mcp.NewServer(&mcp.Implementation{Name: "claimgraph", Title: "ClaimGraph investigation tools", Version: Version},
		&mcp.ServerOptions{Instructions: instructions})

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	srv.AddTool(&mcp.Tool{
		Name:        ToolBeginInvestigation,
		Description: "Start (or restart) an investigation of one claim. Returns the deterministic investigation-priority score (0-100), band and the contributing evidence factors computed from the FalkorDB graph. Required before the investigation tools.",
		InputSchema: objectSchema(map[string]any{
			"claim_id": map[string]any{"type": "string", "description": "Claim id, e.g. CLM-1001."},
			"max_hops": map[string]any{"type": "integer", "minimum": 1, "maximum": investigate.MaxHopsLimit},
		}, []string{"claim_id"}),
		Annotations: readOnly,
	}, s.begin)

	for _, spec := range agent.ReadOnlySpecs() {
		spec := spec
		srv.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description + " Requires BeginInvestigation first.",
			InputSchema: agent.InputSchema(spec),
			Annotations: readOnly,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(spec.Name, req.Params.Arguments), nil
		})
	}

	srv.AddTool(&mcp.Tool{
		Name:        ToolListClaimRings,
		Description: "Book-wide overview: run weakly connected components over strong evidence (claimant payout accounts, private devices, phone/email, addresses, same claimant) and list rings with at least min_size claims, in review order (rings with a human-confirmed case first, then larger, then filed over a shorter period). Queue ordering, not a score; membership is not proof of wrongdoing.",
		InputSchema: objectSchema(map[string]any{
			"min_size": map[string]any{"type": "integer", "minimum": 2, "maximum": graphalgo.MaxRingClaims},
			"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": graphalgo.MaxListRings},
		}, nil),
		Annotations: readOnly,
	}, s.listRings)
	return srv
}

type beginIn struct {
	ClaimID string `json:"claim_id"`
	MaxHops int    `json:"max_hops"`
}

func (s *Server) begin(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in beginIn
	if err := decodeStrict(req.Params.Arguments, &in); err != nil {
		return errResult(err.Error()), nil
	}
	if in.MaxHops < 0 || in.MaxHops > investigate.MaxHopsLimit {
		return errResult(fmt.Sprintf("max_hops must be between 1 and %d", investigate.MaxHopsLimit)), nil
	}
	sess, err := agent.NewSession(s.g, s.log, in.ClaimID, in.MaxHops)
	switch {
	case errors.Is(err, investigate.ErrClaimNotFound):
		return errResult("claim not found"), nil
	case errors.Is(err, agent.ErrInvalid):
		return errResult("invalid claim id"), nil
	case err != nil:
		s.log.Printf("BeginInvestigation %s: %v", in.ClaimID, err)
		return errResult("internal error"), nil
	}
	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()
	return textResult(sess.Opening()), nil
}

func (s *Server) call(name string, args json.RawMessage) *mcp.CallToolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return errResult("no investigation is open: call BeginInvestigation with a claim_id first")
	}
	content, isErr := s.session.Call(name, args)
	if isErr {
		return errResult(content)
	}
	return textResult(content)
}

type ringsIn struct {
	MinSize int `json:"min_size"`
	Limit   int `json:"limit"`
}

func (s *Server) listRings(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in ringsIn
	if err := decodeStrict(req.Params.Arguments, &in); err != nil {
		return errResult(err.Error()), nil
	}
	list, err := graphalgo.ListRings(s.g, in.MinSize, in.Limit)
	if err != nil {
		if errors.Is(err, graphalgo.ErrTimeout) || errors.Is(err, graphalgo.ErrOverlayMissing) {
			return errResult(err.Error()), nil
		}
		if in.MinSize != 0 || in.Limit != 0 { // range errors are the caller's to fix
			return errResult(err.Error()), nil
		}
		s.log.Printf("ListClaimRings: %v", err)
		return errResult("internal error"), nil
	}
	b, _ := json.Marshal(list)
	return textResult(string(b)), nil
}

// ---- helpers ----

func objectSchema(props map[string]any, required []string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: true}
}

// decodeStrict mirrors the agent's rule: unknown fields and trailing data are rejected.
func decodeStrict(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid tool input: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid tool input: trailing data")
	}
	return nil
}
