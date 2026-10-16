package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"claimgraph/internal/actions"
	"claimgraph/internal/disposition"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Run limits.
const (
	MaxTurns          = 8                // model round trips per run
	RunTimeout        = 90 * time.Second // wall-clock cap for the whole run
	MaxConcurrentRuns = 2                // simultaneous runs (cost and load protection)
	// MaxRunTokens caps input+output tokens across one run. It is checked before every
	// further model call, so a run can overshoot it by at most one turn. Typical runs use
	// ~25k; this leaves headroom while bounding the worst case.
	MaxRunTokens = 150_000
)

// ErrBusy is returned when MaxConcurrentRuns agent runs are already in flight.
var ErrBusy = errors.New("agent is busy; try again shortly")

// Source says where the explanation came from.
const (
	SourceLLM      = "llm"
	SourceFallback = "deterministic_fallback"
)

// Fallback reasons (stable codes; details are logged, never returned).
const (
	ReasonDisabled    = "llm_disabled"
	ReasonLLMError    = "llm_error"
	ReasonTimeout     = "llm_timeout"
	ReasonRefused     = "model_refused"
	ReasonTruncated   = "output_truncated"
	ReasonUngrounded  = "invalid_or_ungrounded_output"
	ReasonViolations  = "tool_scope_violations"
	ReasonTurnLimit   = "turn_limit_reached"
	ReasonTokenLimit  = "run_token_limit_reached"
	ReasonBudget      = "llm_budget_exhausted"
	ReasonToolChannel = "tool_channel_error" // the MCP connection to the tools failed
)

// Request is one agent run.
type Request struct {
	ClaimID  string
	Hops     int       // 0 = default
	Decision *Decision // optional human decision to relay and record
}

// Usage reports what a run consumed.
type Usage struct {
	Turns        int   `json:"turns"`
	ToolCalls    int   `json:"tool_calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Result is the outcome of a run. Report is always the deterministic, authoritative
// analysis; Explanation is either validated model output or the deterministic fallback.
type Result struct {
	Report         *scoring.Report         `json:"report"`
	Explanation    *Explanation            `json:"explanation"`
	Source         string                  `json:"source"`
	FallbackReason string                  `json:"fallback_reason,omitempty"`
	Model          string                  `json:"model,omitempty"`
	FollowUps      []CounterfactualSummary `json:"follow_up_counterfactuals"`
	Recorded       *disposition.Case       `json:"recorded_disposition,omitempty"`
	Trace          []TraceEntry            `json:"tool_trace"`
	Usage          Usage                   `json:"usage"`
	// Tools says how the model reached its tools (MCP tools/list + tools/call); nil when
	// the model was not called.
	Tools *ToolChannel `json:"tool_channel,omitempty"`
	// RunID identifies this report so an investigator can close the investigation on exactly
	// what they read (set by the API, which keeps recent reports; the agent itself is stateless).
	RunID string `json:"run_id,omitempty"`
	// RecommendedActions is the deterministic verification plan derived from the graph
	// evidence (internal/actions). The model is shown it and may only refine the next step.
	RecommendedActions *actions.Plan `json:"recommended_actions"`
}

// Agent runs the bounded investigation agent.
type Agent struct {
	Graph    *falkordb.Graph
	Provider Provider // nil disables the LLM: runs return the deterministic fallback
	Logger   *log.Logger
	Budget   *Budget // process-wide spending guard; never nil after New
	sem      chan struct{}
}

// New builds an Agent. provider may be nil.
func New(g *falkordb.Graph, provider Provider, lg *log.Logger) *Agent {
	if lg == nil {
		lg = log.Default()
	}
	return &Agent{Graph: g, Provider: provider, Logger: lg, Budget: NewBudget(0, 0), sem: make(chan struct{}, MaxConcurrentRuns)}
}

// Run investigates one claim. It never returns an LLM failure as an error: the
// deterministic report and fallback explanation are returned instead. It returns an
// error only for invalid requests, unknown claims or database failures.
func (a *Agent) Run(ctx context.Context, req Request) (*Result, error) {
	if !ingest.ValidID(req.ClaimID) {
		return nil, fmt.Errorf("%w: invalid claim id", ErrInvalid)
	}
	if req.Decision != nil {
		if !disposition.ValidDisposition(req.Decision.Disposition) {
			return nil, fmt.Errorf("%w: invalid disposition", ErrInvalid)
		}
		if len([]rune(req.Decision.Notes)) > disposition.MaxNotesLen {
			return nil, fmt.Errorf("%w: notes too long", ErrInvalid)
		}
	}
	// A closed investigation is final: no new runs and no new decisions.
	if closed, err := disposition.IsClosed(a.Graph, req.ClaimID); err != nil {
		return nil, err
	} else if closed {
		return nil, disposition.ErrClosed
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	default:
		return nil, ErrBusy
	}

	// 1. Deterministic analysis first: this is the product's source of truth.
	rep, err := scoring.Analyze(a.Graph, req.ClaimID, req.Hops)
	if err != nil {
		return nil, err
	}
	rec := disposition.New(a.Graph)
	tb := newToolbox(a.Graph, rec, a.Logger, req.ClaimID, req.Decision)
	res := &Result{Report: rep, FollowUps: []CounterfactualSummary{}, Trace: []TraceEntry{}}
	ring, err := graphalgo.RingSummaryOf(a.Graph, req.ClaimID)
	if err != nil {
		a.Logger.Printf("ring summary for %s: %v", req.ClaimID, err) // the plan is built without ring context
		ring = nil
	}
	res.RecommendedActions = actions.Recommend(rep, ring)
	fb := buildFallback(rep)

	// 2. Try the LLM; any failure leaves the deterministic result in place.
	explanation, reason := a.runLLM(ctx, tb, rep, req, res)
	if explanation != nil {
		res.Explanation, res.Source = explanation, SourceLLM
		if a.Provider != nil {
			res.Model = a.Provider.Name()
		}
	} else {
		res.Explanation, res.Source, res.FallbackReason = fb, SourceFallback, reason
	}

	// 3. A human's decision is never lost because the model failed or skipped the tool.
	if req.Decision != nil && tb.recorded == nil {
		c, err := rec.Record(req.ClaimID, req.Decision.Disposition, req.Decision.Notes)
		if err != nil {
			return nil, fmt.Errorf("record investigator decision: %w", err)
		}
		tb.recorded = c
	}

	res.Recorded = tb.recorded
	res.FollowUps = append(res.FollowUps, tb.cfResults...)
	res.Trace = append(res.Trace, tb.trace...)
	res.Usage.ToolCalls = len(tb.trace)
	return res, nil
}

// ErrInvalid marks a rejected request.
var ErrInvalid = errors.New("invalid request")

// runLLM drives the tool-use conversation. It returns a validated explanation, or nil
// plus a fallback reason code.
func (a *Agent) runLLM(parent context.Context, tb *Toolbox, rep *scoring.Report, req Request, res *Result) (*Explanation, string) {
	if a.Provider == nil {
		return nil, ReasonDisabled
	}
	// Spending guard: when exhausted, skip the model entirely (no API call is made).
	if !a.Budget.Allow() {
		a.Logger.Printf("agent LLM budget exhausted: %+v", a.Budget.Status())
		return nil, ReasonBudget
	}
	defer func() { a.Budget.Commit(res.Usage.InputTokens + res.Usage.OutputTokens) }()
	ctx, cancel := context.WithTimeout(parent, RunTimeout)
	defer cancel()

	brief := briefFor(rep, req.Decision, res.RecommendedActions)
	tb.absorb(brief)
	tb.scores[rep.Score] = true

	// The model is offered the tools the MCP server lists, and every call goes over MCP.
	tools, err := openMCPTools(ctx, tb, Specs())
	if err != nil {
		a.Logger.Printf("agent MCP tool channel: %v", err)
		return nil, ReasonToolChannel
	}
	defer tools.Close()
	res.Tools = tools.channel

	conv := a.Provider.NewConversation(systemPrompt, tools.Specs())
	var (
		results []ToolResult
		text    = brief
		repairs = 0
	)
	for turn := 0; turn < MaxTurns; turn++ {
		t, err := conv.Send(ctx, results, text)
		res.Usage.Turns++
		if err != nil {
			a.Logger.Printf("agent model error: %v", err)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ReasonTimeout
			}
			return nil, ReasonLLMError
		}
		res.Usage.InputTokens += t.InTokens
		res.Usage.OutputTokens += t.OutTokens
		if t.Refused {
			return nil, ReasonRefused
		}
		if t.Truncated {
			return nil, ReasonTruncated
		}

		if len(t.Calls) > 0 {
			// Another round trip would resend the whole history: stop if the run is too large.
			if res.Usage.InputTokens+res.Usage.OutputTokens > MaxRunTokens {
				a.Logger.Printf("agent run exceeded %d tokens; stopping", MaxRunTokens)
				return nil, ReasonTokenLimit
			}
			results, text = results[:0], ""
			for _, c := range t.Calls {
				content, isErr, abort, err := tools.Call(ctx, c)
				if err != nil {
					a.Logger.Printf("agent tool call: %v", err)
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						return nil, ReasonTimeout
					}
					return nil, ReasonToolChannel
				}
				results = append(results, ToolResult{CallID: c.ID, Content: content, IsError: isErr})
				if abort != nil {
					a.Logger.Printf("agent run aborted: %v", abort)
					return nil, ReasonViolations
				}
			}
			continue
		}

		// No tool calls: this is the final answer. Validate it.
		exp, err := parseExplanation(t.Text)
		if err == nil {
			err = validateExplanation(exp, tb.seen, tb.scores)
		}
		if err == nil {
			return exp, ""
		}
		a.Logger.Printf("agent output rejected: %v", err)
		if repairs >= 1 {
			return nil, ReasonUngrounded
		}
		repairs++
		results = nil
		text = "Your reply was rejected: " + err.Error() + ". Reply again with only the corrected JSON object, using only identifiers returned by the tools."
	}
	return nil, ReasonTurnLimit
}

// briefFor builds the opening user message: the authoritative deterministic analysis
// plus the task. It contains no claimant names and no free-text notes: the investigator's
// notes are attached by the system, never shown to the model.
func briefFor(rep *scoring.Report, d *Decision, plan *actions.Plan) string {
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
	var verifications []string
	if plan != nil {
		for _, a := range plan.Actions {
			verifications = append(verifications, a.Title)
		}
	}
	analysis, _ := json.Marshal(map[string]any{
		"target_claim": rep.TargetClaim, "investigation_priority_score": rep.Score, "band": rep.Band,
		"connected_claims": rep.ConnectedClaims, "factors": fs, "prior_case_count": len(rep.PriorCases),
		"household_explained": rep.HouseholdExplained, "recommended_verifications": nonNil(verifications),
	})
	var b strings.Builder
	b.WriteString("Investigate target claim ")
	b.WriteString(rep.TargetClaim)
	b.WriteString(".\n\nDeterministic analysis (authoritative; computed by the system, do not recompute or alter it):\n")
	b.Write(analysis)
	b.WriteString("\n\nUse the tools to inspect the claim, confirm the evidence paths and any relevant prior outcomes, then write the final JSON answer.")
	if d != nil {
		b.WriteString("\n\nThe investigator has supplied a decision for this claim: disposition=" + d.Disposition +
			". After finishing the investigation, call RecordInvestigatorDisposition with the target claim id and exactly that disposition.")
	} else {
		b.WriteString("\n\nNo investigator decision was supplied. Do not call RecordInvestigatorDisposition.")
	}
	return b.String()
}
