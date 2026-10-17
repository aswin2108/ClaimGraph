// Package investigate discovers claims connected to a target claim through
// shared entities and returns the actual connecting paths.
//
// FalkorDB performs the multi-hop traversal (see traverse.go) and returns complete
// evidence paths; Go enforces limits, applies counterfactual exclusions and keeps
// results deterministic. No claim-to-claim edge is ever stored or invented.
package investigate

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"claimgraph/internal/graph"
	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Limits that keep an investigation bounded.
const (
	MaxHopsLimit     = 3 // maximum claim-to-claim transitions
	DefaultMaxHops   = 3
	MaxRelatedClaims = 50 // related claims returned
	MaxPathsPerClaim = 5  // shortest paths kept per related claim
	MaxOwnClaims     = 20 // other claims of the target's claimant listed as context
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
	EntityClaims  int    `json:"entity_claims,omitempty"`  // evidence links into the entity (claims using it)
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
	Truncated        bool           `json:"truncated"`       // a row limit may have hidden evidence (data may be incomplete)
	PathsTrimmed     bool           `json:"paths_trimmed"`   // some claims show only MaxPathsPerClaim of their equally short paths
	ConnectedTotal   int            `json:"connected_total"` // every connected claim; RelatedClaims lists at most MaxRelatedClaims
	// Connected is every connected claim (not only the listed ones). Scoring uses it for
	// breadth, timing and prior outcomes, so the list cap never changes a score.
	Connected []ConnectedClaim `json:"-"`
	// Direct is every claim connected by ONE transition, with all its paths, before the
	// MaxRelatedClaims listing cap. Scoring reads the signal categories (payout, device,
	// contact, address, repair shop, public IP) and household co-residence from it, so a
	// direct link is never lost because 50 claims with stronger multi-hop evidence filled
	// the list. Nil for results that were not capped (then RelatedClaims is complete).
	Direct  []RelatedClaim `json:"-"`
	Message string         `json:"message,omitempty"`
	// Outcome is an explicit result type, so "nothing found" is a stated answer, never an
	// empty list to interpret: connected | infrastructure_only | no_supporting_connection.
	Outcome string          `json:"outcome"`
	Stats   *TraversalStats `json:"traversal_stats,omitempty"` // how the answer was computed (S2)
	// Queries are the Cypher queries this traversal sent to FalkorDB, exactly as sent (template +
	// parameters), for the UI's "Show Cypher". The agent's tools never pass them to the model.
	Queries []graph.ExecutedQuery `json:"cypher_queries,omitempty"`
	// Infrastructure the target uses (see InfrastructureMinClaims): reported with exact
	// counts instead of one related claim per user. Nil when there is none.
	InfrastructureLinks []InfrastructureLink `json:"infrastructure_links,omitempty"`
	// OwnClaims are the other claims filed by the target's claimant. A person's own claims are
	// never links between claims (at any depth), so they are listed here as context, add no
	// points and never appear in RelatedClaims. Nil when the claimant filed nothing else.
	OwnClaims *OwnClaims `json:"same_claimant_claims,omitempty"`
}

// OwnClaims lists the target claimant's other claims, newest first (at most MaxOwnClaims).
type OwnClaims struct {
	ClaimantID string     `json:"claimant_id"`
	Total      int        `json:"total"`
	Claims     []OwnClaim `json:"claims"`
}

// OwnClaim is one other claim of the target's claimant.
type OwnClaim struct {
	ClaimID      string `json:"claim_id"`
	FilingDate   string `json:"filing_date,omitempty"`
	IncidentDate string `json:"incident_date,omitempty"`
	// Decision is the investigator's recorded disposition on that claim, if any (context only:
	// it never adds points to this claim).
	Decision string `json:"decision,omitempty"`
}

// ownClaims looks up the target claimant's other claims. The query is appended to log (when
// not nil). Both engines report it with this one function: it is a lookup, not traversal.
func ownClaims(g *falkordb.Graph, claimID string, log *[]graph.ExecutedQuery) (*OwnClaims, error) {
	params := map[string]any{"id": claimID, "limit": MaxOwnClaims}
	t0 := time.Now()
	r, err := g.ROQuery(cypherSameClaimant, params, falkordb.NewQueryOptions().SetTimeout(queryTimeoutMs))
	if err != nil {
		return nil, fmt.Errorf("same-claimant claims: %w", err)
	}
	var out *OwnClaims
	if r.Next() {
		rec := r.Record()
		get := func(k string) any { v, _ := rec.Get(k); return v }
		total, _ := get("total").(int64)
		ids, _ := get("ids").([]interface{})
		filed, _ := get("filed").([]interface{})
		incident, _ := get("incident").([]interface{})
		decisions, _ := get("decisions").([]interface{})
		out = &OwnClaims{ClaimantID: str(get("claimant")), Total: int(total), Claims: []OwnClaim{}}
		for i, id := range ids {
			c := OwnClaim{ClaimID: str(id)}
			if i < len(filed) {
				c.FilingDate = str(filed[i])
			}
			if i < len(incident) {
				c.IncidentDate = str(incident[i])
			}
			if i < len(decisions) {
				c.Decision = str(decisions[i])
			}
			out.Claims = append(out.Claims, c)
		}
	}
	if log != nil {
		n := 0
		if out != nil {
			n = len(out.Claims)
		}
		*log = append(*log, graph.Record("Other claims filed by the same claimant (context, no points)", cypherSameClaimant, params, n, t0))
	}
	return out, nil
}

// Outcome types.
const (
	OutcomeConnected          = "connected"
	OutcomeInfrastructureOnly = "infrastructure_only"
	OutcomeNoConnection       = "no_supporting_connection"
)

const infrastructureOnlyMessage = "This claim is linked to others only through shared infrastructure or context-only identifiers (a busy repair shop, carrier IP, shared public computer, company network or repair vendor's settlement account); no claim is connected through evidence specific to it."

// setOutcome states what the investigation found.
func setOutcome(res *Result) {
	switch {
	case len(res.RelatedClaims) > 0:
		res.Outcome = OutcomeConnected
	case len(res.InfrastructureLinks) > 0:
		res.Outcome, res.Message = OutcomeInfrastructureOnly, infrastructureOnlyMessage
	default:
		res.Outcome, res.Message = OutcomeNoConnection, noConnectionMessage
	}
}

// ConnectedClaim is the minimal record of a connected claim used for scoring.
type ConnectedClaim struct {
	ClaimID    string
	Hops       int
	FilingDate string
	Strong     bool // reached by a path whose every step is strong evidence
}

func connectedOf(rcs []RelatedClaim) []ConnectedClaim {
	out := make([]ConnectedClaim, len(rcs))
	for i, rc := range rcs {
		out[i] = ConnectedClaim{ClaimID: rc.ClaimID, Hops: rc.Hops, FilingDate: rc.FilingDate, Strong: StrongPath(rc)}
	}
	return out
}

// StrongPath reports whether any of the claim's paths uses strong evidence on every step.
func StrongPath(rc RelatedClaim) bool {
	for _, p := range rc.Paths {
		strong := len(p.Steps) > 0
		for _, s := range p.Steps {
			if !StrongCategory(SignalCategory(s)) {
				strong = false
				break
			}
		}
		if strong {
			return true
		}
	}
	return false
}

// InfrastructureLink is a weak/context identifier the target shares with many claims.
type InfrastructureLink struct {
	Category      string `json:"category"`
	EntityType    string `json:"entity_type"`
	EntityID      string `json:"entity_id"`
	EntitySubtype string `json:"entity_subtype,omitempty"`
	EntityPublic  *bool  `json:"entity_public,omitempty"`
	Rel           string `json:"rel"`
	OtherClaims   int    `json:"other_claims"` // claims other than the target that use it
	EvidenceID    string `json:"evidence_id,omitempty"`
	RecordedAt    string `json:"recorded_at,omitempty"`
}

// TraversalStats reports how much work FalkorDB did for an investigation. Timing varies
// between runs; it is never part of the evidence or the score.
type TraversalStats struct {
	Engine        string `json:"engine"`         // where the multi-hop traversal ran
	Queries       int    `json:"queries"`        // FalkorDB round trips
	PathsReturned int    `json:"paths_returned"` // evidence paths FalkorDB returned before shortest-path selection
	DurationMs    int64  `json:"duration_ms"`    // wall clock for the whole investigation
}

// FindRelatedClaims returns claims connected to claimID through shared entities
// within maxHops claim-to-claim transitions (1..3; 0 means the default of 3).
func FindRelatedClaims(g *falkordb.Graph, claimID string, maxHops int) (*Result, error) {
	return FindRelatedClaimsExcluding(g, claimID, maxHops, "")
}

// FindRelatedClaimsExcluding is FindRelatedClaims on an altered, read-only analytical
// view: every path that uses an evidence category equal to excludeCategory is ignored
// (see SignalCategory). The graph itself is never modified. "" excludes nothing.
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
	start := time.Now()
	targetFiled, err := requireClaim(g, claimID)
	if err != nil {
		return nil, err
	}

	res := &Result{TargetClaim: claimID, TargetFilingDate: targetFiled, ExcludedCategory: excludeCategory,
		MaxHops: maxHops, RelatedClaims: []RelatedClaim{}}
	res.Queries = append(res.Queries, graph.Record("Load the target claim", cypherTargetClaim,
		map[string]any{"claim_id": claimID}, 1, start))

	chains, info, cut, err := discover(g, claimID, maxHops, excludeCategory, &res.Queries)
	if err != nil {
		return nil, err
	}
	res.Truncated = cut
	if res.InfrastructureLinks, err = infrastructure(g, claimID, excludeCategory, &res.Queries); err != nil {
		return nil, err
	}
	if res.OwnClaims, err = ownClaims(g, claimID, &res.Queries); err != nil {
		return nil, err
	}
	queries := 4 // target lookup + infrastructure summary + same-claimant lookup + single-transition paths
	if maxHops >= 2 {
		queries++ // 2..maxHops transition shapes, one UNION ALL query
	}
	defer func() {
		res.Stats = &TraversalStats{Engine: "falkordb_in_database", Queries: queries, PathsReturned: len(chains),
			DurationMs: time.Since(start).Milliseconds()}
	}()

	for claim, shortest := range shortestOnly(chains, excludeCategory) {
		hops := len(shortest[0])
		paths, trimmed := pathsFor(shortest, hops)
		if trimmed {
			res.PathsTrimmed = true
		}
		res.RelatedClaims = append(res.RelatedClaims, RelatedClaim{
			ClaimID: claim, Hops: hops, FilingDate: info[claim].filed, IncidentDate: info[claim].incident, Paths: paths})
	}

	// When more than MaxRelatedClaims are connected, keep the ones with the strongest
	// evidence (then the closest, then by id), so a very busy repair shop can never push a
	// payout or device link, direct or two transitions away, out of the list. The kept
	// claims are presented closest first, strongest evidence first, then by id.
	strength := make(map[string]int, len(res.RelatedClaims))
	for _, rc := range res.RelatedClaims {
		strength[rc.ClaimID] = strongestRank(rc)
	}
	order := func(strengthFirst bool) {
		sort.Slice(res.RelatedClaims, func(i, j int) bool {
			a, b := res.RelatedClaims[i], res.RelatedClaims[j]
			if strengthFirst && strength[a.ClaimID] != strength[b.ClaimID] {
				return strength[a.ClaimID] < strength[b.ClaimID]
			}
			if a.Hops != b.Hops {
				return a.Hops < b.Hops
			}
			if strength[a.ClaimID] != strength[b.ClaimID] {
				return strength[a.ClaimID] < strength[b.ClaimID]
			}
			return a.ClaimID < b.ClaimID
		})
	}
	res.Connected = connectedOf(res.RelatedClaims)
	res.ConnectedTotal = len(res.Connected)
	if len(res.RelatedClaims) > MaxRelatedClaims {
		res.Direct = []RelatedClaim{}
		for _, rc := range res.RelatedClaims {
			if rc.Hops == 1 {
				res.Direct = append(res.Direct, rc)
			}
		}
		order(true)
		res.RelatedClaims = res.RelatedClaims[:MaxRelatedClaims] // listing only; scoring uses Connected
	}
	order(false)
	setOutcome(res)
	return res, nil
}

// categoryRank orders evidence categories by scoring weight (lower = stronger).
var categoryRank = map[string]int{
	CatPayout: 0, CatDevice: 1, CatContact: 2, CatAddress: 3, CatRepairShop: 4, CatPublicIP: 5,
}

func strongestRank(rc RelatedClaim) int {
	best := len(categoryRank) + 1 // context-only categories rank last
	for _, p := range rc.Paths {
		for _, s := range p.Steps {
			if r, ok := categoryRank[SignalCategory(s)]; ok && r < best {
				best = r
			}
		}
	}
	return best
}

func validIDs(ids ...string) bool {
	for _, id := range ids {
		if !ingest.ValidID(id) {
			return false
		}
	}
	return true
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

// pathKey is a total order over paths: the description first, then the evidence ids and
// timestamps, so two paths that read the same but rest on different evidence records are
// always returned in the same order.
func pathKey(steps []Step) string {
	var b strings.Builder
	b.WriteString(describe(steps))
	for _, s := range steps {
		b.WriteString("|" + s.FromEvidence + "|" + s.ToEvidence + "|" + s.FromAt + "|" + s.ToAt)
	}
	return b.String()
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
