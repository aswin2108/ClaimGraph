package agent

import (
	"encoding/json"
	"fmt"
	"log"

	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Session is one investigation driven by an EXTERNAL model (the MCP server): the same
// Toolbox, allowlist, scope rules, strict input decoding and per-investigation limits as
// the built-in agent, for one target claim, but read-only: the disposition tool is not
// available, because an external client cannot prove that a human made the decision.
// Human decisions are recorded through the investigator UI / API instead.
//
// What a Session cannot do that the built-in agent does: validate the external model's
// final prose. Grounding therefore rests on the tool outputs, which come only from
// FalkorDB queries and the deterministic scoring engine.
type Session struct {
	tb      *Toolbox
	Report  *scoring.Report
	aborted bool
}

// ReadOnlySpecs are the tools offered to external clients: every allowlisted tool except
// RecordInvestigatorDisposition.
func ReadOnlySpecs() []ToolSpec {
	var out []ToolSpec
	for _, s := range Specs() {
		if s.Name != ToolRecordDisposition {
			out = append(out, s)
		}
	}
	return out
}

// NewSession runs the deterministic analysis for claimID and opens a tool session on it.
func NewSession(g *falkordb.Graph, lg *log.Logger, claimID string, hops int) (*Session, error) {
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("%w: invalid claim id", ErrInvalid)
	}
	rep, err := scoring.Analyze(g, claimID, hops)
	if err != nil {
		return nil, err
	}
	if lg == nil {
		lg = log.Default()
	}
	tb := newToolbox(g, nil, lg, claimID, nil)
	s := &Session{tb: tb, Report: rep}
	tb.absorb(s.Opening())
	tb.scores[rep.Score] = true
	return s, nil
}

// Opening is the authoritative starting context for the external model: the
// deterministic score, band and factors, plus the rules the tools enforce.
func (s *Session) Opening() string {
	rep := s.Report
	type factor struct {
		Name     string   `json:"name"`
		Points   int      `json:"points"`
		Reason   string   `json:"reason"`
		Claims   []string `json:"claims"`
		Entities []string `json:"entities"`
	}
	fs := make([]factor, 0, len(rep.Factors))
	for _, f := range rep.Factors {
		fs = append(fs, factor{f.Name, f.Points, f.Reason, nonNil(f.Claims), nonNil(f.Entities)})
	}
	b, _ := json.Marshal(map[string]any{
		"target_claim": rep.TargetClaim, "investigation_priority_score": rep.Score, "band": rep.Band,
		"connected_claims": rep.ConnectedClaims, "factors": fs, "prior_case_count": len(rep.PriorCases),
		"disclaimer": rep.Disclaimer,
		"rules": []string{
			"The score was computed deterministically from FalkorDB evidence; it is an investigation priority, not a probability of fraud. Do not recompute or change it.",
			"Tools only accept the target claim and claims revealed in this investigation. Raw queries are not accepted.",
			fmt.Sprintf("At most %d tool calls and %d counterfactuals per investigation; call BeginInvestigation again to start over.", MaxToolCalls, MaxCounterfactuals),
			"Use investigation-oriented language. Never state that fraud occurred and never recommend denying, approving or paying a claim.",
			"Recording a human disposition is not available here; the investigator records decisions in the ClaimGraph UI.",
		},
	})
	return string(b)
}

// Call executes one allowlisted tool with raw JSON arguments under every Toolbox guard.
// It returns the content for the client and whether it is an error. After MaxViolations
// scope/allowlist violations the session is closed.
func (s *Session) Call(name string, args []byte) (string, bool) {
	if s.aborted {
		return "this investigation was closed after repeated out-of-scope requests; call BeginInvestigation to start again", true
	}
	if name == ToolRecordDisposition {
		return "recording a disposition is not available over this interface; the investigator records decisions in the ClaimGraph UI", true
	}
	content, isErr, abort := s.tb.Execute(ToolCall{Name: name, Input: args})
	if abort != nil {
		s.aborted = true
		return content + " (investigation closed: too many out-of-scope requests)", true
	}
	return content, isErr
}

// Trace is the audit trail of this session's tool calls.
func (s *Session) Trace() []TraceEntry { return append([]TraceEntry(nil), s.tb.trace...) }
