package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"claimgraph/internal/claims"
	"claimgraph/internal/disposition"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/investigate"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Hard limits on what one agent run may do.
const (
	MaxToolCalls       = 10    // tool calls per run (duplicates included)
	MaxToolInputBytes  = 4096  // size of one tool call's JSON input
	MaxToolResultBytes = 16000 // size of one tool result sent back to the model
	MaxCounterfactuals = 3     // RunCounterfactual calls per run
	MaxEntityIDs       = 10    // entity ids per GetPriorCases call
	MaxPriorCases      = 20
	MaxViolations      = 3 // scope/allowlist violations before the run is aborted
)

// The eight allowlisted tool names. Nothing else can be executed. The last two run
// FalkorDB graph algorithms (algo.WCC + algo.MSF, algo.SPpaths); all are read-only except
// RecordInvestigatorDisposition, which only relays a human's decision.
const (
	ToolGetClaim          = "GetClaim"
	ToolFindRelatedClaims = "FindRelatedClaims"
	ToolGetEvidencePaths  = "GetEvidencePaths"
	ToolGetPriorCases     = "GetPriorCases"
	ToolRunCounterfactual = "RunCounterfactual"
	ToolRecordDisposition = "RecordInvestigatorDisposition"
	ToolGetClaimRing      = "GetClaimRing"
	ToolGetStrongestLink  = "GetStrongestLink"
)

// Output caps for the algorithm tools (the full result is available through the API).
const (
	maxRelatedToModel      = 20
	maxPathsToModel        = 2
	maxRingMembersToModel  = 25
	maxRingEntitiesToModel = 10
	maxBackboneToModel     = 30
)

// Decision is a disposition explicitly supplied by a human investigator for this run.
// The agent can only relay it; it can never originate one.
type Decision struct {
	Disposition string `json:"disposition"`
	Notes       string `json:"notes,omitempty"`
}

// TraceEntry is the audit record of one tool call.
type TraceEntry struct {
	Seq        int    `json:"seq"`
	Tool       string `json:"tool"`
	Input      string `json:"input"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// toolRejection is an error whose message is safe to show the model.
type toolRejection struct {
	msg       string
	violation bool // counts toward MaxViolations
}

func (e *toolRejection) Error() string { return e.msg }

func reject(msg string) error    { return &toolRejection{msg: msg} }
func violation(msg string) error { return &toolRejection{msg: msg, violation: true} }

// Toolbox executes the six tools for exactly one target claim. A fresh Toolbox is
// created per run, so no state leaks between runs or between claims.
type Toolbox struct {
	g        *falkordb.Graph
	rec      *disposition.Recorder
	log      *log.Logger
	target   string
	decision *Decision

	known      map[string]bool // claims the agent may reference: target + claims revealed by FindRelatedClaims, GetClaimRing or GetStrongestLink
	related    *investigate.Result
	entityIDs  map[string]bool // entity ids revealed by evidence tools this run
	seen       map[string]bool // every identifier token the model has been shown this run
	cache      map[string]string
	calls      int
	cfRuns     int
	violations int
	trace      []TraceEntry
	cfResults  []CounterfactualSummary
	recorded   *disposition.Case
	scores     map[int]bool // scores the model has legitimately been shown
}

func newToolbox(g *falkordb.Graph, rec *disposition.Recorder, lg *log.Logger, target string, d *Decision) *Toolbox {
	return &Toolbox{
		g: g, rec: rec, log: lg, target: target, decision: d,
		known: map[string]bool{target: true}, entityIDs: map[string]bool{}, seen: map[string]bool{},
		cache: map[string]string{}, scores: map[int]bool{},
	}
}

// Specs returns the allowlisted tools exactly as offered to the model.
func Specs() []ToolSpec {
	id := map[string]any{"type": "string", "description": "Claim id."}
	return []ToolSpec{
		{Name: ToolGetClaim, Description: "Get the stored facts and ordinary policy-check status of the target claim or of a related claim already discovered in this run.",
			Properties: map[string]any{"claim_id": id}, Required: []string{"claim_id"}},
		{Name: ToolFindRelatedClaims, Description: "Query the graph for claims connected to the target claim through shared entities, up to max_hops claim-to-claim transitions (1-3, default 3). Must be called before GetEvidencePaths.",
			Properties: map[string]any{"claim_id": id, "max_hops": map[string]any{"type": "integer", "minimum": 1, "maximum": 3}}, Required: []string{"claim_id"}},
		{Name: ToolGetEvidencePaths, Description: "Get the exact entity chains, evidence ids and timestamps that connect the target claim to one related claim discovered by FindRelatedClaims.",
			Properties: map[string]any{"claim_id": id, "related_claim_id": map[string]any{"type": "string", "description": "A related claim id returned by FindRelatedClaims."}},
			Required:   []string{"claim_id", "related_claim_id"}},
		{Name: ToolGetPriorCases, Description: "Retrieve previously recorded investigation outcomes on other claims that use the given entities (entity ids returned by the evidence tools). Outcomes are context; only an older human-confirmed fraud case affects the score.",
			Properties: map[string]any{"entity_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": MaxEntityIDs}}, Required: []string{"entity_ids"}},
		{Name: ToolRunCounterfactual, Description: "Read-only what-if: recompute the investigation-priority score as if one evidence category were ignored. Does not change any data.",
			Properties: map[string]any{"claim_id": id, "exclude_signal_type": map[string]any{"type": "string", "enum": investigate.ScoringCategories}},
			Required:   []string{"claim_id", "exclude_signal_type"}},
		{Name: ToolGetClaimRing, Description: "Run the weakly-connected-components graph algorithm over strong evidence only (claimant payout accounts, private devices, phone/email, addresses, same claimant) and return the ring that contains the target claim, at ANY depth: members, shared identifiers, the identifier shared by the most members, filing span, recorded outcomes, and the minimum spanning backbone of evidence links. Repair shops and IP addresses are excluded. Ring membership is context, not proof of wrongdoing, and does not change the score.",
			Properties: map[string]any{"claim_id": id}, Required: []string{"claim_id"}},
		{Name: ToolGetStrongestLink, Description: "Run a weighted shortest-path graph algorithm to find the strongest evidence chain between the target claim and another claim already revealed in this run (by FindRelatedClaims, GetClaimRing or an earlier GetStrongestLink), at any depth, over strong evidence only (payout account, private device, phone/email, address, same claimant). Stronger evidence costs less.",
			Properties: map[string]any{"claim_id": id, "other_claim_id": map[string]any{"type": "string", "description": "A claim id revealed earlier in this run."}},
			Required:   []string{"claim_id", "other_claim_id"}},
		{Name: ToolRecordDisposition, Description: "Save the human investigator's decision. Only succeeds when the investigator supplied this exact decision for this run; you cannot choose a disposition yourself. Do not call this unless the task message says the investigator supplied a decision.",
			Properties: map[string]any{"claim_id": id,
				"disposition": map[string]any{"type": "string", "enum": []string{disposition.ConfirmedFraud, disposition.NotSubstantiated, disposition.NeedsInfo}}},
			Required: []string{"claim_id", "disposition"}},
	}
}

// Execute runs one tool call under every guard. It returns the content to send back
// to the model and whether it is an error. A non-nil abort error means the run must
// stop (too many violations).
func (tb *Toolbox) Execute(call ToolCall) (content string, isErr bool, abort error) {
	start := time.Now()
	entry := TraceEntry{Seq: len(tb.trace) + 1, Tool: call.Name, Input: clip(string(call.Input), 300)}
	finish := func(c string, err error) (string, bool, error) {
		entry.DurationMs = time.Since(start).Milliseconds()
		entry.OK = err == nil
		if err != nil {
			entry.Error = clip(err.Error(), 200)
		}
		tb.trace = append(tb.trace, entry)
		if err == nil {
			tb.absorb(c)
			return c, false, nil
		}
		var tr *toolRejection
		if errors.As(err, &tr) {
			if tr.violation {
				tb.violations++
				if tb.violations >= MaxViolations {
					return tr.msg, true, fmt.Errorf("aborted after %d scope violations", tb.violations)
				}
			}
			return tr.msg, true, nil
		}
		tb.log.Printf("agent tool %s failed: %v", call.Name, err) // details stay server-side
		return "tool failed due to an internal error", true, nil
	}

	if tb.calls >= MaxToolCalls {
		// A limit, not an attack: parallel calls in one turn cannot see this message in time.
		return finish("", reject("tool call budget exhausted; write your final answer now without further tool calls"))
	}
	tb.calls++

	if len(call.Input) > MaxToolInputBytes {
		return finish("", violation("tool input too large"))
	}
	key := call.Name + "|" + canonical(call.Input)
	if c, ok := tb.cache[key]; ok {
		return finish(c, nil) // identical repeat call: served from this run's cache
	}

	var (
		out any
		err error
	)
	switch call.Name {
	case ToolGetClaim:
		out, err = tb.getClaim(call.Input)
	case ToolFindRelatedClaims:
		out, err = tb.findRelated(call.Input)
	case ToolGetEvidencePaths:
		out, err = tb.evidencePaths(call.Input)
	case ToolGetPriorCases:
		out, err = tb.priorCases(call.Input)
	case ToolRunCounterfactual:
		out, err = tb.counterfactual(call.Input)
	case ToolRecordDisposition:
		out, err = tb.recordDisposition(call.Input)
	case ToolGetClaimRing:
		out, err = tb.claimRing(call.Input)
	case ToolGetStrongestLink:
		out, err = tb.strongestLink(call.Input)
	default:
		return finish("", violation(fmt.Sprintf("unknown tool %q: only the listed tools exist", clip(call.Name, 40))))
	}
	if err != nil {
		return finish("", err)
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		return finish("", merr)
	}
	if len(b) > MaxToolResultBytes {
		return finish("", reject("result too large; narrow the request"))
	}
	tb.cache[key] = string(b)
	return finish(string(b), nil)
}

// ---- tools ----

type claimIn struct {
	ClaimID string `json:"claim_id"`
}

func (tb *Toolbox) getClaim(raw []byte) (any, error) {
	var in claimIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if !tb.known[in.ClaimID] {
		return nil, violation("claim is outside this investigation: only the target claim and related claims found by FindRelatedClaims may be inspected")
	}
	c, err := claims.Get(tb.g, in.ClaimID)
	if errors.Is(err, claims.ErrNotFound) {
		return nil, reject("claim not found")
	}
	if err != nil {
		return nil, err
	}
	// Personal data minimisation: the agent never sees names or free-text notes.
	return map[string]any{
		"claim_id": c.ClaimID, "status": c.Status, "filing_date": c.FilingDate, "incident_date": c.IncidentDate,
		"amount": c.Amount, "loss_type": c.LossType, "policy_check_status": c.PolicyCheckStatus,
		"claimant_id": idOf(c.Claimant), "policy": c.Policy, "vehicle_id": idOf(c.Vehicle),
		"repair_shop_id": idOf(c.RepairShop), "payout_destination": c.Payout, "tow_provider_id": idOf(c.TowProvider),
		"device": c.Device, "ip_address": c.IP, "recorded_disposition": c.Disposition,
	}, nil
}

type findIn struct {
	ClaimID string `json:"claim_id"`
	MaxHops int    `json:"max_hops"`
}

func (tb *Toolbox) findRelated(raw []byte) (any, error) {
	var in findIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClaimID != tb.target {
		return nil, violation("claim_id must be the target claim of this investigation")
	}
	if in.MaxHops < 0 || in.MaxHops > investigate.MaxHopsLimit {
		return nil, reject("max_hops must be between 1 and 3")
	}
	res, err := investigate.FindRelatedClaims(tb.g, in.ClaimID, in.MaxHops)
	if err != nil {
		return nil, err
	}
	tb.related = res
	type related struct {
		ClaimID    string   `json:"claim_id"`
		Hops       int      `json:"hops"`
		FilingDate string   `json:"filing_date"`
		Paths      []string `json:"paths"` // at most maxPathsToModel per claim; use GetEvidencePaths for detail
	}
	// Every related claim is in scope, but only the first maxRelatedToModel (closest, then
	// strongest evidence) are listed: on a large book a claim can have 50 related claims,
	// which would not fit in one tool result.
	out := make([]related, 0, len(res.RelatedClaims))
	for _, rc := range res.RelatedClaims {
		tb.known[rc.ClaimID] = true
		tb.noteEntities(rc)
		if len(out) == maxRelatedToModel {
			continue
		}
		r := related{ClaimID: rc.ClaimID, Hops: rc.Hops, FilingDate: rc.FilingDate}
		for i, p := range rc.Paths {
			if i == maxPathsToModel {
				break
			}
			r.Paths = append(r.Paths, p.Description)
		}
		out = append(out, r)
	}
	return map[string]any{"target_claim": res.TargetClaim, "max_hops": res.MaxHops, "related_claims": out,
		"related_total": len(res.RelatedClaims), "related_shown": len(out),
		"shared_infrastructure": infrastructureForModel(res.InfrastructureLinks),
		"truncated":             res.Truncated, "message": res.Message}, nil
}

// infrastructureForModel summarizes busy shared identifiers (counted once as weak evidence;
// their users are not connections).
func infrastructureForModel(links []investigate.InfrastructureLink) []string {
	out := []string{}
	for _, l := range links {
		out = append(out, fmt.Sprintf("%s %s (%s) is shared infrastructure used by %d other claims; weak evidence, not a connection",
			l.EntityType, l.EntityID, l.Category, l.OtherClaims))
	}
	return out
}

type evidenceIn struct {
	ClaimID        string `json:"claim_id"`
	RelatedClaimID string `json:"related_claim_id"`
}

func (tb *Toolbox) evidencePaths(raw []byte) (any, error) {
	var in evidenceIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClaimID != tb.target {
		return nil, violation("claim_id must be the target claim of this investigation")
	}
	if tb.related == nil {
		return nil, reject("call FindRelatedClaims first")
	}
	for _, rc := range tb.related.RelatedClaims {
		if rc.ClaimID != in.RelatedClaimID {
			continue
		}
		type step struct {
			investigate.Step
			Category string `json:"category"`
		}
		type path struct {
			Hops        int    `json:"hops"`
			Description string `json:"description"`
			Steps       []step `json:"steps"`
		}
		// The paths come from a dedicated FalkorDB query with both claims bound (not from
		// the cached related-claims list), searched only as deep as FindRelatedClaims found.
		found, err := investigate.EvidencePaths(tb.g, tb.target, rc.ClaimID, rc.Hops, tb.related.ExcludedCategory)
		if err != nil {
			return nil, reject("evidence lookup failed; use the paths already returned by FindRelatedClaims")
		}
		if len(found) == 0 {
			return nil, reject("the graph returned no path between these claims")
		}
		paths := make([]path, 0, len(found))
		for _, p := range found {
			pp := path{Hops: p.Hops, Description: p.Description}
			for _, s := range p.Steps {
				pp.Steps = append(pp.Steps, step{Step: s, Category: investigate.SignalCategory(s)})
			}
			paths = append(paths, pp)
		}
		tb.noteEntities(rc)
		return map[string]any{"claim_id": tb.target, "related_claim_id": rc.ClaimID, "related_filing_date": rc.FilingDate,
			"hops": rc.Hops, "paths": paths}, nil
	}
	return nil, violation("related_claim_id was not returned by FindRelatedClaims in this run")
}

type priorIn struct {
	EntityIDs []string `json:"entity_ids"`
}

const cypherPriorCasesByEntity = `
	MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c:Claim)-[:PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]->(e)
	WHERE coalesce(e.account_id, e.device_id, e.ip, e.shop_id) IN $ids AND c.claim_id <> $target
	RETURN ic.case_id AS case_id, c.claim_id AS claim_id, ic.disposition AS disposition, ic.timestamp AS recorded_at,
	       coalesce(e.account_id, e.device_id, e.ip, e.shop_id) AS entity_id
	UNION
	MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c:Claim)-[:FILED_BY]->(:Claimant)-[:HAS_CONTACT|RESIDES_AT]->(e)
	WHERE coalesce(e.value, e.address_id) IN $ids AND c.claim_id <> $target
	RETURN ic.case_id AS case_id, c.claim_id AS claim_id, ic.disposition AS disposition, ic.timestamp AS recorded_at,
	       coalesce(e.value, e.address_id) AS entity_id`

func (tb *Toolbox) priorCases(raw []byte) (any, error) {
	var in priorIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if len(in.EntityIDs) == 0 || len(in.EntityIDs) > MaxEntityIDs {
		return nil, reject(fmt.Sprintf("entity_ids must contain 1 to %d ids", MaxEntityIDs))
	}
	ids := make([]any, 0, len(in.EntityIDs))
	for _, raw := range in.EntityIDs {
		id := stripTypePrefix(raw)
		if !tb.seen[id] {
			return nil, violation("entity id was never shown to you in this run; use ids from the analysis or tool results")
		}
		ids = append(ids, id)
	}
	res, err := tb.g.ROQuery(cypherPriorCasesByEntity, map[string]any{"ids": ids, "target": tb.target}, nil)
	if err != nil {
		return nil, err
	}
	type pc struct {
		CaseID      string `json:"case_id"`
		ClaimID     string `json:"claim_id"`
		Disposition string `json:"disposition"`
		RecordedAt  string `json:"recorded_at,omitempty"`
		ViaEntity   string `json:"via_entity"`
	}
	var cases []pc
	for res.Next() {
		get := res.Record().Get
		s := func(k string) string { v, _ := get(k); x, _ := v.(string); return x }
		cases = append(cases, pc{s("case_id"), s("claim_id"), s("disposition"), s("recorded_at"), s("entity_id")})
	}
	sort.Slice(cases, func(i, j int) bool {
		if cases[i].CaseID != cases[j].CaseID {
			return cases[i].CaseID < cases[j].CaseID
		}
		return cases[i].ViaEntity < cases[j].ViaEntity
	})
	truncated := false
	if len(cases) > MaxPriorCases {
		cases, truncated = cases[:MaxPriorCases], true
	}
	if cases == nil {
		cases = []pc{}
	}
	// Free-text investigator notes are deliberately never returned to the model.
	return map[string]any{"prior_cases": cases, "truncated": truncated,
		"scoring_note": "Outcomes are context. Only an older claim with disposition confirmed_fraud adds points; not_substantiated and needs_info never do."}, nil
}

type cfIn struct {
	ClaimID string `json:"claim_id"`
	Exclude string `json:"exclude_signal_type"`
}

// CounterfactualSummary is the compact form of a counterfactual shown to the model
// and returned to API clients.
type CounterfactualSummary struct {
	ExcludedCategory    string                 `json:"excluded_category"`
	OriginalScore       int                    `json:"original_score"`
	OriginalBand        string                 `json:"original_band"`
	CounterfactualScore int                    `json:"counterfactual_score"`
	CounterfactualBand  string                 `json:"counterfactual_band"`
	Delta               int                    `json:"delta"`
	RemovedFactors      []scoring.FactorChange `json:"removed_factors"`
	StillJustifies      bool                   `json:"still_justifies_investigation"`
	SurvivingClaims     []string               `json:"surviving_connected_claims"`
	Explanation         string                 `json:"explanation"`
}

func (tb *Toolbox) counterfactual(raw []byte) (any, error) {
	var in cfIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClaimID != tb.target {
		return nil, violation("claim_id must be the target claim of this investigation")
	}
	if !investigate.ValidCategory(in.Exclude) {
		return nil, reject("exclude_signal_type must be one of " + strings.Join(investigate.ScoringCategories, ", "))
	}
	if tb.cfRuns >= MaxCounterfactuals {
		return nil, reject("counterfactual budget for this run is exhausted")
	}
	tb.cfRuns++
	cf, err := scoring.RunCounterfactual(tb.g, in.ClaimID, 0, in.Exclude)
	if err != nil {
		return nil, err
	}
	sum := CounterfactualSummary{
		ExcludedCategory: cf.ExcludedCategory, OriginalScore: cf.Original.Score, OriginalBand: cf.Original.Band,
		CounterfactualScore: cf.Counterfactual.Score, CounterfactualBand: cf.Counterfactual.Band, Delta: cf.Delta,
		RemovedFactors: cf.RemovedFactors, StillJustifies: cf.StillJustifies, Explanation: cf.Explanation,
		SurvivingClaims: []string{},
	}
	for _, rc := range cf.Counterfactual.Investigation.RelatedClaims {
		sum.SurvivingClaims = append(sum.SurvivingClaims, rc.ClaimID)
	}
	tb.cfResults = append(tb.cfResults, sum)
	tb.scores[sum.OriginalScore], tb.scores[sum.CounterfactualScore] = true, true
	return sum, nil
}

type recordIn struct {
	ClaimID     string `json:"claim_id"`
	Disposition string `json:"disposition"`
	Notes       string `json:"notes"` // tolerated but ignored: the system supplies the investigator's notes
}

func (tb *Toolbox) recordDisposition(raw []byte) (any, error) {
	var in recordIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if tb.decision == nil {
		return nil, violation("no investigator decision was supplied for this run; you may not record a disposition")
	}
	if in.ClaimID != tb.target || in.Disposition != tb.decision.Disposition {
		return nil, violation("the disposition must exactly match the decision supplied by the investigator")
	}
	if tb.recorded != nil {
		return nil, reject("the investigator's decision was already recorded")
	}
	c, err := tb.rec.Record(tb.target, tb.decision.Disposition, tb.decision.Notes)
	if err != nil {
		return nil, err
	}
	tb.recorded = c
	return map[string]any{"recorded": true, "case_id": c.CaseID, "claim_id": c.ClaimID, "disposition": c.Disposition}, nil
}

// ---- graph algorithm tools ----

func (tb *Toolbox) claimRing(raw []byte) (any, error) {
	var in claimIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClaimID != tb.target {
		return nil, violation("claim_id must be the target claim of this investigation")
	}
	ring, err := graphalgo.RingOf(tb.g, in.ClaimID)
	if errors.Is(err, graphalgo.ErrOverlayMissing) || errors.Is(err, graphalgo.ErrTimeout) {
		return nil, reject(err.Error()) // a known condition the caller should be told about
	}
	if err != nil {
		return nil, err
	}
	type member struct {
		ClaimID      string   `json:"claim_id"`
		FilingDate   string   `json:"filing_date,omitempty"`
		Dispositions []string `json:"recorded_dispositions,omitempty"`
	}
	members := make([]member, 0, len(ring.Members))
	for _, m := range ring.Members {
		tb.known[m.ClaimID] = true // ring members are in scope for GetClaim / GetStrongestLink
		if len(members) < maxRingMembersToModel {
			members = append(members, member{m.ClaimID, m.FilingDate, m.Dispositions})
		}
	}
	entities := ring.SharedEntities
	for _, e := range entities {
		tb.entityIDs[e.ID] = true
	}
	if len(entities) > maxRingEntitiesToModel {
		entities = entities[:maxRingEntitiesToModel]
	}
	backbone := make([]string, 0, len(ring.Backbone))
	for _, l := range ring.Backbone {
		if len(backbone) == maxBackboneToModel {
			break
		}
		backbone = append(backbone, fmt.Sprintf("%s(%s) -[%s]- %s(%s)", l.From.Type, l.From.ID, l.Rel, l.To.Type, l.To.ID))
	}
	return map[string]any{
		"algorithm": ring.Algorithm, "ring_id": ring.RingID, "target_claim": ring.TargetClaim, "size": ring.Size,
		"oversized": ring.Oversized, "members": members, "members_shown": len(members),
		"shared_entities": entities, "hub_entity": ring.HubEntity,
		"first_filed": ring.FirstFiled, "last_filed": ring.LastFiled, "filing_span_days": ring.SpanDays,
		"confirmed_fraud_cases_in_ring": ring.ConfirmedFraudCases, "other_recorded_cases_in_ring": ring.OtherRecordedCases,
		"backbone_links": backbone, "ring_indicators": ring.Traits, "message": ring.Message, "note": ring.Note,
	}, nil
}

type linkIn struct {
	ClaimID      string `json:"claim_id"`
	OtherClaimID string `json:"other_claim_id"`
}

func (tb *Toolbox) strongestLink(raw []byte) (any, error) {
	var in linkIn
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClaimID != tb.target {
		return nil, violation("claim_id must be the target claim of this investigation")
	}
	if in.OtherClaimID == tb.target || !tb.known[in.OtherClaimID] {
		return nil, violation("other_claim_id must be a different claim revealed earlier in this run")
	}
	link, err := graphalgo.FindStrongestLink(tb.g, in.ClaimID, in.OtherClaimID)
	if errors.Is(err, graphalgo.ErrOverlayMissing) || errors.Is(err, graphalgo.ErrTimeout) {
		return nil, reject(err.Error())
	}
	if err != nil {
		return nil, err
	}
	for _, c := range link.Claims {
		tb.known[c] = true
	}
	for _, l := range link.Links {
		for _, n := range []graphalgo.GraphNode{l.From, l.To} {
			if n.Type != "Claim" && n.Type != "Claimant" {
				tb.entityIDs[n.ID] = true
			}
		}
	}
	return map[string]any{
		"algorithm": link.Algorithm, "from_claim": link.FromClaim, "to_claim": link.ToClaim, "found": link.Found,
		"total_evidence_cost": link.TotalCost, "claims_on_path": link.Claims, "evidence_used": link.Evidence,
		"path": link.Description, "note": link.Note,
	}, nil
}

// ---- helpers ----

// decodeStrict parses a tool input object, rejecting unknown fields and trailing data.
// An unknown field is a violation: it is how an attempt to smuggle in e.g. a query
// string would look.
func decodeStrict(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return violation("invalid tool input: " + clip(err.Error(), 120))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return violation("invalid tool input: trailing data")
	}
	return nil
}

func canonical(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v) // map keys are sorted by encoding/json
	return string(b)
}

func idOf(e *claims.Entity) string {
	if e == nil {
		return ""
	}
	return e.ID
}

func stripTypePrefix(s string) string {
	if i := strings.Index(s, ":"); i > 0 && i < len(s)-1 && !strings.ContainsAny(s[:i], " +-0123456789") {
		return s[i+1:]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// noteEntities records the entity ids revealed by a related claim's evidence paths.
func (tb *Toolbox) noteEntities(rc investigate.RelatedClaim) {
	for _, p := range rc.Paths {
		for _, s := range p.Steps {
			tb.entityIDs[s.EntityID] = true
		}
	}
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9+][A-Za-z0-9_\-.+@]*`)

// tokens splits text into identifier-like tokens (trailing dots trimmed).
func tokens(s string) []string {
	raw := tokenRe.FindAllString(s, -1)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		out = append(out, strings.TrimRight(t, ".-"))
	}
	return out
}

// absorb records every identifier the model is shown, so its answer can later be
// checked against what it was actually given.
func (tb *Toolbox) absorb(content string) {
	for _, t := range tokens(content) {
		tb.seen[t] = true
	}
}
