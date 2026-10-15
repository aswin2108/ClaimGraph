package investigate

// In-database multi-hop traversal.
//
// FalkorDB walks the graph and returns complete evidence paths (claim, entity, claim,
// ...) in one round trip per depth band; Go only parses the paths, applies the
// counterfactual exclusion, keeps the shortest paths and enforces result limits.
//
// A "claim-to-claim transition" is one of two shapes, and a path of up to MaxHops
// transitions is any sequence of them:
//
//	claim-level    (x)-[PAID_TO|USED_DEVICE|REPAIRED_AT|FILED_FROM]->(entity)<-[same]-(y)
//	claimant-level (x)-[FILED_BY]->(k1)-[HAS_CONTACT|RESIDES_AT]->(entity)<-[same]-(k2)<-[FILED_BY]-(y),  k1 <> k2
//
// Why fixed-shape patterns instead of one undirected variable-length pattern such as
// (a)-[*..12]-(b) over every relationship type: an unconstrained variable-length walk may
// pass claim -> claimant -> another claim of the SAME claimant (not shared-entity
// evidence), or hop claimant -> contact -> claimant -> address -> claimant without a
// claim in between. Those are not valid transitions and variable-length syntax cannot
// forbid them. Enumerating the 2+4+8 legal shapes (all built from the constants below,
// never from user input) keeps every returned path valid, lets FalkorDB use its
// matrix-based traversal for each fixed pattern, and bounds the work per shape.
//
// shortestPath/allShortestPaths needs both endpoints bound, so it cannot discover
// related claims, but the same shapes with BOTH ends bound answer "how is claim A
// connected to claim B" (EvidencePaths), searched level by level so the shortest
// paths are found first.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	queryTimeoutMs = 2500 // per-query FalkorDB timeout, kept below the Go client's 3s read timeout so a runaway traversal fails cleanly
	directRowLimit = 1000 // rows per relationship type for single-transition paths
	deepRowLimit   = 750  // rows per path shape for 2..3-transition paths (12 shapes x 751 < serverResultCap)
	// serverResultCap is FalkorDB's default RESULTSET_SIZE: a reply is silently cut at this
	// many rows. The row limits keep every query below it; a reply that reaches it anyway
	// (a server configured lower) is treated as truncated.
	serverResultCap = 10000
	maxExpandFanout = 100 // entities shared by more claims than this are not expanded beyond direct evidence
	samplesPerClaim = 50  // sample paths per reached claim per shape in aggregated traversal

	noConnectionMessage = "No supporting connection to other claims was found in the graph."
)

// ErrTraversalTimeout is returned when FalkorDB aborts a traversal that exceeded the time limit.
var ErrTraversalTimeout = errors.New("graph traversal timed out")

// relSets are the relationship types a traversal may follow. Excluding a category in a
// counterfactual removes its relationship type from the template, so FalkorDB never
// expands it (this matters for busy repair shops).
type relSets struct{ claim, claimant []string }

func relSetsFor(exclude string) relSets {
	rs := relSets{
		claim:    []string{"PAID_TO", "USED_DEVICE", "REPAIRED_AT", "FILED_FROM"},
		claimant: []string{"HAS_CONTACT", "RESIDES_AT"},
	}
	drop := func(list []string, t string) []string {
		out := make([]string, 0, len(list))
		for _, x := range list {
			if x != t {
				out = append(out, x)
			}
		}
		return out
	}
	switch exclude {
	case CatRepairShop:
		rs.claim = drop(rs.claim, "REPAIRED_AT")
	case CatContact:
		rs.claimant = drop(rs.claimant, "HAS_CONTACT")
	case CatAddress:
		rs.claimant = drop(rs.claimant, "RESIDES_AT")
	}
	return rs
}

// shapesOfLength returns every sequence of n transitions; true = claimant-level.
func shapesOfLength(n int) [][]bool {
	if n == 0 {
		return [][]bool{{}}
	}
	var out [][]bool
	for _, rest := range shapesOfLength(n - 1) {
		out = append(out, append([]bool{false}, rest...), append([]bool{true}, rest...))
	}
	return out
}

// shapeQuery renders one branch for a transition sequence as one MATCH clause per
// transition, each with its own WHERE. Staging matters: FalkorDB applies a WHERE as soon
// as its MATCH is done, so the fan-out guard and the distinct-claim checks prune a hub
// BEFORE the next transition expands it (a single long pattern would enumerate every
// combination first). With pair set, the last claim is bound to $rel as well as the first
// to $id. The branch returns the path as parallel node and edge lists.
func shapeQuery(kinds []bool, rs relSets, pair bool, branch int, aggregate bool) string {
	claimRels := typeList(rs.claim)
	claimantRels := typeList(rs.claimant)
	guard := len(kinds) >= 2
	nodes := []string{"x0"}
	vars := []string{"x0"} // every variable bound so far, carried through each WITH
	var edges, stages []string
	for i, claimant := range kinds {
		from, to := fmt.Sprintf("x%d", i), fmt.Sprintf("x%d", i+1)
		bound := ""
		if i == len(kinds)-1 && pair {
			bound = " {claim_id:$rel}"
		}
		start := "(" + from + ")"
		if i == 0 {
			start = "(x0:Claim {claim_id:$id})"
		}
		// First half of the transition: reach the shared entity from the claim already on the path.
		var reach string
		// Relationship types are matched with an UNTYPED pattern and a type() filter: FalkorDB
		// builds a union matrix for every multi-type pattern ([:A|B|C|D]), which measured ~140 ms
		// per traversal step on a 20k-claim book, versus ~8 ms for the untyped adjacency + filter.
		if claimant {
			reach = fmt.Sprintf("MATCH %s-[f%da:FILED_BY]->(k%da:Claimant)-[r%da]->(e%d) WHERE type(r%da) IN %s", start, i, i, i, i, i, claimantRels)
		} else {
			reach = fmt.Sprintf("MATCH %s-[r%da]->(e%d) WHERE type(r%da) IN %s", start, i, i, i, claimRels)
		}
		if guard {
			// Fan-out guard, checked once per entity BEFORE expanding it to its other claims:
			// an entity with more than maxExpandFanout evidence links (a busy repair shop, a
			// carrier IP) is still reported as direct evidence, but is not expanded further.
			// indegree() is O(1); a pattern comprehension counting edges cost ~25 ms per hub.
			// Infrastructure (a weak/context identifier used by more than $infra claims; the
			// overlay cost w > CostAddress marks weak/context evidence) is never expanded.
			reach += fmt.Sprintf(" AND indegree(e%d) <= $fanout AND (indegree(e%d) <= $infra OR coalesce(r%da.w, %d) <= %d)",
				i, i, i, ingest.CostContext, ingest.CostAddress)
		}
		// Second half: from the entity to the next claim, which must be a new claim.
		var where []string
		for j := 0; j <= i; j++ {
			where = append(where, fmt.Sprintf("ID(%s) <> ID(x%d)", to, j))
		}
		var cross string
		if claimant {
			vars = append(vars, fmt.Sprintf("f%da", i), fmt.Sprintf("k%da", i), fmt.Sprintf("r%da", i), fmt.Sprintf("e%d", i))
			cross = fmt.Sprintf("MATCH (e%d)<-[r%db]-(k%db:Claimant)<-[f%db:FILED_BY]-(%s:Claim%s)", i, i, i, i, to, bound)
			where = append(where, fmt.Sprintf("type(r%db) = type(r%da)", i, i), fmt.Sprintf("ID(k%da) <> ID(k%db)", i, i))
			nodes = append(nodes, fmt.Sprintf("k%da", i), fmt.Sprintf("e%d", i), fmt.Sprintf("k%db", i), to)
			edges = append(edges, fmt.Sprintf("f%da", i), fmt.Sprintf("r%da", i), fmt.Sprintf("r%db", i), fmt.Sprintf("f%db", i))
		} else {
			vars = append(vars, fmt.Sprintf("r%da", i), fmt.Sprintf("e%d", i))
			cross = fmt.Sprintf("MATCH (e%d)<-[r%db]-(%s:Claim%s)", i, i, to, bound)
			// The same person filing twice with their own account/device is not shared-entity
			// evidence (the claimant-level branch already requires different claimants).
			where = append(where, fmt.Sprintf("type(r%db) = type(r%da)", i, i),
				fmt.Sprintf("NOT (%s)-[:FILED_BY]->()<-[:FILED_BY]-(%s)", from, to))
			nodes = append(nodes, fmt.Sprintf("e%d", i), to)
			edges = append(edges, fmt.Sprintf("r%da", i), fmt.Sprintf("r%db", i))
		}
		// WITH after each half pins the evaluation order. Without it FalkorDB merges the
		// consecutive MATCH clauses into one pattern and may start from a label scan
		// (measured: all 15,670 claimants) instead of the one indexed target claim, and the
		// fan-out guard no longer prunes before the next expansion.
		stages = append(stages, reach, "WITH "+strings.Join(vars, ", "), cross+" WHERE "+strings.Join(where, " AND "))
		if claimant {
			vars = append(vars, fmt.Sprintf("r%db", i), fmt.Sprintf("k%db", i), fmt.Sprintf("f%db", i), to)
		} else {
			vars = append(vars, fmt.Sprintf("r%db", i), to)
		}
		if i < len(kinds)-1 {
			stages = append(stages, "WITH "+strings.Join(vars, ", "))
		}
	}
	degrees := make([]string, len(kinds))
	for i := range kinds {
		degrees[i] = fmt.Sprintf("indegree(e%d)", i)
	}
	if aggregate {
		// One row per reached claim, aggregated inside FalkorDB: the claim set is complete
		// however many paths lead to it (no per-path row limit), with up to $per_claim
		// sample paths for evidence. Paths multiply (8 x 20 x 20 ...); claims do not.
		last := fmt.Sprintf("x%d", len(kinds))
		return fmt.Sprintf("%s\nWITH %s, [%s] AS p_nodes, [%s] AS p_edges, [%s] AS p_degrees\n"+
			"WITH %s, collect([p_nodes, p_edges, p_degrees])[..$per_claim] AS samples\nRETURN samples, %d AS branch",
			strings.Join(stages, "\n"), last, strings.Join(nodes, ", "), strings.Join(edges, ", "), strings.Join(degrees, ", "), last, branch)
	}
	return fmt.Sprintf("%s\nRETURN [%s] AS nodes, [%s] AS edges, [%s] AS degrees, %d AS branch LIMIT $limit",
		strings.Join(stages, "\n"), strings.Join(nodes, ", "), strings.Join(edges, ", "), strings.Join(degrees, ", "), branch)
}

// typeList renders constant relationship type names as a Cypher list literal.
func typeList(types []string) string {
	q := make([]string, len(types))
	for i, t := range types {
		q[i] = "'" + t + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// directQuery is one UNION ALL branch per single relationship type, so a busy repair shop
// can never crowd the payout/device/contact/address signals out of the row limit.
func directQuery(rs relSets) string {
	var parts []string
	for i, t := range rs.claim {
		// Infrastructure is summarized by infrastructure(), not expanded into its users.
		parts = append(parts, fmt.Sprintf(
			"MATCH (x0:Claim {claim_id:$id})-[a:%s]->(e0) WHERE NOT (indegree(e0) > $infra AND coalesce(a.w, %d) > %d) "+
				"WITH x0, a, e0 MATCH (e0)<-[b:%s]-(x1:Claim) WHERE ID(x0) <> ID(x1) AND NOT (x0)-[:FILED_BY]->()<-[:FILED_BY]-(x1) "+
				"RETURN [x0, e0, x1] AS nodes, [a, b] AS edges, [indegree(e0)] AS degrees, %d AS branch LIMIT $limit",
			t, ingest.CostContext, ingest.CostAddress, t, i))
	}
	for i, t := range rs.claimant {
		parts = append(parts, fmt.Sprintf(
			"MATCH (x0:Claim {claim_id:$id})-[f0:FILED_BY]->(k0a:Claimant)-[a:%s]->(e0)<-[b:%s]-(k0b:Claimant)<-[f1:FILED_BY]-(x1:Claim) "+
				"WHERE ID(x0) <> ID(x1) AND ID(k0a) <> ID(k0b) "+
				"RETURN [x0, k0a, e0, k0b, x1] AS nodes, [f0, a, b, f1] AS edges, [indegree(e0)] AS degrees, %d AS branch LIMIT $limit", t, t, len(rs.claim)+i))
	}
	return strings.Join(parts, "\nUNION ALL\n")
}

// deepQuery covers every path of exactly the given transition counts (2 or 3). With
// aggregate, each branch returns one row per reached claim instead of one row per path.
func deepQuery(rs relSets, minLen, maxLen int, pair, aggregate bool) string {
	var parts []string
	n := 0
	for l := minLen; l <= maxLen; l++ {
		for _, kinds := range shapesOfLength(l) {
			parts = append(parts, shapeQuery(kinds, rs, pair, n, aggregate))
			n++
		}
	}
	return strings.Join(parts, "\nUNION ALL\n")
}

// run executes a read-only traversal with a time limit and returns the parsed paths.
// limit is the per-branch row limit; cut is true when any branch returned more rows
// than that (results may be incomplete).
func run(g *falkordb.Graph, query string, params map[string]any, limit int) (chains [][]Step, info map[string]claimInfo, cut bool, err error) {
	params["limit"] = limit + 1 // one extra row per branch reveals truncation
	params["fanout"] = maxExpandFanout
	params["infra"] = InfrastructureMinClaims
	params["per_claim"] = samplesPerClaim
	r, err := g.ROQuery(query, params, falkordb.NewQueryOptions().SetTimeout(queryTimeoutMs))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "timed out") {
			return nil, nil, false, ErrTraversalTimeout
		}
		return nil, nil, false, fmt.Errorf("graph traversal: %w", err)
	}
	info = map[string]claimInfo{}
	perBranch := map[int64]int{}
	rows := 0
	add := func(nv, ev, dv any) error {
		path, ok := pathOf(nv, ev)
		if !ok {
			return errors.New("graph traversal: unexpected path value")
		}
		degrees, _ := dv.([]interface{})
		steps, claims, ok := stepsOf(path, degrees)
		if !ok {
			return errors.New("graph traversal: malformed path")
		}
		for _, c := range claims {
			info[str(c.Properties["claim_id"])] = claimInfo{filed: str(c.Properties["filing_date"]), incident: str(c.Properties["incident_date"])}
		}
		chains = append(chains, steps)
		return nil
	}
	for r.Next() {
		rows++
		rec := r.Record()
		if sv, aggregated := rec.Get("samples"); aggregated {
			samples, _ := sv.([]interface{})
			for _, s := range samples {
				p, ok := s.([]interface{})
				if !ok || len(p) != 3 {
					return nil, nil, false, errors.New("graph traversal: malformed sample")
				}
				if err := add(p[0], p[1], p[2]); err != nil {
					return nil, nil, false, err
				}
			}
			continue
		}
		nv, _ := rec.Get("nodes")
		ev, _ := rec.Get("edges")
		dv, _ := rec.Get("degrees")
		bv, _ := rec.Get("branch")
		branch, _ := bv.(int64)
		perBranch[branch]++
		if perBranch[branch] > limit {
			cut = true
			continue
		}
		if err := add(nv, ev, dv); err != nil {
			return nil, nil, false, err
		}
	}
	if rows >= serverResultCap {
		cut = true // the server may have dropped rows
	}
	return chains, info, cut, nil
}

// pathOf assembles a path from the parallel node and edge lists a branch returns.
func pathOf(nodesVal, edgesVal any) (falkordb.Path, bool) {
	nl, ok1 := nodesVal.([]interface{})
	el, ok2 := edgesVal.([]interface{})
	if !ok1 || !ok2 || len(nl) == 0 {
		return falkordb.Path{}, false
	}
	nodes := make([]*falkordb.Node, len(nl))
	for i, v := range nl {
		n, ok := v.(*falkordb.Node)
		if !ok {
			return falkordb.Path{}, false
		}
		nodes[i] = n
	}
	edges := make([]*falkordb.Edge, len(el))
	for i, v := range el {
		e, ok := v.(*falkordb.Edge)
		if !ok {
			return falkordb.Path{}, false
		}
		edges[i] = e
	}
	return falkordb.Path{Nodes: nodes, Edges: edges}, true
}

// stepsOf converts a FalkorDB path into claim-to-claim steps. Claim-level transitions use
// 3 nodes/2 edges; claimant-level transitions use 5 nodes/4 edges.
func stepsOf(p falkordb.Path, degrees []interface{}) (steps []Step, claims []*falkordb.Node, ok bool) {
	n, e := p.Nodes, p.Edges
	ni, ei := 0, 0
	claims = append(claims, n[0])
	for ni < len(n)-1 {
		var from, ent, to, k1, k2 *falkordb.Node
		var r1, r2 *falkordb.Edge
		if ni+1 < len(n) && hasLabel(n[ni+1], "Claimant") {
			if ni+4 >= len(n) || ei+3 >= len(e) {
				return nil, nil, false
			}
			from, k1, ent, k2, to = n[ni], n[ni+1], n[ni+2], n[ni+3], n[ni+4]
			r1, r2 = e[ei+1], e[ei+2]
			ni, ei = ni+4, ei+4
		} else {
			if ni+2 >= len(n) || ei+1 >= len(e) {
				return nil, nil, false
			}
			from, ent, to = n[ni], n[ni+1], n[ni+2]
			r1, r2 = e[ei], e[ei+1]
			ni, ei = ni+2, ei+2
		}
		st := Step{
			FromClaim: str(from.Properties["claim_id"]), ToClaim: str(to.Properties["claim_id"]),
			FromRel: r1.Relation, ToRel: r2.Relation,
			FromEvidence: str(r1.Properties["evidence_id"]), ToEvidence: str(r2.Properties["evidence_id"]),
			FromAt: str(r1.Properties["recorded_at"]), ToAt: str(r2.Properties["recorded_at"]),
		}
		if k1 != nil {
			st.FromClaimant, st.ToClaimant = str(k1.Properties["claimant_id"]), str(k2.Properties["claimant_id"])
		}
		st.EntityType, st.EntityID, st.EntitySubtype, st.EntityPublic = entityOf(ent)
		if len(steps) < len(degrees) {
			if d, ok := degrees[len(steps)].(int64); ok {
				st.EntityClaims = int(d)
			}
		}
		steps = append(steps, st)
		claims = append(claims, to)
	}
	return steps, claims, len(steps) > 0
}

func hasLabel(n *falkordb.Node, l string) bool {
	for _, x := range n.Labels {
		if x == l {
			return true
		}
	}
	return false
}

// entityOf reads the shared entity's type, stable id, subtype and visibility.
func entityOf(n *falkordb.Node) (typ, id, subtype string, public *bool) {
	for _, l := range n.Labels { // the domain label, not an analytics-overlay label
		if l != ingest.LabelClaimantPayout && l != ingest.LabelPrivateDevice {
			typ = l
			break
		}
	}
	pr := n.Properties
	switch typ {
	case "PayoutDestination":
		id, subtype = str(pr["account_id"]), str(pr["type"])
	case "Device":
		id = str(pr["device_id"])
	case "RepairShop":
		id = str(pr["shop_id"])
	case "IPAddress":
		id = str(pr["ip"])
	case "ContactPoint":
		id, subtype = str(pr["value"]), str(pr["type"])
	case "Address":
		id = str(pr["address_id"])
	}
	if b, isBool := pr["is_public"].(bool); isBool && (typ == "Device" || typ == "IPAddress") {
		public = &b
	}
	return typ, id, subtype, public
}

// discover returns every path of 1..maxHops transitions that starts at claimID.
func discover(g *falkordb.Graph, claimID string, maxHops int, exclude string) (chains [][]Step, info map[string]claimInfo, cut bool, err error) {
	rs := relSetsFor(exclude)
	info = map[string]claimInfo{}
	merge := func(c [][]Step, i map[string]claimInfo, k bool) {
		chains = append(chains, c...)
		for id, v := range i {
			info[id] = v
		}
		cut = cut || k
	}
	c, i, k, err := run(g, directQuery(rs), map[string]any{"id": claimID}, directRowLimit)
	if err != nil {
		return nil, nil, false, err
	}
	merge(c, i, k)
	if maxHops >= 2 {
		// Aggregated first: complete claim set, one row per reached claim. If FalkorDB
		// cannot finish that within the time limit (an unusually dense neighbourhood), fall
		// back to row-limited path enumeration and report the result as truncated.
		c, i, k, err = run(g, deepQuery(rs, 2, maxHops, false, true), map[string]any{"id": claimID}, deepRowLimit)
		if errors.Is(err, ErrTraversalTimeout) {
			c, i, _, err = run(g, deepQuery(rs, 2, maxHops, false, false), map[string]any{"id": claimID}, deepRowLimit)
			k = true
		}
		if err != nil {
			return nil, nil, false, err
		}
		merge(c, i, k)
	}
	return chains, info, cut, nil
}

// cypherInfrastructure lists the weak/context identifiers the target shares with more than
// $infra claims, with exact usage counts (one row per identifier, aggregated in FalkorDB).
const cypherInfrastructure = `
	MATCH (x0:Claim {claim_id:$id})-[a]->(e)
	WHERE type(a) IN %s AND indegree(e) > $infra AND coalesce(a.w, %d) > %d
	RETURN e AS entity, type(a) AS rel, indegree(e) AS users, a.evidence_id AS evidence, a.recorded_at AS at`

// infrastructure summarizes the shared infrastructure the target uses.
func infrastructure(g *falkordb.Graph, claimID, exclude string) ([]InfrastructureLink, error) {
	q := fmt.Sprintf(cypherInfrastructure, typeList(relSetsFor(exclude).claim), ingest.CostContext, ingest.CostAddress)
	r, err := g.ROQuery(q, map[string]any{"id": claimID, "infra": InfrastructureMinClaims},
		falkordb.NewQueryOptions().SetTimeout(queryTimeoutMs))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "timed out") {
			return nil, ErrTraversalTimeout
		}
		return nil, fmt.Errorf("infrastructure summary: %w", err)
	}
	var out []InfrastructureLink
	for r.Next() {
		rec := r.Record()
		ev, _ := rec.Get("entity")
		n, ok := ev.(*falkordb.Node)
		if !ok {
			continue
		}
		rel, _ := rec.Get("rel")
		users, _ := rec.Get("users")
		evid, _ := rec.Get("evidence")
		at, _ := rec.Get("at")
		l := InfrastructureLink{Rel: str(rel), EvidenceID: str(evid), RecordedAt: str(at)}
		l.EntityType, l.EntityID, l.EntitySubtype, l.EntityPublic = entityOf(n)
		u, _ := users.(int64)
		l.OtherClaims = int(u) - 1
		l.Category = SignalCategory(Step{EntityType: l.EntityType, EntitySubtype: l.EntitySubtype, EntityPublic: l.EntityPublic})
		if l.Category == exclude || !WeakOrContext(l.Category) {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].EntityID < out[j].EntityID
	})
	return out, nil
}

// usesCategory reports whether any step of the chain belongs to the excluded category.
func usesCategory(chain []Step, category string) bool {
	if category == "" {
		return false
	}
	for _, s := range chain {
		if SignalCategory(s) == category {
			return true
		}
	}
	return false
}

// shortestOnly keeps, for each end claim, only the chains with the fewest transitions.
func shortestOnly(chains [][]Step, exclude string) map[string][][]Step {
	best := map[string]int{}
	for _, c := range chains {
		if usesCategory(c, exclude) {
			continue
		}
		to := c[len(c)-1].ToClaim
		if b, ok := best[to]; !ok || len(c) < b {
			best[to] = len(c)
		}
	}
	out := map[string][][]Step{}
	for _, c := range chains {
		if usesCategory(c, exclude) {
			continue
		}
		to := c[len(c)-1].ToClaim
		if len(c) == best[to] {
			out[to] = append(out[to], c)
		}
	}
	return out
}

// pathsFor sorts chains deterministically and applies the per-claim path cap
// (single-transition paths are never capped: scoring needs every signal category).
func pathsFor(chains [][]Step, hops int) (paths []Path, truncated bool) {
	sort.Slice(chains, func(i, j int) bool { return pathKey(chains[i]) < pathKey(chains[j]) })
	for _, c := range chains {
		if hops > 1 && len(paths) == MaxPathsPerClaim {
			return paths, true
		}
		paths = append(paths, Path{Hops: hops, Steps: c, Description: describe(c)})
	}
	return paths, false
}

// EvidencePaths returns the shortest evidence paths between two claims (at most maxHops
// transitions, optionally ignoring one evidence category). FalkorDB searches with both
// ends bound, one depth at a time, and stops at the first depth that connects them.
// It returns nil paths when no connection exists within the limit.
func EvidencePaths(g *falkordb.Graph, claimID, relatedID string, maxHops int, exclude string) ([]Path, error) {
	if exclude != "" && !ValidCategory(exclude) {
		return nil, fmt.Errorf("unknown evidence category %q", exclude)
	}
	if !validIDs(claimID, relatedID) {
		return nil, errors.New("invalid claim id")
	}
	if maxHops < 1 || maxHops > MaxHopsLimit {
		return nil, fmt.Errorf("max_hops must be between 1 and %d", MaxHopsLimit)
	}
	if claimID == relatedID {
		return nil, errors.New("claims must be different")
	}
	rs := relSetsFor(exclude)
	for depth := 1; depth <= maxHops; depth++ {
		chains, _, _, err := run(g, deepQuery(rs, depth, depth, true, false), map[string]any{"id": claimID, "rel": relatedID}, deepRowLimit)
		if err != nil {
			return nil, err
		}
		var kept [][]Step
		for _, c := range chains {
			if !usesCategory(c, exclude) {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			paths, _ := pathsFor(kept, depth)
			return paths, nil
		}
	}
	return nil, nil
}
