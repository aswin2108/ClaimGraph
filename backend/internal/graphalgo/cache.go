package graphalgo

// Cache of whole-graph algorithm results.
//
// algo.WCC and algo.MSF have no "start from this node" mode: every call processes the
// whole strong-evidence projection (~0.1 s and ~0.2 s on a 20k-claim book). Their
// results only change when evidence changes, and every evidence write path bumps the
// GraphMeta evidence_version marker (see ingest/version.go). So the components and the
// spanning forest are computed once per (graph, epoch, version) and reused; a ring lookup
// then only runs small indexed queries for live facts (amounts, recorded outcomes).
//
// Graphs without the marker are never cached. Edits made outside ClaimGraph's write paths
// (e.g. by hand in FalkorDB Browser) do not bump the version; restart the reader or run
// ingest.EnsureOverlay (which bumps it) after such edits.

import (
	"sort"
	"sync"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	cypherEvidenceVersion = `MATCH (m:GraphMeta {key:'evidence_version'}) RETURN m.epoch AS epoch, m.version AS version`

	// All components with at least two claims, one row each (singletons are implied).
	cypherAllComponents = `
		CALL algo.WCC({nodeLabels: $labels, relationshipTypes: $rels}) YIELD node, componentId
		WITH node, componentId WHERE node:Claim
		WITH componentId, collect(node.claim_id) AS claims
		WHERE size(claims) >= 2
		RETURN claims`

	// The whole minimum spanning forest, one row per tree that spans two or more claims.
	cypherAllBackbones = `
		CALL algo.MSF({nodeLabels: $labels, relationshipTypes: $rels, weightAttribute: 'w'}) YIELD nodes, edges
		WITH nodes, edges WHERE size([n IN nodes WHERE n:Claim]) >= 2
		RETURN nodes, edges`
)

type versionKey struct {
	graph, epoch string
	version      int64
}

type algoCache struct {
	key versionKey
	mu  sync.Mutex

	membersLoaded bool
	membersOK     bool                // false when the reply hit the server's row cap
	members       map[string][]string // claim -> sorted members of its component (>= 2 claims)

	backboneLoaded bool
	backboneOK     bool
	backbone       map[string][]Link // smallest member claim id -> spanning-tree links
}

var (
	cachesMu sync.Mutex
	caches   = map[string]*algoCache{} // by graph name; one entry (the current version) per graph
)

// cacheFor returns the cache entry for the graph's current evidence version, or nil when
// the graph has no version marker.
func cacheFor(g *falkordb.Graph) *algoCache {
	r, err := roQuery(g, cypherEvidenceVersion, nil)
	if err != nil || !r.Next() {
		return nil
	}
	key := versionKey{graph: g.Id, epoch: str(get(r.Record(), "epoch")), version: int64(num(get(r.Record(), "version")))}
	if key.epoch == "" {
		return nil
	}
	cachesMu.Lock()
	defer cachesMu.Unlock()
	if c := caches[g.Id]; c != nil && c.key == key {
		return c
	}
	c := &algoCache{key: key}
	caches[g.Id] = c
	return c
}

// cachedMembers returns the claim's component members from the cache, loading every
// component with one WCC run if needed. ok is false when the cache cannot answer.
func (c *algoCache) cachedMembers(g *falkordb.Graph, sc schema, claimID string) (members []string, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.membersLoaded {
		r, err := roQuery(g, cypherAllComponents, sc.params(map[string]any{}))
		if err != nil {
			return nil, false, err
		}
		c.members, c.membersLoaded = map[string][]string{}, true
		rows := 0
		for r.Next() {
			rows++
			comp := strings_(get(r.Record(), "claims"))
			sort.Strings(comp)
			for _, id := range comp {
				c.members[id] = comp
			}
		}
		c.membersOK = rows < serverResultCap
	}
	if !c.membersOK {
		return nil, false, nil
	}
	if m, found := c.members[claimID]; found {
		return m, true, nil
	}
	return []string{claimID}, true, nil // a singleton: no strong link to any other claim
}

// cachedBackbone returns the spanning-tree links of the component whose smallest member
// claim id is firstClaim, loading the whole forest with one MSF run if needed.
func (c *algoCache) cachedBackbone(g *falkordb.Graph, sc schema, firstClaim string) (links []Link, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.backboneLoaded {
		r, err := roQuery(g, cypherAllBackbones, sc.params(map[string]any{}))
		if err != nil {
			return nil, false, err
		}
		c.backbone, c.backboneLoaded = map[string][]Link{}, true
		rows := 0
		for r.Next() {
			rows++
			tree, first := treeLinks(get(r.Record(), "nodes"), get(r.Record(), "edges"))
			if first != "" {
				c.backbone[first] = tree
			}
		}
		c.backboneOK = rows < serverResultCap
	}
	if !c.backboneOK {
		return nil, false, nil
	}
	return c.backbone[firstClaim], true, nil
}

// treeLinks converts one MSF tree into links and returns its smallest claim id.
func treeLinks(nodesVal, edgesVal any) ([]Link, string) {
	nodes, _ := nodesVal.([]interface{})
	edges, _ := edgesVal.([]interface{})
	byID := map[uint64]GraphNode{}
	first := ""
	for _, v := range nodes {
		n, ok := v.(*falkordb.Node)
		if !ok {
			continue
		}
		gn := nodeOf(n)
		byID[n.ID] = gn
		if gn.Type == "Claim" && (first == "" || gn.ID < first) {
			first = gn.ID
		}
	}
	var links []Link
	for _, v := range edges {
		e, ok := v.(*falkordb.Edge)
		if !ok {
			continue
		}
		links = append(links, Link{From: byID[e.SourceNodeID()], Rel: e.Relation, To: byID[e.DestNodeID()],
			Cost: int(num(e.Properties[ingest.CostProperty]))})
	}
	sort.Slice(links, func(i, j int) bool { return linkKey(links[i]) < linkKey(links[j]) })
	return links, first
}
