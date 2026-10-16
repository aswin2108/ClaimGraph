// Package demo saves a pristine snapshot of a demo graph and restores it on request, so
// every person who tries ClaimGraph (and every recording of the demo) starts from the same
// graph: no claims submitted by an earlier visitor, no outcomes recorded by them.
//
// Both operations are whole-graph copies inside FalkorDB (GRAPH.COPY, about 1.5 s for the
// 20,000-claim book). A restore copies the snapshot into a staging graph and then RENAMEs it
// over the live graph, which is atomic: readers see the old graph or the restored one, never
// a half-restored one. The snapshot itself is never modified by a restore.
//
// This is a demo convenience, not a data-management feature: there is no history, only one
// starting state per graph.
package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	// SnapshotSuffix names the pristine copy of a graph: claimgraph_scale -> claimgraph_scale__pristine.
	SnapshotSuffix = "__pristine"
	restoreSuffix  = "__restore"
	opTimeout      = 30 * time.Second
)

// ErrNoSnapshot is returned when a graph has no pristine snapshot to restore.
var ErrNoSnapshot = errors.New("this graph has no demo snapshot; load it with `go run ./cmd/seedscale`, which saves one")

// SnapshotName returns the snapshot graph name for a graph.
func SnapshotName(graphName string) string { return graphName + SnapshotSuffix }

// valid refuses names that are themselves snapshots or staging graphs, so a restore can
// never overwrite a snapshot with a modified graph.
func valid(graphName string) error {
	if graphName == "" || strings.HasSuffix(graphName, SnapshotSuffix) || strings.HasSuffix(graphName, restoreSuffix) {
		return fmt.Errorf("invalid demo graph name %q", graphName)
	}
	return nil
}

func exists(ctx context.Context, db *falkordb.FalkorDB, key string) (bool, error) {
	n, err := db.Conn.Exists(ctx, key).Result()
	return n == 1, err
}

// HasSnapshot reports whether graphName has a pristine snapshot.
func HasSnapshot(db *falkordb.FalkorDB, graphName string) (bool, error) {
	if err := valid(graphName); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exists(ctx, db, SnapshotName(graphName))
}

// Snapshot saves the current state of graphName as its pristine snapshot, replacing an
// older snapshot. Call it right after seeding, before anyone uses the graph.
func Snapshot(db *falkordb.FalkorDB, graphName string) error {
	if err := valid(graphName); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if ok, err := exists(ctx, db, graphName); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("graph %q does not exist", graphName)
	}
	snap := SnapshotName(graphName)
	if err := db.Conn.Del(ctx, snap).Err(); err != nil {
		return fmt.Errorf("drop old snapshot: %w", err)
	}
	if err := db.Conn.Do(ctx, "GRAPH.COPY", graphName, snap).Err(); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", graphName, snap, err)
	}
	return nil
}

// Result describes a completed restore.
type Result struct {
	Graph      string `json:"graph"`
	Snapshot   string `json:"restored_from"`
	Claims     int    `json:"claims"`
	DurationMs int64  `json:"duration_ms"`
}

// Restore replaces graphName with a copy of its pristine snapshot. Everything written to the
// graph since the snapshot (submitted claims, recorded dispositions) is discarded.
func Restore(db *falkordb.FalkorDB, graphName string) (*Result, error) {
	start := time.Now()
	if err := valid(graphName); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	snap, staging := SnapshotName(graphName), graphName+restoreSuffix
	if ok, err := exists(ctx, db, snap); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoSnapshot
	}
	if err := db.Conn.Del(ctx, staging).Err(); err != nil { // a crashed earlier restore may have left one
		return nil, fmt.Errorf("drop old staging graph: %w", err)
	}
	if err := db.Conn.Do(ctx, "GRAPH.COPY", snap, staging).Err(); err != nil {
		return nil, fmt.Errorf("copy %s -> %s: %w", snap, staging, err)
	}
	if err := db.Conn.Do(ctx, "RENAME", staging, graphName).Err(); err != nil {
		_ = db.Conn.Del(ctx, staging).Err()
		return nil, fmt.Errorf("swap in the restored graph: %w", err)
	}
	g := db.SelectGraph(graphName)
	// The restored graph carries the snapshot's evidence version; give it a new epoch so no
	// reader reuses a ring result cached for pre-reset data.
	if err := ingest.RenewEvidenceEpoch(g); err != nil {
		return nil, err
	}
	res := &Result{Graph: graphName, Snapshot: snap}
	if r, err := g.ROQuery(`MATCH (c:Claim) RETURN count(c) AS n`, nil, nil); err == nil && r.Next() {
		if v, ok := r.Record().Get("n"); ok {
			if n, ok := v.(int64); ok {
				res.Claims = int(n)
			}
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}
