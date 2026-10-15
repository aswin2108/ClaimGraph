// Package graphalgo answers investigation questions with FalkorDB's built-in graph
// algorithms. Every query is a constant, parameterized, read-only template.
//
//	Question                                                     Algorithm
//	Which claims form a ring with this claim?                    algo.WCC  over the strong-evidence subgraph
//	What is the minimal evidence that holds the ring together?   algo.MSF  (minimum spanning forest, evidence cost as weight)
//	Which rings across the whole book should be reviewed first?  algo.WCC  over the whole graph
//	What is the strongest evidence chain between two claims?     algo.SPpaths (weighted, both directions, cost-capped)
//
// The strong-evidence subgraph is Claim, Claimant, claimant payout accounts, private
// devices, contact points and addresses, linked by PAID_TO, USED_DEVICE, FILED_BY,
// HAS_CONTACT and RESIDES_AT. Repair shops, IP addresses, vendor settlement accounts and
// public devices are deliberately left out: they are legitimately shared by unrelated
// people and would merge the whole book into one component.
//
// Algorithm results are investigation context and queue ordering only. They never change
// the deterministic 0-100 investigation-priority score.
//
// Algorithms evaluated and NOT used (checked against FalkorDB 4.x on our graph shape):
// algo.labelPropagation oscillates on the bipartite claim/entity graph (claims end up in
// one community and entities in another); algo.betweenness, algo.HarmonicCentrality and
// algo.pageRank treat edges as directed, and all our evidence edges point claim -> entity,
// so scores collapse to zero or to plain in-degree; algo.BFS follows a single
// relationship type; algo.maxFlow needs flow capacities we do not model.
package graphalgo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Limits.
const (
	queryTimeoutMs   = 2500 // below the Go client's 3 s read timeout
	MaxRingClaims    = 200  // larger components are reported as oversized, not as a ring
	MaxMembersShown  = 50   // member claims returned in detail
	MaxEntitiesShown = 20   // shared entities returned
	MaxBackboneLinks = 120  // backbone edges returned
	DefaultMinRing   = 3
	MaxListRings     = 50
	DefaultListRings = 20
	serverResultCap  = 10000 // FalkorDB's default RESULTSET_SIZE
	ringNote         = "A ring is a group of claims connected through strong shared identifiers (claimant payout account, private device, phone/email, address, or the same claimant). Membership is investigation context, not proof of wrongdoing; households and joint accounts can form legitimate rings."
	noRingMessage    = "The claim shares no strong identifier with any other claim, so it belongs to no ring."
	oversizedMessage = "The connected component is too large to be a meaningful ring; it is reported by size only. Review its evidence with FindRelatedClaims instead."
)

var (
	// ErrClaimNotFound is returned when a claim id is not in the graph.
	ErrClaimNotFound = errors.New("claim not found")
	// ErrTimeout is returned when FalkorDB aborts an algorithm that exceeded the time limit.
	ErrTimeout = errors.New("graph algorithm timed out")
	// ErrOverlayMissing is returned when evidence relationships lack the analytics overlay
	// (labels + evidence costs) that the algorithms rely on. Answering anyway would report
	// a misleading "no ring"; the overlay is built by ingestion, seeding and API start-up.
	ErrOverlayMissing = errors.New("the graph's analytics overlay is missing or stale; start the API or run the seed once to build it")
)

const (
	cypherOverlayMarker = `MATCH (m:GraphMeta {key:'analytics_overlay'}) RETURN m.version AS v`

	// cypherOverlayGap finds one evidence relationship without an evidence cost. One branch
	// per type: a multi-type pattern ([:A|B|...]) is far slower in FalkorDB.
	cypherOverlayGap = `
		MATCH ()-[r:PAID_TO]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:USED_DEVICE]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:FILED_BY]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:HAS_CONTACT]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:RESIDES_AT]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:REPAIRED_AT]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1 UNION ALL
		MATCH ()-[r:FILED_FROM]->() WHERE r.w IS NULL RETURN 1 AS gap LIMIT 1`
)

// CheckOverlay returns ErrOverlayMissing when evidence relationships lack their cost. A
// graph marked by ingest.EnsureOverlay (current version) is trusted without a scan, since
// ingestion maintains the overlay from then on; other graphs are scanned.
func CheckOverlay(g *falkordb.Graph) error {
	if m, err := roQuery(g, cypherOverlayMarker, nil); err == nil && m.Next() {
		if num(get(m.Record(), "v")) >= ingest.OverlayVersion {
			return nil
		}
	}
	r, err := roQuery(g, cypherOverlayGap, nil)
	if errors.Is(err, errEmptyGraph) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Next() {
		return ErrOverlayMissing
	}
	return nil
}

const (
	// cypherRingMembers: WCC over the strong-evidence subgraph, keeping the component
	// that contains the target claim.
	cypherRingMembers = `
		CALL algo.WCC({nodeLabels: $labels, relationshipTypes: $rels}) YIELD node, componentId
		WITH node, componentId WHERE node:Claim
		WITH componentId, collect(node.claim_id) AS claims
		WHERE $id IN claims
		RETURN claims`

	// cypherClaimFacts: filing date, amount and recorded dispositions of the member claims.
	cypherClaimFacts = `
		MATCH (c:Claim) WHERE c.claim_id IN $claims
		OPTIONAL MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c)
		WITH c, collect(ic.disposition) AS dispositions
		OPTIONAL MATCH (c)-[:FILED_BY]->(k:Claimant)
		OPTIONAL MATCH (k)-[:RESIDES_AT]->(a:Address)
		OPTIONAL MATCH (c)-[:COVERED_BY]->(p:Policy)
		RETURN c.claim_id AS claim_id, c.filing_date AS filed, c.amount AS amount, dispositions,
		       k.claimant_id AS claimant, collect(DISTINCT a.address_id) AS addresses,
		       c.incident_date AS incident, p.start_date AS policy_start`

	// cypherSharedEntities: strong entities used by at least two member claims.
	cypherSharedEntities = `
		MATCH (c:Claim)-[:PAID_TO]->(e:ClaimantPayout) WHERE c.claim_id IN $claims
		WITH e, count(DISTINCT c) AS n WHERE n >= 2
		RETURN 'PayoutDestination' AS type, e.account_id AS id, e.type AS subtype, n AS claims
		UNION ALL
		MATCH (c:Claim)-[:USED_DEVICE]->(e:PrivateDevice) WHERE c.claim_id IN $claims
		WITH e, count(DISTINCT c) AS n WHERE n >= 2
		RETURN 'Device' AS type, e.device_id AS id, null AS subtype, n AS claims
		UNION ALL
		MATCH (c:Claim)-[:FILED_BY]->(:Claimant)-[:HAS_CONTACT]->(e:ContactPoint) WHERE c.claim_id IN $claims
		WITH e, count(DISTINCT c) AS n WHERE n >= 2
		RETURN 'ContactPoint' AS type, e.value AS id, e.type AS subtype, n AS claims
		UNION ALL
		MATCH (c:Claim)-[:FILED_BY]->(:Claimant)-[:RESIDES_AT]->(e:Address) WHERE c.claim_id IN $claims
		WITH e, count(DISTINCT c) AS n WHERE n >= 2
		RETURN 'Address' AS type, e.address_id AS id, null AS subtype, n AS claims
		UNION ALL
		MATCH (c:Claim)-[:FILED_BY]->(e:Claimant) WHERE c.claim_id IN $claims
		WITH e, count(DISTINCT c) AS n WHERE n >= 2
		RETURN 'Claimant' AS type, e.claimant_id AS id, null AS subtype, n AS claims`

	// cypherBackbone: the minimum spanning tree (lowest total evidence cost) of the
	// component that contains the target claim.
	cypherBackbone = `
		CALL algo.MSF({nodeLabels: $labels, relationshipTypes: $rels, weightAttribute: 'w'}) YIELD nodes, edges
		WITH nodes, edges WHERE any(n IN nodes WHERE n.claim_id = $id)
		RETURN nodes, edges`

	// cypherAllRings: WCC over the whole book, aggregated per component INSIDE FalkorDB
	// (one row per ring). Returning one row per member claim exceeded FalkorDB's default
	// 10,000-row reply cap on a 20k-claim book and silently undercounted rings.
	cypherAllRings = `
		CALL algo.WCC({nodeLabels: $labels, relationshipTypes: $rels}) YIELD node, componentId
		WITH node, componentId WHERE node:Claim
		OPTIONAL MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(node)
		WITH componentId, node, count(ic) AS cases,
		     sum(CASE WHEN ic.disposition = 'confirmed_fraud' THEN 1 ELSE 0 END) AS confirmed
		OPTIONAL MATCH (node)-[:FILED_BY]->(k:Claimant)
		OPTIONAL MATCH (k)-[:RESIDES_AT]->(a:Address)
		OPTIONAL MATCH (node)-[:COVERED_BY]->(p:Policy)
		WITH componentId, node, cases, confirmed, k.claimant_id AS kid, collect(DISTINCT a.address_id) AS addrs, p.start_date AS pstart
		WITH componentId, count(node) AS size, min(node.claim_id) AS first_id, collect(node.claim_id)[..10] AS sample,
		     min(node.filing_date) AS first_filed, max(node.filing_date) AS last_filed, sum(node.amount) AS total,
		     sum(confirmed) AS confirmed, sum(CASE WHEN cases = 0 THEN 1 ELSE 0 END) AS open,
		     collect(node.filing_date) AS filed, collect(DISTINCT kid) AS claimants, collect(addrs) AS addr_lists,
		     collect([node.incident_date, pstart]) AS tenure
		WHERE size >= $min
		RETURN first_id, size, sample, first_filed, last_filed, total, confirmed, open, filed, claimants, addr_lists, tenure`

	cypherClaimExists = `MATCH (c:Claim {claim_id:$id}) RETURN count(c) AS n`
)

// RingClaim is one member claim.
type RingClaim struct {
	ClaimID      string   `json:"claim_id"`
	FilingDate   string   `json:"filing_date,omitempty"`
	Amount       float64  `json:"amount"`
	Dispositions []string `json:"recorded_dispositions,omitempty"`
}

// RingEntity is a strong identifier shared by several member claims.
type RingEntity struct {
	Type    string `json:"entity_type"`
	ID      string `json:"entity_id"`
	Subtype string `json:"entity_subtype,omitempty"`
	Claims  int    `json:"member_claims"` // member claims that use it
}

// GraphNode identifies a node on an algorithm result.
type GraphNode struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Link is one relationship on a backbone or a strongest-link path.
type Link struct {
	From GraphNode `json:"from"`
	Rel  string    `json:"rel"`
	To   GraphNode `json:"to"`
	Cost int       `json:"evidence_cost"`
}

// Ring is the claim's strong-evidence component.
type Ring struct {
	RingID              string       `json:"ring_id,omitempty"` // "RING-" + smallest member claim id (stable)
	TargetClaim         string       `json:"target_claim"`
	Algorithm           string       `json:"algorithm"`
	Size                int          `json:"size"` // member claims, target included
	Oversized           bool         `json:"oversized"`
	Members             []RingClaim  `json:"members"`
	SharedEntities      []RingEntity `json:"shared_entities"`
	HubEntity           *RingEntity  `json:"hub_entity,omitempty"` // shared by the most member claims
	FirstFiled          string       `json:"first_filed,omitempty"`
	LastFiled           string       `json:"last_filed,omitempty"`
	SpanDays            int          `json:"filing_span_days"`
	TotalAmount         float64      `json:"total_amount"`
	ConfirmedFraudCases int          `json:"confirmed_fraud_cases"`
	OtherRecordedCases  int          `json:"other_recorded_cases"`
	Backbone            []Link       `json:"backbone"` // algo.MSF: the minimum evidence that connects every member
	BackboneCost        int          `json:"backbone_cost"`
	Truncated           bool         `json:"truncated"`
	Message             string       `json:"message,omitempty"`
	Note                string       `json:"note"`
	Cached              bool         `json:"algorithm_results_cached"` // WCC/MSF reused for this evidence version
	Traits              RingTraits   `json:"traits"`                   // indicators that separate rings from households
	DurationMs          int64        `json:"duration_ms"`
}

// RingOf returns the ring (strong-evidence connected component) that contains claimID.
func RingOf(g *falkordb.Graph, claimID string) (*Ring, error) {
	start := time.Now()
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("invalid claim id %q", claimID)
	}
	if err := requireClaim(g, claimID); err != nil {
		return nil, err
	}
	if err := CheckOverlay(g); err != nil {
		return nil, err
	}
	ring := &Ring{TargetClaim: claimID, Algorithm: "algo.WCC + algo.MSF", Members: []RingClaim{},
		SharedEntities: []RingEntity{}, Backbone: []Link{}, Note: ringNote}

	sc, err := strongSchema(g)
	if err != nil {
		return nil, err
	}
	var members []string
	cache := cacheFor(g)
	if sc.usable() { // no strong relationship exists yet: every claim is on its own
		cachedOK := false
		if cache != nil {
			if members, cachedOK, err = cache.cachedMembers(g, sc, claimID); err != nil {
				return nil, err
			}
			ring.Cached = cachedOK
		}
		if !cachedOK {
			r, err := roQuery(g, cypherRingMembers, sc.params(map[string]any{"id": claimID}))
			if err != nil {
				return nil, err
			}
			if r.Next() {
				v, _ := r.Record().Get("claims")
				members = strings_(v)
			}
		}
	}
	if len(members) == 0 {
		members = []string{claimID} // not in the strong subgraph at all
	}
	sort.Strings(members)
	ring.Size = len(members)
	if ring.Size == 1 {
		ring.Message = noRingMessage
		ring.Members = []RingClaim{{ClaimID: claimID}}
		ring.DurationMs = time.Since(start).Milliseconds()
		return ring, nil
	}
	ring.RingID = "RING-" + members[0]
	if ring.Size > MaxRingClaims {
		ring.Oversized, ring.Truncated, ring.Message = true, true, oversizedMessage
		members = keepTarget(members, claimID, MaxMembersShown)
	}

	if err := ring.loadFacts(g, members); err != nil {
		return nil, err
	}
	if !ring.Oversized {
		if err := ring.loadEntities(g, members); err != nil {
			return nil, err
		}
		done := false
		if cache != nil {
			links, ok, err := cache.cachedBackbone(g, sc, ring.RingID[len("RING-"):])
			if err != nil {
				return nil, err
			}
			if ok {
				ring.setBackbone(links)
				done = true
			}
		}
		if !done {
			if err := ring.loadBackbone(g, claimID, sc); err != nil {
				return nil, err
			}
		}
	}
	ring.DurationMs = time.Since(start).Milliseconds()
	return ring, nil
}

func (ring *Ring) loadFacts(g *falkordb.Graph, members []string) error {
	r, err := roQuery(g, cypherClaimFacts, map[string]any{"claims": anyList(members)})
	if err != nil {
		return err
	}
	var all []RingClaim
	var filed []string
	claimants, addresses := map[string]bool{}, map[string]bool{}
	newPolicies := 0
	for r.Next() {
		rec := r.Record()
		rc := RingClaim{ClaimID: str(get(rec, "claim_id")), FilingDate: str(get(rec, "filed")),
			Amount: num(get(rec, "amount")), Dispositions: strings_(get(rec, "dispositions"))}
		filed = append(filed, rc.FilingDate)
		if k := str(get(rec, "claimant")); k != "" {
			claimants[k] = true
		}
		for _, a := range strings_(get(rec, "addresses")) {
			addresses[a] = true
		}
		if newPolicy(str(get(rec, "incident")), str(get(rec, "policy_start"))) {
			newPolicies++
		}
		sort.Strings(rc.Dispositions)
		for _, d := range rc.Dispositions {
			if d == "confirmed_fraud" {
				ring.ConfirmedFraudCases++
			} else {
				ring.OtherRecordedCases++
			}
		}
		ring.TotalAmount += rc.Amount
		if rc.FilingDate != "" && (ring.FirstFiled == "" || rc.FilingDate < ring.FirstFiled) {
			ring.FirstFiled = rc.FilingDate
		}
		if rc.FilingDate > ring.LastFiled {
			ring.LastFiled = rc.FilingDate
		}
		all = append(all, rc)
	}
	ring.SpanDays = daysBetween(ring.FirstFiled, ring.LastFiled)
	ring.Traits = traitsOf(len(all), filed, claimants, addresses, newPolicies, ring.ConfirmedFraudCases)
	sort.Slice(all, func(i, j int) bool { return all[i].ClaimID < all[j].ClaimID })
	if len(all) > MaxMembersShown {
		all = keepTargetClaims(all, ring.TargetClaim, MaxMembersShown)
		ring.Truncated = true
	}
	ring.Members = all
	return nil
}

func (ring *Ring) loadEntities(g *falkordb.Graph, members []string) error {
	r, err := roQuery(g, cypherSharedEntities, map[string]any{"claims": anyList(members)})
	if err != nil {
		return err
	}
	for r.Next() {
		rec := r.Record()
		ring.SharedEntities = append(ring.SharedEntities, RingEntity{Type: str(get(rec, "type")), ID: str(get(rec, "id")),
			Subtype: str(get(rec, "subtype")), Claims: int(num(get(rec, "claims")))})
	}
	sort.Slice(ring.SharedEntities, func(i, j int) bool {
		a, b := ring.SharedEntities[i], ring.SharedEntities[j]
		if a.Claims != b.Claims {
			return a.Claims > b.Claims
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})
	if len(ring.SharedEntities) > 0 {
		hub := ring.SharedEntities[0]
		ring.HubEntity = &hub
	}
	if len(ring.SharedEntities) > MaxEntitiesShown {
		ring.SharedEntities = ring.SharedEntities[:MaxEntitiesShown]
		ring.Truncated = true
	}
	return nil
}

func (ring *Ring) loadBackbone(g *falkordb.Graph, claimID string, sc schema) error {
	r, err := roQuery(g, cypherBackbone, sc.params(map[string]any{"id": claimID}))
	if err != nil {
		return err
	}
	if !r.Next() {
		return nil
	}
	rec := r.Record()
	nodes, _ := get(rec, "nodes").([]interface{})
	edges, _ := get(rec, "edges").([]interface{})
	links, _ := treeLinks(nodes, edges)
	ring.setBackbone(links)
	return nil
}

func (ring *Ring) setBackbone(links []Link) {
	ring.Backbone, ring.BackboneCost = []Link{}, 0
	for _, l := range links {
		ring.BackboneCost += l.Cost
	}
	ring.Backbone = append(ring.Backbone, links...)
	if len(ring.Backbone) > MaxBackboneLinks {
		ring.Backbone = ring.Backbone[:MaxBackboneLinks]
		ring.Truncated = true
	}
}

// RingSummary is one ring in the book-wide list.
type RingSummary struct {
	RingID              string   `json:"ring_id"`
	Size                int      `json:"size"`
	Claims              []string `json:"claims"` // up to 10 members, sorted
	FirstFiled          string   `json:"first_filed,omitempty"`
	LastFiled           string   `json:"last_filed,omitempty"`
	SpanDays            int      `json:"filing_span_days"`
	TotalAmount         float64  `json:"total_amount"`
	ConfirmedFraudCases int      `json:"confirmed_fraud_cases"`
	OpenClaims          int      `json:"claims_without_recorded_outcome"`
	Oversized           bool     `json:"oversized"`
	RingTraits
}

// RingList is the book-wide ring overview.
type RingList struct {
	Algorithm  string        `json:"algorithm"`
	MinSize    int           `json:"min_size"`
	TotalRings int           `json:"total_rings"`
	Truncated  bool          `json:"truncated"` // the server's reply cap was reached; rings may be missing
	Rings      []RingSummary `json:"rings"`
	Ordering   string        `json:"ordering"`
	Note       string        `json:"note"`
	DurationMs int64         `json:"duration_ms"`
}

// ListRings runs WCC over the whole book and returns rings with at least minSize claims,
// ordered for review: rings containing a human-confirmed case first, then larger rings,
// then rings filed over a shorter period, then by ring id. This is queue ordering, not a score.
func ListRings(g *falkordb.Graph, minSize, limit int) (*RingList, error) {
	start := time.Now()
	if minSize == 0 {
		minSize = DefaultMinRing
	}
	if limit == 0 {
		limit = DefaultListRings
	}
	if minSize < 2 || minSize > MaxRingClaims {
		return nil, fmt.Errorf("min_size must be between 2 and %d", MaxRingClaims)
	}
	if limit < 1 || limit > MaxListRings {
		return nil, fmt.Errorf("limit must be between 1 and %d", MaxListRings)
	}
	if err := CheckOverlay(g); err != nil {
		return nil, err
	}
	empty := &RingList{Algorithm: "algo.WCC", MinSize: minSize, Rings: []RingSummary{}, Note: ringNote}
	sc, err := strongSchema(g)
	if errors.Is(err, errEmptyGraph) {
		return empty, nil
	}
	if err != nil {
		return nil, err
	}
	if !sc.usable() {
		return empty, nil
	}
	r, err := roQuery(g, cypherAllRings, sc.params(map[string]any{"min": minSize}))
	if err != nil {
		return nil, err
	}
	out := &RingList{Algorithm: "algo.WCC", MinSize: minSize, Rings: []RingSummary{}, Note: ringNote,
		Ordering: indicatorsOrderNote}
	rows := 0
	for r.Next() {
		rows++
		rec := r.Record()
		s := RingSummary{RingID: "RING-" + str(get(rec, "first_id")), Size: int(num(get(rec, "size"))),
			Claims: strings_(get(rec, "sample")), FirstFiled: str(get(rec, "first_filed")), LastFiled: str(get(rec, "last_filed")),
			TotalAmount: num(get(rec, "total")), ConfirmedFraudCases: int(num(get(rec, "confirmed"))), OpenClaims: int(num(get(rec, "open")))}
		sort.Strings(s.Claims)
		s.Oversized = s.Size > MaxRingClaims
		claimants, addresses := map[string]bool{}, map[string]bool{}
		for _, k := range strings_(get(rec, "claimants")) {
			claimants[k] = true
		}
		lists, _ := get(rec, "addr_lists").([]interface{})
		for _, l := range lists {
			for _, a := range strings_(l) {
				addresses[a] = true
			}
		}
		newPolicies := 0
		tenure, _ := get(rec, "tenure").([]interface{})
		for _, t := range tenure {
			if pair := strings_(t); len(pair) == 2 && newPolicy(pair[0], pair[1]) {
				newPolicies++
			}
		}
		s.RingTraits = traitsOf(s.Size, strings_(get(rec, "filed")), claimants, addresses, newPolicies, s.ConfirmedFraudCases)
		s.SpanDays = daysBetween(s.FirstFiled, s.LastFiled)
		out.Rings = append(out.Rings, s)
	}
	out.Truncated = rows >= serverResultCap
	sort.Slice(out.Rings, func(i, j int) bool { return lessRing(out.Rings[i], out.Rings[j]) })
	out.TotalRings = len(out.Rings)
	if len(out.Rings) > limit {
		out.Rings = out.Rings[:limit]
	}
	out.DurationMs = time.Since(start).Milliseconds()
	return out, nil
}

// ---- helpers ----

// The strong-evidence subgraph, and the relationships a strongest-link search may use.
var (
	strongLabels = []string{"Claim", "Claimant", ingest.LabelClaimantPayout, ingest.LabelPrivateDevice, "ContactPoint", "Address"}
	strongRels   = []string{"PAID_TO", "USED_DEVICE", "FILED_BY", "HAS_CONTACT", "RESIDES_AT"}
	// The strongest-link search uses the same strong evidence as the rings. Repair shops and
	// public IPs are left out: on a 20k-claim book, letting Dijkstra route through a shop used
	// by 1,000+ claims made long chains take 1.6-2.5 s (and time out); strong-only takes ms.
	linkRels = strongRels
)

// schema is the subset of an allowlist that exists in this graph. FalkorDB's algorithm
// procedures reject unknown labels/types, and treat an EMPTY list as "everything", so
// callers must check usable() before running an algorithm.
type schema struct{ labels, rels []any }

func (s schema) usable() bool { return len(s.rels) > 0 && len(s.labels) > 0 }

func (s schema) params(p map[string]any) map[string]any {
	p["labels"], p["rels"] = s.labels, s.rels
	return p
}

func strongSchema(g *falkordb.Graph) (schema, error) {
	return presentSchema(g, strongLabels, strongRels)
}

func presentSchema(g *falkordb.Graph, labels, rels []string) (schema, error) {
	have := func(q, col string) (map[string]bool, error) {
		r, err := roQuery(g, q, nil)
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for r.Next() {
			m[str(get(r.Record(), col))] = true
		}
		return m, nil
	}
	hl, err := have(`CALL db.labels() YIELD label RETURN label`, "label")
	if err != nil {
		return schema{}, err
	}
	hr, err := have(`CALL db.relationshipTypes() YIELD relationshipType RETURN relationshipType`, "relationshipType")
	if err != nil {
		return schema{}, err
	}
	var s schema
	for _, l := range labels {
		if hl[l] {
			s.labels = append(s.labels, l)
		}
	}
	for _, t := range rels {
		if hr[t] {
			s.rels = append(s.rels, t)
		}
	}
	return s, nil
}

var errEmptyGraph = errors.New("empty graph")

func roQuery(g *falkordb.Graph, q string, p map[string]any) (*falkordb.QueryResult, error) {
	r, err := g.ROQuery(q, p, falkordb.NewQueryOptions().SetTimeout(queryTimeoutMs))
	if err != nil {
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "timed out"):
			return nil, ErrTimeout
		case strings.Contains(msg, "empty key"):
			return nil, errEmptyGraph
		}
		return nil, fmt.Errorf("graph algorithm: %w", err)
	}
	return r, nil
}

func requireClaim(g *falkordb.Graph, id string) error {
	r, err := roQuery(g, cypherClaimExists, map[string]any{"id": id})
	if errors.Is(err, errEmptyGraph) {
		return ErrClaimNotFound
	}
	if err != nil {
		return err
	}
	if !r.Next() || num(get(r.Record(), "n")) == 0 {
		return ErrClaimNotFound
	}
	return nil
}

func get(rec *falkordb.Record, k string) any { v, _ := rec.Get(k); return v }

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func strings_(v any) []string {
	list, _ := v.([]interface{})
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// keepTarget returns the first n ids, always including the target.
func keepTarget(ids []string, target string, n int) []string {
	out := []string{target}
	for _, id := range ids {
		if len(out) == n {
			break
		}
		if id != target {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func keepTargetClaims(cs []RingClaim, target string, n int) []RingClaim {
	var out []RingClaim
	for _, c := range cs {
		if c.ClaimID == target {
			out = append(out, c)
		}
	}
	for _, c := range cs {
		if len(out) == n {
			break
		}
		if c.ClaimID != target {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClaimID < out[j].ClaimID })
	return out
}

// nodeOf names a node by its type and stable synthetic identifier.
func nodeOf(n *falkordb.Node) GraphNode {
	p := n.Properties
	typ := ""
	for _, l := range n.Labels { // skip overlay labels: report the domain type
		if l != ingest.LabelClaimantPayout && l != ingest.LabelPrivateDevice {
			typ = l
			break
		}
	}
	id := ""
	for _, k := range []string{"claim_id", "claimant_id", "account_id", "device_id", "value", "address_id", "shop_id", "ip"} {
		if s := str(p[k]); s != "" {
			id = s
			break
		}
	}
	return GraphNode{Type: typ, ID: id}
}

func linkKey(l Link) string {
	return l.From.Type + ":" + l.From.ID + "|" + l.Rel + "|" + l.To.Type + ":" + l.To.ID
}

func daysBetween(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(tb.Sub(ta).Hours() / 24)
}
