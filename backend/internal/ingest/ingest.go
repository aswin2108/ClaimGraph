package ingest

import (
	"fmt"
	"strings"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Result describes what an ingestion did.
type Result struct {
	ClaimID           string        `json:"claim_id"`
	Created           bool          `json:"created"` // false when the claim already existed (re-submission)
	PolicyChecks      []PolicyCheck `json:"policy_checks"`
	PolicyCheckStatus string        `json:"policy_check_status"`
}

const (
	cypherClaimExists = `MATCH (c:Claim {claim_id:$claim_id}) RETURN count(c)`

	// Core claim, policy, claimant, vehicle, shop and payout. MERGE keeps it idempotent.
	cypherCore = `
		MERGE (c:Claim {claim_id:$claim_id})
		  SET c.incident_date=$incident, c.filing_date=$filed, c.amount=$amount, c.status='open',
		      c.loss_type=$loss_type, c.incident_location=$location, c.policy_check_status=$check_status
		MERGE (pol:Policy {policy_id:$policy_id})
		  SET pol.start_date=$pol_start, pol.end_date=$pol_end, pol.status=$pol_status
		MERGE (cl:Claimant {claimant_id:$claimant_id})
		  SET cl.name=$claimant_name, cl.is_driver=true
		MERGE (v:Vehicle {vin:$vin})
		MERGE (s:RepairShop {shop_id:$shop_id})
		MERGE (pd:PayoutDestination {account_id:$payout_id})
		  SET pd.type=$payout_type
		MERGE (c)-[:COVERED_BY]->(pol)
		MERGE (c)-[:FILED_BY {source:$src, evidence_id:$evid, recorded_at:$filed}]->(cl)
		MERGE (c)-[:INVOLVES]->(v)
		MERGE (c)-[:REPAIRED_AT {source:$src, evidence_id:$evid, recorded_at:$filed}]->(s)
		MERGE (c)-[:PAID_TO {source:$src, evidence_id:$evid, recorded_at:$filed}]->(pd)`

	cypherTow = `
		MATCH (c:Claim {claim_id:$claim_id})
		MERGE (t:TowProvider {tow_id:$tow_id})
		MERGE (c)-[:TOWED_BY {source:$src, evidence_id:$evid, recorded_at:$filed}]->(t)`

	cypherContact = `
		MATCH (cl:Claimant {claimant_id:$claimant_id})
		MERGE (cp:ContactPoint {type:$contact_type, value:$contact_value})
		MERGE (cl)-[:HAS_CONTACT {source:$src, evidence_id:$evid, recorded_at:$filed}]->(cp)`

	cypherAddress = `
		MATCH (cl:Claimant {claimant_id:$claimant_id})
		MERGE (a:Address {address_id:$address_id})
		MERGE (cl)-[:RESIDES_AT {source:$src, evidence_id:$evid, recorded_at:$filed}]->(a)`

	cypherDevice = `
		MATCH (c:Claim {claim_id:$claim_id})
		MERGE (d:Device {device_id:$device_id})
		  SET d.is_public=$device_public
		MERGE (c)-[:USED_DEVICE {source:$src, evidence_id:$evid, recorded_at:$filed}]->(d)`

	cypherIP = `
		MATCH (c:Claim {claim_id:$claim_id})
		MERGE (ip:IPAddress {ip:$ip})
		  SET ip.is_public=$ip_public
		MERGE (c)-[:FILED_FROM {source:$src, evidence_id:$evid, recorded_at:$filed}]->(ip)`
)

// Ingest validates the FNOL and writes it to the graph. Re-submitting the same
// FNOL updates properties in place and creates no duplicate nodes or edges.
// All values are passed as Cypher parameters; no query text is built from input.
func Ingest(g *falkordb.Graph, f *FNOL) (*Result, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	checks := f.PolicyChecks()
	status := PolicyCheckStatus(checks)

	existed, err := claimExists(g, f.ClaimID)
	if err != nil {
		return nil, err
	}

	base := map[string]any{
		"claim_id": f.ClaimID, "claimant_id": f.ClaimantID,
		"src": "fnol_submission", "evid": "EVID-" + f.ClaimID, "filed": f.FilingDate,
	}
	with := func(extra map[string]any) map[string]any {
		m := make(map[string]any, len(base)+len(extra))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	run := func(q string, p map[string]any) error {
		if _, err := g.Query(q, p, nil); err != nil {
			return fmt.Errorf("ingest %s: %w", f.ClaimID, err)
		}
		return nil
	}

	if err := run(cypherCore, with(map[string]any{
		"incident": f.IncidentDate, "amount": f.Amount, "loss_type": orDefault(f.LossType, "collision"),
		"location": f.IncidentLocation, "check_status": status,
		"policy_id": f.PolicyID, "pol_start": f.PolicyStart, "pol_end": f.PolicyEnd, "pol_status": f.PolicyStatus,
		"claimant_name": f.ClaimantName, "vin": f.VIN, "shop_id": f.RepairShopID,
		"payout_id": f.PayoutAccountID, "payout_type": f.PayoutType,
	})); err != nil {
		return nil, err
	}
	if f.TowProviderID != "" {
		if err := run(cypherTow, with(map[string]any{"tow_id": f.TowProviderID})); err != nil {
			return nil, err
		}
	}
	if f.ContactType != "" {
		if err := run(cypherContact, with(map[string]any{
			"contact_type": f.ContactType, "contact_value": f.ContactValue})); err != nil {
			return nil, err
		}
	}
	if f.AddressID != "" {
		if err := run(cypherAddress, with(map[string]any{"address_id": f.AddressID})); err != nil {
			return nil, err
		}
	}
	if f.DeviceID != "" {
		if err := run(cypherDevice, with(map[string]any{
			"device_id": f.DeviceID, "device_public": f.DevicePublic})); err != nil {
			return nil, err
		}
	}
	if f.IPAddress != "" {
		if err := run(cypherIP, with(map[string]any{
			"ip": f.IPAddress, "ip_public": IPIsPublic(f.IPAddress)})); err != nil {
			return nil, err
		}
	}

	return &Result{ClaimID: f.ClaimID, Created: !existed, PolicyChecks: checks, PolicyCheckStatus: status}, nil
}

func claimExists(g *falkordb.Graph, claimID string) (bool, error) {
	res, err := g.ROQuery(cypherClaimExists, map[string]any{"claim_id": claimID}, nil)
	if err != nil {
		// A read-only query on a graph that does not exist yet means "no claims".
		if strings.Contains(err.Error(), "empty key") {
			return false, nil
		}
		return false, fmt.Errorf("check existing claim: %w", err)
	}
	if !res.Next() {
		return false, nil
	}
	v, err := res.Record().GetByIndex(0)
	if err != nil {
		return false, err
	}
	n, ok := v.(int64)
	return ok && n > 0, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
