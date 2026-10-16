package ingest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Evidence version marker.
//
// FalkorDB's graph algorithms (WCC, MSF) always process the whole projected graph, so
// their results are cached by readers (internal/graphalgo) and reused until the evidence
// changes. Every write path that changes evidence relationships or the analytics overlay
// (Ingest, EnsureOverlay, the bulk loader via EnsureOverlay) bumps this marker. The epoch is
// random per graph, so a graph that is deleted and recreated under the same name never
// matches an old cache entry even if its version counter repeats.
const cypherBumpEvidenceVersion = `
	MERGE (m:GraphMeta {key:'evidence_version'})
	  ON CREATE SET m.epoch = $epoch, m.version = 0
	SET m.version = m.version + 1`

func bumpEvidenceVersion(g *falkordb.Graph) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	if _, err := g.Query(cypherBumpEvidenceVersion, map[string]any{"epoch": hex.EncodeToString(b)}, nil); err != nil {
		return fmt.Errorf("bump evidence version: %w", err)
	}
	return nil
}

// cypherRenewEvidenceEpoch gives the marker a new random epoch. A graph restored from a
// snapshot carries the snapshot's epoch and counter; without a new epoch the next write
// would repeat a version a reader may already have cached for different (pre-reset) data.
const cypherRenewEvidenceEpoch = `
	MERGE (m:GraphMeta {key:'evidence_version'})
	SET m.epoch = $epoch, m.version = coalesce(m.version, 0) + 1`

// RenewEvidenceEpoch marks the graph's evidence as new, so no cached algorithm result is reused.
func RenewEvidenceEpoch(g *falkordb.Graph) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	if _, err := g.Query(cypherRenewEvidenceEpoch, map[string]any{"epoch": hex.EncodeToString(b)}, nil); err != nil {
		return fmt.Errorf("renew evidence epoch: %w", err)
	}
	return nil
}
