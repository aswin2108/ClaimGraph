package graphalgo

import (
	"errors"
	"time"

	"github.com/FalkorDB/falkordb-go/v2"
)

// GraphStats describes the size and readiness of the graph (for the UI badge and the README).
type GraphStats struct {
	Nodes            int64            `json:"nodes"`
	Relationships    int64            `json:"relationships"`
	Labels           map[string]int64 `json:"labels"`
	RelationshipType map[string]int64 `json:"relationship_types"`
	EvidenceVersion  int64            `json:"evidence_version"`
	OverlayReady     bool             `json:"analytics_overlay_ready"`
	DurationMs       int64            `json:"duration_ms"`
}

// Stats reads FalkorDB's own counters (db.meta.stats) plus the evidence version marker.
func Stats(g *falkordb.Graph) (*GraphStats, error) {
	start := time.Now()
	out := &GraphStats{Labels: map[string]int64{}, RelationshipType: map[string]int64{}}
	r, err := roQuery(g, `CALL db.meta.stats() YIELD labels, relTypes, nodeCount, relCount RETURN labels, relTypes, nodeCount, relCount`, nil)
	if errors.Is(err, errEmptyGraph) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Next() {
		rec := r.Record()
		out.Nodes, out.Relationships = int64(num(get(rec, "nodeCount"))), int64(num(get(rec, "relCount")))
		toCounts := func(v any, dst map[string]int64) {
			if m, ok := v.(map[string]interface{}); ok {
				for k, c := range m {
					dst[k] = int64(num(c))
				}
			}
		}
		toCounts(get(rec, "labels"), out.Labels)
		toCounts(get(rec, "relTypes"), out.RelationshipType)
	}
	if v, err := roQuery(g, cypherEvidenceVersion, nil); err == nil && v.Next() {
		out.EvidenceVersion = int64(num(get(v.Record(), "version")))
	}
	out.OverlayReady = CheckOverlay(g) == nil
	out.DurationMs = time.Since(start).Milliseconds()
	return out, nil
}
