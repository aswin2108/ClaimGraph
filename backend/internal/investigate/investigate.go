// Package investigate discovers claims connected to a target claim through
// shared entities and returns the actual connecting paths.
//
// FalkorDB matches the shared-entity links (one claim-to-claim transition per
// query); Go chains up to MaxHops transitions, enforces limits and keeps
// results deterministic. No claim-to-claim edge is ever stored or invented.
package investigate

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Limits that keep an investigation bounded.
const (
	MaxHopsLimit       = 3 // maximum claim-to-claim transitions
	DefaultMaxHops     = 3
	MaxRelatedClaims   = 50  // related claims returned
	MaxPathsPerClaim   = 5   // shortest paths kept per related claim
	maxPartialPerClaim = 50  // candidate paths built per claim before trimming (bounds fan-out)
	rowLimitPerHop     = 500 // rows fetched per branch per hop
)

// ErrClaimNotFound is returned when the target claim is not in the graph.
var ErrClaimNotFound = errors.New("claim not found")

// Step is one claim-to-claim transition through a single shared entity.
type Step struct {
	FromClaim     string `json:"from_claim"`
	ToClaim       string `json:"to_claim"`
	FromClaimant  string `json:"from_claimant,omitempty"` // claimant hop for contact/address evidence
	ToClaimant    string `json:"to_claimant,omitempty"`
	EntityType    string `json:"entity_type"`
	EntityID      string `json:"entity_id"`
	EntitySubtype string `json:"entity_subtype,omitempty"` // payout type or contact type
	EntityPublic  *bool  `json:"entity_public,omitempty"`  // device / IP visibility
	FromRel       string `json:"from_rel"`
	ToRel         string `json:"to_rel"`
	FromEvidence  string `json:"from_evidence,omitempty"`
	ToEvidence    string `json:"to_evidence,omitempty"`
	FromAt        string `json:"from_recorded_at,omitempty"`
	ToAt          string `json:"to_recorded_at,omitempty"`
}

// Describe renders the step as a readable path segment.
func (s Step) Describe() string {
	if s.FromClaimant != "" {
		return fmt.Sprintf("%s -[FILED_BY]-> Claimant(%s) -[%s]-> %s(%s) <-[%s]- Claimant(%s) <-[FILED_BY]- %s",
			s.FromClaim, s.FromClaimant, s.FromRel, s.EntityType, s.EntityID, s.ToRel, s.ToClaimant, s.ToClaim)
	}
	return fmt.Sprintf("%s -[%s]-> %s(%s) <-[%s]- %s", s.FromClaim, s.FromRel, s.EntityType, s.EntityID, s.ToRel, s.ToClaim)
}

// Path is an ordered chain of steps from the target to a related claim.
type Path struct {
	Hops        int    `json:"hops"`
	Steps       []Step `json:"steps"`
	Description string `json:"description"`
}

// RelatedClaim is a claim connected to the target, with its shortest evidence paths.
type RelatedClaim struct {
	ClaimID      string `json:"claim_id"`
	Hops         int    `json:"hops"`
	FilingDate   string `json:"filing_date,omitempty"`
	IncidentDate string `json:"incident_date,omitempty"`
	Paths        []Path `json:"paths"`
}

// Result is the full investigation output.
type Result struct {
	TargetClaim      string         `json:"target_claim"`
	TargetFilingDate string         `json:"target_filing_date,omitempty"`
	ExcludedCategory string         `json:"excluded_category,omitempty"` // set for counterfactual views
	MaxHops          int            `json:"max_hops"`
	RelatedClaims    []RelatedClaim `json:"related_claims"`
	Truncated        bool           `json:"truncated"` // a limit cut off some results
	Message          string         `json:"message,omitempty"`
}

type partial struct {
	steps []Step
}

type claimInfo struct{ filed, incident string }

// FindRelatedClaims returns claims connected to claimID through shared entities
// within maxHops claim-to-claim transitions (1..3; 0 means the default of 3).
func FindRelatedClaims(g *falkordb.Graph, claimID string, maxHops int) (*Result, error) {
	return FindRelatedClaimsExcluding(g, claimID, maxHops, "")
}

// FindRelatedClaimsExcluding is FindRelatedClaims on an altered, read-only analytical
// view: every step whose evidence category equals excludeCategory is ignored (see
// SignalCategory). The graph itself is never modified. "" excludes nothing.
func FindRelatedClaimsExcluding(g *falkordb.Graph, claimID string, maxHops int, excludeCategory string) (*Result, error) {
	if excludeCategory != "" && !ValidCategory(excludeCategory) {
		return nil, fmt.Errorf("unknown evidence category %q", excludeCategory)
	}
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("invalid claim_id %q", claimID)
	}
	if maxHops == 0 {
		maxHops = DefaultMaxHops
	}
	if maxHops < 1 || maxHops > MaxHopsLimit {
		return nil, fmt.Errorf("max_hops must be between 1 and %d", MaxHopsLimit)
	}
	targetFiled, err := requireClaim(g, claimID)
	if err != nil {
		return nil, err
	}

	res := &Result{TargetClaim: claimID, TargetFilingDate: targetFiled, ExcludedCategory: excludeCategory,
		MaxHops: maxHops, RelatedClaims: []RelatedClaim{}}

	visited := map[string]bool{claimID: true}
	// frontier: claim -> all shortest paths that reach it (target has the empty path).
	frontier := map[string][]partial{claimID: {{}}}
	reached := map[string]*RelatedClaim{}
	info := map[string]claimInfo{}

	for hop := 1; hop <= maxHops && len(frontier) > 0; hop++ {
		ids := make([]any, 0, len(frontier))
		for id := range frontier {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].(string) < ids[j].(string) })

		steps, filed, rowsCut, err := oneHop(g, ids)
		if err != nil {
			return nil, err
		}
		if rowsCut {
			res.Truncated = true
		}
		for k, v := range filed {
			info[k] = v
		}

		next := map[string][]partial{}
		for _, st := range steps {
			if visited[st.ToClaim] {
				continue // already reached on a shorter (or same-level earlier) hop
			}
			if excludeCategory != "" && SignalCategory(st) == excludeCategory {
				continue // counterfactual: this evidence is ignored in the analytical view
			}
			for _, parent := range frontier[st.FromClaim] {
				if hop > 1 && len(next[st.ToClaim]) >= maxPartialPerClaim {
					res.Truncated = true // bound path fan-out on dense graphs
					continue
				}
				chain := append(append([]Step{}, parent.steps...), st)
				next[st.ToClaim] = append(next[st.ToClaim], partial{steps: chain})
			}
		}

		for claim, paths := range next {
			visited[claim] = true
			rc := &RelatedClaim{ClaimID: claim, Hops: hop, FilingDate: info[claim].filed, IncidentDate: info[claim].incident}
			sort.Slice(paths, func(i, j int) bool { return describe(paths[i].steps) < describe(paths[j].steps) })
			for _, p := range paths {
				// Direct (1-hop) paths are never capped: scoring needs every signal category.
				if hop > 1 && len(rc.Paths) == MaxPathsPerClaim {
					res.Truncated = true
					break
				}
				rc.Paths = append(rc.Paths, Path{Hops: hop, Steps: p.steps, Description: describe(p.steps)})
			}
			reached[claim] = rc
		}
		frontier = next
	}

	for _, rc := range reached {
		res.RelatedClaims = append(res.RelatedClaims, *rc)
	}
	sort.Slice(res.RelatedClaims, func(i, j int) bool {
		a, b := res.RelatedClaims[i], res.RelatedClaims[j]
		if a.Hops != b.Hops {
			return a.Hops < b.Hops
		}
		return a.ClaimID < b.ClaimID
	})
	if len(res.RelatedClaims) > MaxRelatedClaims {
		res.RelatedClaims = res.RelatedClaims[:MaxRelatedClaims]
		res.Truncated = true
	}
	if len(res.RelatedClaims) == 0 {
		res.Message = "No supporting connection to other claims was found in the graph."
	}
	return res, nil
}

// requireClaim confirms the claim exists and returns its filing date.
func requireClaim(g *falkordb.Graph, claimID string) (string, error) {
	r, err := g.ROQuery(cypherTargetClaim, map[string]any{"claim_id": claimID}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return "", ErrClaimNotFound
		}
		return "", fmt.Errorf("load target claim: %w", err)
	}
	if !r.Next() {
		return "", ErrClaimNotFound
	}
	v, _ := r.Record().Get("filing_date")
	return str(v), nil
}

// oneHop runs the parameterized one-hop query for every claim in ids.
// The bool is true when the per-branch row limit was reached (results may be incomplete).
func oneHop(g *falkordb.Graph, ids []any) ([]Step, map[string]claimInfo, bool, error) {
	r, err := g.ROQuery(cypherOneHop, map[string]any{"ids": ids, "row_limit": rowLimitPerHop + 1}, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("one-hop query: %w", err)
	}
	var steps []Step
	infos := map[string]claimInfo{}
	for r.Next() {
		rec := r.Record()
		get := func(k string) any { v, _ := rec.Get(k); return v }
		st := Step{
			FromClaim: str(get("from_claim")), ToClaim: str(get("to_claim")),
			FromClaimant: str(get("from_claimant")), ToClaimant: str(get("to_claimant")),
			EntityType: str(get("entity_type")), EntityID: str(get("entity_id")),
			EntitySubtype: str(get("entity_subtype")),
			FromRel:       str(get("from_rel")), ToRel: str(get("to_rel")),
			FromEvidence: str(get("from_evidence")), ToEvidence: str(get("to_evidence")),
			FromAt: str(get("from_at")), ToAt: str(get("to_at")),
		}
		if b, ok := get("entity_public").(bool); ok {
			st.EntityPublic = &b
		}
		steps = append(steps, st)
		infos[st.ToClaim] = claimInfo{filed: str(get("to_filed")), incident: str(get("to_incident"))}
	}
	// Deterministic order regardless of database row order.
	sort.Slice(steps, func(i, j int) bool {
		a, b := steps[i], steps[j]
		if a.FromClaim != b.FromClaim {
			return a.FromClaim < b.FromClaim
		}
		if a.ToClaim != b.ToClaim {
			return a.ToClaim < b.ToClaim
		}
		if a.EntityType != b.EntityType {
			return a.EntityType < b.EntityType
		}
		return a.EntityID < b.EntityID
	})
	return steps, infos, len(steps) > rowLimitPerHop, nil
}

func describe(steps []Step) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.Describe()
	}
	return strings.Join(parts, "  ==>  ")
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
