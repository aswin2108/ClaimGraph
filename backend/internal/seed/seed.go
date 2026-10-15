// Package seed loads a small, fully synthetic auto-insurance graph.
// Every write uses MERGE, so running it repeatedly never duplicates data.
package seed

import (
	"fmt"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

type claim struct {
	ID, Claimant, Policy, VIN, Shop, Payout, Device, Tow string
	Incident, Filed                                      string
	Amount                                               int
}

var claimants = [][2]string{
	{"CLM-DRV-01", "Ana Ruiz"}, {"CLM-DRV-02", "Ben Cole"}, {"CLM-DRV-03", "Cara Diaz"},
	{"CLM-DRV-04", "Dev Patel"}, {"CLM-DRV-05", "Eli Marsh"},
}

var vehicles = [][4]string{
	{"1HGSYN00000000001", "Honda", "Civic", "2019"}, {"1HGSYN00000000002", "Toyota", "Camry", "2020"},
	{"1HGSYN00000000003", "Ford", "Focus", "2018"}, {"1HGSYN00000000004", "Mazda", "CX-5", "2021"},
	{"1HGSYN00000000005", "Kia", "Soul", "2017"},
}

var shops = [][2]string{{"SHOP-01", "Northside Auto Body"}, {"SHOP-02", "Quick Fix Collision"}}
var tows = [][2]string{{"TOW-01", "Rapid Tow Co"}}

// PAY-01 is shared by three claimants (suspicious pattern); PAY-VENDOR is a repair shop's account.
var payouts = [][2]string{
	{"PAY-01", "claimant"}, {"PAY-02", "claimant"}, {"PAY-VENDOR", "vendor_direct_settlement"},
}

// device_id, fingerprint, is_public
var devices = []struct {
	ID, Fingerprint string
	Public          bool
}{
	{"DEV-01", "fp-synth-aaa111", false},
	{"DEV-02", "fp-synth-bbb222", false},
}

var claims = []claim{
	{"CLM-1001", "CLM-DRV-01", "POL-5001", "1HGSYN00000000001", "SHOP-01", "PAY-01", "DEV-01", "TOW-01", "2026-09-01", "2026-09-02", 4200},
	{"CLM-1002", "CLM-DRV-02", "POL-5002", "1HGSYN00000000002", "SHOP-02", "PAY-01", "DEV-01", "TOW-01", "2026-09-04", "2026-09-05", 3900},
	{"CLM-1003", "CLM-DRV-03", "POL-5003", "1HGSYN00000000003", "SHOP-01", "PAY-01", "DEV-02", "", "2026-09-06", "2026-09-07", 5100},
	{"CLM-1004", "CLM-DRV-04", "POL-5004", "1HGSYN00000000004", "SHOP-01", "PAY-VENDOR", "", "", "2026-07-10", "2026-07-11", 2300},
	{"CLM-1005", "CLM-DRV-05", "POL-5005", "1HGSYN00000000005", "SHOP-02", "PAY-02", "", "", "2026-06-01", "2026-06-02", 1800},
}

// Run seeds the graph and returns the number of statements executed.
func Run(g *falkordb.Graph) (int, error) {
	n := 0
	if err := ingest.EnsureIndexes(g); err != nil {
		return n, err
	}
	exec := func(q string, p map[string]interface{}) error {
		if _, err := g.Query(q, p, nil); err != nil {
			return fmt.Errorf("%w (query: %s)", err, q)
		}
		n++
		return nil
	}

	for _, c := range claimants {
		if err := exec(`MERGE (c:Claimant {claimant_id:$id}) SET c.name=$name, c.is_driver=true`,
			map[string]interface{}{"id": c[0], "name": c[1]}); err != nil {
			return n, err
		}
	}
	for _, v := range vehicles {
		if err := exec(`MERGE (v:Vehicle {vin:$vin}) SET v.make=$make, v.model=$model, v.year=$year`,
			map[string]interface{}{"vin": v[0], "make": v[1], "model": v[2], "year": v[3]}); err != nil {
			return n, err
		}
	}
	for _, s := range shops {
		if err := exec(`MERGE (s:RepairShop {shop_id:$id}) SET s.name=$name`,
			map[string]interface{}{"id": s[0], "name": s[1]}); err != nil {
			return n, err
		}
	}
	for _, t := range tows {
		if err := exec(`MERGE (t:TowProvider {tow_id:$id}) SET t.name=$name`,
			map[string]interface{}{"id": t[0], "name": t[1]}); err != nil {
			return n, err
		}
	}
	for _, p := range payouts {
		if err := exec(`MERGE (p:PayoutDestination {account_id:$id}) SET p.type=$type`,
			map[string]interface{}{"id": p[0], "type": p[1]}); err != nil {
			return n, err
		}
	}
	for _, d := range devices {
		if err := exec(`MERGE (d:Device {device_id:$id}) SET d.device_fingerprint=$fp, d.is_public=$pub`,
			map[string]interface{}{"id": d.ID, "fp": d.Fingerprint, "pub": d.Public}); err != nil {
			return n, err
		}
	}

	for _, c := range claims {
		edge := map[string]interface{}{
			"id": c.ID, "src": "synthetic_seed", "evid": "EVID-" + c.ID, "filed": c.Filed,
		}
		p := map[string]interface{}{
			"id": c.ID, "claimant": c.Claimant, "policy": c.Policy, "vin": c.VIN,
			"shop": c.Shop, "payout": c.Payout, "incident": c.Incident, "filed": c.Filed,
			"amount": c.Amount, "evid": "EVID-" + c.ID, "src": "synthetic_seed",
		}
		// Claim + policy + links to claimant, vehicle, shop, payout; evidence metadata on edges.
		if err := exec(`
			MERGE (c:Claim {claim_id:$id})
			  SET c.incident_date=$incident, c.filing_date=$filed, c.amount=$amount, c.status='open',
			      c.loss_type='collision', c.incident_location='Synthetic City',
			      c.policy_check_status='passed' // active policy, incident inside coverage, amount under the limit
			MERGE (pol:Policy {policy_id:$policy})
			  SET pol.start_date='2026-01-01', pol.end_date='2026-12-31', pol.status='active'
			WITH c, pol
			MATCH (cl:Claimant {claimant_id:$claimant}), (v:Vehicle {vin:$vin}),
			      (s:RepairShop {shop_id:$shop}), (pd:PayoutDestination {account_id:$payout})
			MERGE (c)-[:COVERED_BY]->(pol)
			MERGE (c)-[:FILED_BY {source:$src, evidence_id:$evid, recorded_at:$filed}]->(cl)
			MERGE (c)-[:INVOLVES]->(v)
			MERGE (c)-[:REPAIRED_AT {source:$src, evidence_id:$evid, recorded_at:$filed}]->(s)
			MERGE (c)-[:PAID_TO {source:$src, evidence_id:$evid, recorded_at:$filed}]->(pd)`, p); err != nil {
			return n, err
		}
		if c.Device != "" {
			edge["dev"] = c.Device
			if err := exec(`MATCH (c:Claim {claim_id:$id}), (d:Device {device_id:$dev})
				MERGE (c)-[:USED_DEVICE {source:$src, evidence_id:$evid, recorded_at:$filed}]->(d)`, edge); err != nil {
				return n, err
			}
		}
		if c.Tow != "" {
			edge["tow"] = c.Tow
			if err := exec(`MATCH (c:Claim {claim_id:$id}), (t:TowProvider {tow_id:$tow})
				MERGE (c)-[:TOWED_BY {source:$src, evidence_id:$evid, recorded_at:$filed}]->(t)`, edge); err != nil {
				return n, err
			}
		}
	}
	// Labels and evidence costs used by the graph algorithms (WCC, MSF, SPpaths).
	if err := ingest.EnsureOverlay(g); err != nil {
		return n, err
	}
	return n, nil
}
