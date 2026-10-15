package synth

// Bulk loader.
//
// It writes EXACTLY what ingest.Ingest (one FNOL at a time) and disposition.Recorder would
// write for the same data: the same labels, properties, evidence metadata, one evidence
// edge per FNOL, and the same analytics overlay. TestBulkLoadEqualsIngest proves it.
//
// Speed: FalkorDB MERGE that creates nodes is slow in bulk (measured: 20,000 new nodes
// took 90 s in one UNWIND...MERGE and 12 s in batches of 500), while UNWIND...CREATE took
// 90 ms. So the loader deduplicates in Go and CREATEs into a fresh staging graph, then
// atomically RENAMEs it over the target. Re-running is idempotent (deterministic data,
// fresh staging, atomic swap) and the application never reads a half-loaded graph.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	batchSize      = 2000
	fnolSource     = "fnol_submission"        // ingest's evidence source tag
	caseSource     = "investigator_simulated" // disposition.Recorder's source tag
	stagingSuffix  = "__staging"
	loadTimeoutSec = 600
)

// Node creation: one constant template per label.
var nodeQueries = map[string]string{
	"Claim": `UNWIND $rows AS r CREATE (:Claim {claim_id:r.id, incident_date:r.incident, filing_date:r.filed, amount:r.amount,
		status:'open', loss_type:r.loss_type, incident_location:r.location, policy_check_status:r.check})`,
	"Policy":            `UNWIND $rows AS r CREATE (:Policy {policy_id:r.id, start_date:r.start, end_date:r.end, status:r.status})`,
	"Claimant":          `UNWIND $rows AS r CREATE (:Claimant {claimant_id:r.id, name:r.name, is_driver:true})`,
	"Vehicle":           `UNWIND $rows AS r CREATE (:Vehicle {vin:r.id})`,
	"RepairShop":        `UNWIND $rows AS r CREATE (:RepairShop {shop_id:r.id})`,
	"PayoutDestination": `UNWIND $rows AS r CREATE (:PayoutDestination {account_id:r.id, type:r.type})`,
	"TowProvider":       `UNWIND $rows AS r CREATE (:TowProvider {tow_id:r.id})`,
	"ContactPoint":      `UNWIND $rows AS r CREATE (:ContactPoint {type:r.type, value:r.id})`,
	"Address":           `UNWIND $rows AS r CREATE (:Address {address_id:r.id})`,
	"Device":            `UNWIND $rows AS r CREATE (:Device {device_id:r.id, is_public:r.public})`,
	"IPAddress":         `UNWIND $rows AS r CREATE (:IPAddress {ip:r.id, is_public:r.public})`,
}

// Edge creation: rows carry claim id (c), target key (x) and evidence metadata.
var edgeQueries = map[string]string{
	"COVERED_BY":  `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:Policy {policy_id:r.x}) CREATE (c)-[:COVERED_BY]->(x)`,
	"INVOLVES":    `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:Vehicle {vin:r.x}) CREATE (c)-[:INVOLVES]->(x)`,
	"FILED_BY":    `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:Claimant {claimant_id:r.x}) CREATE (c)-[:FILED_BY {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"REPAIRED_AT": `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:RepairShop {shop_id:r.x}) CREATE (c)-[:REPAIRED_AT {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"PAID_TO":     `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:PayoutDestination {account_id:r.x}) CREATE (c)-[:PAID_TO {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"TOWED_BY":    `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:TowProvider {tow_id:r.x}) CREATE (c)-[:TOWED_BY {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"USED_DEVICE": `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:Device {device_id:r.x}) CREATE (c)-[:USED_DEVICE {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"FILED_FROM":  `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.c}), (x:IPAddress {ip:r.x}) CREATE (c)-[:FILED_FROM {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	// Claimant-level evidence: ingest MERGEs these WITH the FNOL's evidence properties, so a
	// claimant who files twice gets one edge per FNOL. Rows carry the claimant id in c.
	"HAS_CONTACT": `UNWIND $rows AS r MATCH (k:Claimant {claimant_id:r.c}), (x:ContactPoint {type:r.t, value:r.x}) CREATE (k)-[:HAS_CONTACT {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
	"RESIDES_AT":  `UNWIND $rows AS r MATCH (k:Claimant {claimant_id:r.c}), (x:Address {address_id:r.x}) CREATE (k)-[:RESIDES_AT {source:r.src, evidence_id:r.ev, recorded_at:r.at}]->(x)`,
}

const caseQuery = `UNWIND $rows AS r MATCH (c:Claim {claim_id:r.claim})
	CREATE (ic:InvestigationCase {case_id:r.id, disposition:r.disp, notes:r.notes, timestamp:r.ts, source:r.src})
	CREATE (ic)-[:INVESTIGATES {source:r.src, evidence_id:'EVID-' + r.id, recorded_at:r.ts}]->(c)`

// LoadReport describes a bulk load.
type LoadReport struct {
	Graph    string           `json:"graph"`
	Nodes    map[string]int   `json:"nodes"`
	Edges    map[string]int   `json:"edges"`
	Cases    int              `json:"cases"`
	StepsMs  map[string]int64 `json:"steps_ms"`
	TotalMs  int64            `json:"total_ms"`
	Swapped  bool             `json:"atomically_swapped"`
	Staging  string           `json:"staging_graph"`
	Warnings []string         `json:"warnings,omitempty"`
}

// Load bulk-loads ds into graph target via a staging graph and an atomic rename.
// progress (optional) receives one line per step.
func Load(db *falkordb.FalkorDB, target string, ds *Dataset, progress func(string)) (*LoadReport, error) {
	if progress == nil {
		progress = func(string) {}
	}
	start := time.Now()
	rep := &LoadReport{Graph: target, Staging: target + stagingSuffix, Nodes: map[string]int{}, Edges: map[string]int{}, StepsMs: map[string]int64{}}
	step := func(name string, t0 time.Time) {
		rep.StepsMs[name] = time.Since(t0).Milliseconds()
		progress(fmt.Sprintf("%-22s %6d ms", name, rep.StepsMs[name]))
	}

	g := db.SelectGraph(rep.Staging)
	_ = g.Delete() // a crashed earlier load may have left a staging graph behind

	t0 := time.Now()
	if err := ingest.EnsureIndexes(g); err != nil {
		return nil, err
	}
	step("indexes", t0)

	nodes, edges, err := rows(ds)
	if err != nil {
		return nil, err
	}
	for _, label := range sortedKeys(nodes) {
		t0 = time.Now()
		if err := batched(g, nodeQueries[label], nodes[label]); err != nil {
			return nil, fmt.Errorf("create %s nodes: %w", label, err)
		}
		rep.Nodes[label] = len(nodes[label])
		step("nodes "+label, t0)
	}
	for _, rel := range sortedKeys(edges) {
		t0 = time.Now()
		if err := batched(g, edgeQueries[rel], edges[rel]); err != nil {
			return nil, fmt.Errorf("create %s edges: %w", rel, err)
		}
		rep.Edges[rel] = len(edges[rel])
		step("edges "+rel, t0)
	}

	t0 = time.Now()
	var caseRows []map[string]any
	for _, c := range ds.Cases {
		caseRows = append(caseRows, map[string]any{"claim": c.ClaimID, "id": "CASE-" + c.ClaimID, "disp": c.Disposition,
			"notes": c.Notes, "ts": c.Timestamp, "src": caseSource})
	}
	if err := batched(g, caseQuery, caseRows); err != nil {
		return nil, fmt.Errorf("create cases: %w", err)
	}
	rep.Cases = len(caseRows)
	step("prior cases", t0)

	t0 = time.Now()
	if err := ingest.EnsureOverlay(g); err != nil {
		return nil, err
	}
	step("analytics overlay", t0)

	if err := verifyCounts(g, ds); err != nil {
		return nil, err
	}

	t0 = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeoutSec*time.Second)
	defer cancel()
	if err := db.Conn.Do(ctx, "RENAME", rep.Staging, target).Err(); err != nil {
		return nil, fmt.Errorf("swap %s -> %s: %w", rep.Staging, target, err)
	}
	rep.Swapped = true
	step("atomic swap", t0)
	rep.TotalMs = time.Since(start).Milliseconds()
	return rep, nil
}

// rows converts the dataset into deduplicated node rows and per-FNOL edge rows.
func rows(ds *Dataset) (map[string][]map[string]any, map[string][]map[string]any, error) {
	nodes := map[string][]map[string]any{}
	seen := map[string]map[string]any{} // label|key -> row (to detect conflicting values)
	addNode := func(label, key string, row map[string]any) error {
		k := label + "|" + key
		if prev, ok := seen[k]; ok {
			for f, v := range row {
				if prev[f] != v {
					return fmt.Errorf("%s %s has conflicting %s (%v vs %v)", label, key, f, prev[f], v)
				}
			}
			return nil
		}
		row["id"] = key
		seen[k] = row
		nodes[label] = append(nodes[label], row)
		return nil
	}
	edges := map[string][]map[string]any{}
	addEdge := func(rel string, row map[string]any) { edges[rel] = append(edges[rel], row) }

	for _, f := range ds.FNOLs {
		if err := f.Validate(); err != nil {
			return nil, nil, fmt.Errorf("generated FNOL %s is invalid: %w", f.ClaimID, err)
		}
		ev := map[string]any{"src": fnolSource, "ev": "EVID-" + f.ClaimID, "at": f.FilingDate}
		edge := func(rel, x string) {
			row := map[string]any{"c": f.ClaimID, "x": x}
			for k, v := range ev {
				row[k] = v
			}
			addEdge(rel, row)
		}
		lossType := f.LossType
		if lossType == "" {
			lossType = "collision"
		}
		errs := []error{
			addNode("Claim", f.ClaimID, map[string]any{"incident": f.IncidentDate, "filed": f.FilingDate, "amount": f.Amount,
				"loss_type": lossType, "location": f.IncidentLocation, "check": ingest.PolicyCheckStatus(f.PolicyChecks())}),
			addNode("Policy", f.PolicyID, map[string]any{"start": f.PolicyStart, "end": f.PolicyEnd, "status": f.PolicyStatus}),
			addNode("Claimant", f.ClaimantID, map[string]any{"name": f.ClaimantName}),
			addNode("Vehicle", f.VIN, map[string]any{}),
			addNode("RepairShop", f.RepairShopID, map[string]any{}),
			addNode("PayoutDestination", f.PayoutAccountID, map[string]any{"type": f.PayoutType}),
		}
		addEdge("COVERED_BY", map[string]any{"c": f.ClaimID, "x": f.PolicyID})
		addEdge("INVOLVES", map[string]any{"c": f.ClaimID, "x": f.VIN})
		edge("FILED_BY", f.ClaimantID)
		edge("REPAIRED_AT", f.RepairShopID)
		edge("PAID_TO", f.PayoutAccountID)
		if f.TowProviderID != "" {
			errs = append(errs, addNode("TowProvider", f.TowProviderID, map[string]any{}))
			edge("TOWED_BY", f.TowProviderID)
		}
		if f.DeviceID != "" {
			errs = append(errs, addNode("Device", f.DeviceID, map[string]any{"public": f.DevicePublic}))
			edge("USED_DEVICE", f.DeviceID)
		}
		if f.IPAddress != "" {
			errs = append(errs, addNode("IPAddress", f.IPAddress, map[string]any{"public": ingest.IPIsPublic(f.IPAddress)}))
			edge("FILED_FROM", f.IPAddress)
		}
		if f.ContactType != "" {
			errs = append(errs, addNode("ContactPoint", f.ContactValue, map[string]any{"type": f.ContactType}))
			row := map[string]any{"c": f.ClaimantID, "x": f.ContactValue, "t": f.ContactType}
			for k, v := range ev {
				row[k] = v
			}
			addEdge("HAS_CONTACT", row)
		}
		if f.AddressID != "" {
			errs = append(errs, addNode("Address", f.AddressID, map[string]any{}))
			row := map[string]any{"c": f.ClaimantID, "x": f.AddressID}
			for k, v := range ev {
				row[k] = v
			}
			addEdge("RESIDES_AT", row)
		}
		for _, err := range errs {
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return nodes, edges, nil
}

func batched(g *falkordb.Graph, q string, rs []map[string]any) error {
	for i := 0; i < len(rs); i += batchSize {
		end := i + batchSize
		if end > len(rs) {
			end = len(rs)
		}
		batch := make([]any, 0, end-i)
		for _, r := range rs[i:end] {
			batch = append(batch, r)
		}
		if _, err := g.Query(q, map[string]any{"rows": batch}, nil); err != nil {
			return err
		}
	}
	return nil
}

// verifyCounts refuses to swap a staging graph that is missing claims or cases.
func verifyCounts(g *falkordb.Graph, ds *Dataset) error {
	r, err := g.ROQuery(`MATCH (c:Claim) WITH count(c) AS claims MATCH (ic:InvestigationCase) RETURN claims, count(ic) AS cases`, nil, nil)
	if err != nil {
		return err
	}
	if !r.Next() {
		if len(ds.Cases) == 0 {
			return nil
		}
		return fmt.Errorf("staging graph has no cases")
	}
	claims, _ := r.Record().Get("claims")
	cases, _ := r.Record().Get("cases")
	if claims != int64(len(ds.FNOLs)) || cases != int64(len(ds.Cases)) {
		return fmt.Errorf("staging graph incomplete: %v claims / %v cases, want %d / %d", claims, cases, len(ds.FNOLs), len(ds.Cases))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
