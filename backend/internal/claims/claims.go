// Package claims provides read-only lookups of stored claims for the claim queue
// and the claim detail view (the basis of the GetClaim agent tool).
package claims

import (
	"fmt"
	"strings"

	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 200
	MaxListOffset    = 1_000_000
	MaxSearchLength  = 64
)

// ErrNotFound is returned when a claim does not exist.
var ErrNotFound = investigate.ErrClaimNotFound

// Summary is one row of the claim queue.
type Summary struct {
	ClaimID           string `json:"claim_id"`
	FilingDate        string `json:"filing_date"`
	IncidentDate      string `json:"incident_date"`
	Amount            int64  `json:"amount"`
	Status            string `json:"status"`
	PolicyCheckStatus string `json:"policy_check_status"`
	Disposition       string `json:"disposition,omitempty"` // latest recorded investigator outcome
}

// Entity is a graph entity attached to a claim.
type Entity struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Type   string `json:"type,omitempty"`   // payout type
	Public *bool  `json:"public,omitempty"` // device / IP visibility
	Status string `json:"status,omitempty"` // policy status
	Start  string `json:"start,omitempty"`  // policy coverage start
	End    string `json:"end,omitempty"`    // policy coverage end
}

// Claim is the full detail of one claim and its directly attached entities.
type Claim struct {
	Summary
	LossType         string  `json:"loss_type,omitempty"`
	IncidentLocation string  `json:"incident_location,omitempty"`
	Claimant         *Entity `json:"claimant,omitempty"`
	Policy           *Entity `json:"policy,omitempty"`
	Vehicle          *Entity `json:"vehicle,omitempty"`
	RepairShop       *Entity `json:"repair_shop,omitempty"`
	Payout           *Entity `json:"payout_destination,omitempty"`
	TowProvider      *Entity `json:"tow_provider,omitempty"`
	Device           *Entity `json:"device,omitempty"`
	IP               *Entity `json:"ip_address,omitempty"`
}

const (
	cypherList = `
		MATCH (c:Claim)
		OPTIONAL MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c)
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date,
		       c.amount AS amount, c.status AS status, c.policy_check_status AS pcs, ic.disposition AS disposition
		ORDER BY c.filing_date DESC, c.claim_id
		LIMIT $limit`

	// cypherPage pages the queue (newest filings first), optionally filtered to claim ids that
	// contain $q. The page is cut before the case lookup, so only the shown claims are expanded.
	cypherPage = `
		MATCH (c:Claim) WHERE $q = '' OR c.claim_id CONTAINS $q
		WITH c ORDER BY c.filing_date DESC, c.claim_id SKIP $offset LIMIT $limit
		OPTIONAL MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c)
		WITH c, ic ORDER BY ic.timestamp DESC
		WITH c, collect(ic.disposition)[0] AS disposition
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date,
		       c.amount AS amount, c.status AS status, c.policy_check_status AS pcs, disposition
		ORDER BY c.filing_date DESC, c.claim_id`

	cypherCount = `MATCH (c:Claim) WHERE $q = '' OR c.claim_id CONTAINS $q RETURN count(c) AS n`

	cypherGet = `
		MATCH (c:Claim {claim_id:$id})
		OPTIONAL MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c)
		OPTIONAL MATCH (c)-[:FILED_BY]->(cl:Claimant)
		OPTIONAL MATCH (c)-[:COVERED_BY]->(pol:Policy)
		OPTIONAL MATCH (c)-[:INVOLVES]->(v:Vehicle)
		OPTIONAL MATCH (c)-[:REPAIRED_AT]->(s:RepairShop)
		OPTIONAL MATCH (c)-[:PAID_TO]->(pd:PayoutDestination)
		OPTIONAL MATCH (c)-[:TOWED_BY]->(t:TowProvider)
		OPTIONAL MATCH (c)-[:USED_DEVICE]->(d:Device)
		OPTIONAL MATCH (c)-[:FILED_FROM]->(ip:IPAddress)
		RETURN c.claim_id AS claim_id, c.filing_date AS filing_date, c.incident_date AS incident_date,
		       c.amount AS amount, c.status AS status, c.policy_check_status AS pcs,
		       c.loss_type AS loss_type, c.incident_location AS location, ic.disposition AS disposition,
		       cl.claimant_id AS claimant_id, cl.name AS claimant_name,
		       pol.policy_id AS policy_id, pol.status AS policy_status, pol.start_date AS policy_start, pol.end_date AS policy_end,
		       v.vin AS vin, s.shop_id AS shop_id, s.name AS shop_name,
		       pd.account_id AS payout_id, pd.type AS payout_type,
		       t.tow_id AS tow_id, t.name AS tow_name,
		       d.device_id AS device_id, d.is_public AS device_public,
		       ip.ip AS ip, ip.is_public AS ip_public`

	cypherTowProviders = `
		MATCH (c:Claim)-[:TOWED_BY]->(t:TowProvider)
		WHERE c.claim_id IN $ids
		RETURN c.claim_id AS claim_id, t.tow_id AS tow_id, t.name AS tow_name`
)

// Page is one page of the claim queue.
type Page struct {
	Claims []Summary `json:"claims"`
	Total  int       `json:"total"` // claims matching the search (all claims when q is empty)
	Offset int       `json:"offset"`
	Limit  int       `json:"limit"`
	Query  string    `json:"q,omitempty"`
}

// ListPage returns claims newest filings first, skipping offset, optionally only those whose
// id contains q (case-insensitive for the letters of an id: q is upper-cased).
func ListPage(g *falkordb.Graph, limit, offset int, q string) (*Page, error) {
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d", MaxListLimit)
	}
	if offset < 0 || offset > MaxListOffset {
		return nil, fmt.Errorf("offset must be between 0 and %d", MaxListOffset)
	}
	q = strings.ToUpper(strings.TrimSpace(q))
	if q != "" && (len(q) > MaxSearchLength || !ingest.ValidID(q)) {
		return nil, fmt.Errorf("search must be up to %d letters, digits, '-' or '_'", MaxSearchLength)
	}
	p := &Page{Claims: []Summary{}, Offset: offset, Limit: limit, Query: q}
	params := map[string]any{"limit": limit, "offset": offset, "q": q}
	res, err := g.ROQuery(cypherPage, params, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return p, nil // no graph yet: empty queue
		}
		return nil, fmt.Errorf("list claims: %w", err)
	}
	for res.Next() {
		p.Claims = append(p.Claims, summary(res.Record().Get))
	}
	cnt, err := g.ROQuery(cypherCount, map[string]any{"q": q}, nil)
	if err != nil {
		return nil, fmt.Errorf("count claims: %w", err)
	}
	if cnt.Next() {
		if n, err := cnt.Record().GetByIndex(0); err == nil {
			if v, isInt := n.(int64); isInt {
				p.Total = int(v)
			}
		}
	}
	return p, nil
}

// List returns the claim queue, newest filings first.
func List(g *falkordb.Graph, limit int) ([]Summary, error) {
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d", MaxListLimit)
	}
	res, err := g.ROQuery(cypherList, map[string]any{"limit": limit}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return []Summary{}, nil // no graph yet: empty queue
		}
		return nil, fmt.Errorf("list claims: %w", err)
	}
	out := []Summary{}
	for res.Next() {
		out = append(out, summary(res.Record().Get))
	}
	return out, nil
}

// Get returns one claim and its attached entities, or ErrNotFound.
func Get(g *falkordb.Graph, id string) (*Claim, error) {
	if !ingest.ValidID(id) {
		return nil, fmt.Errorf("invalid claim_id %q", id)
	}
	res, err := g.ROQuery(cypherGet, map[string]any{"id": id}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get claim: %w", err)
	}
	if !res.Next() {
		return nil, ErrNotFound
	}
	get := res.Record().Get
	c := &Claim{Summary: summary(get), LossType: str(get, "loss_type"), IncidentLocation: str(get, "location")}
	if v := str(get, "claimant_id"); v != "" {
		c.Claimant = &Entity{ID: v, Name: str(get, "claimant_name")}
	}
	if v := str(get, "policy_id"); v != "" {
		c.Policy = &Entity{ID: v, Status: str(get, "policy_status"), Start: str(get, "policy_start"), End: str(get, "policy_end")}
	}
	if v := str(get, "vin"); v != "" {
		c.Vehicle = &Entity{ID: v}
	}
	if v := str(get, "shop_id"); v != "" {
		c.RepairShop = &Entity{ID: v, Name: str(get, "shop_name")}
	}
	if v := str(get, "payout_id"); v != "" {
		c.Payout = &Entity{ID: v, Type: str(get, "payout_type")}
	}
	if v := str(get, "tow_id"); v != "" {
		c.TowProvider = &Entity{ID: v, Name: str(get, "tow_name")}
	}
	if v := str(get, "device_id"); v != "" {
		c.Device = &Entity{ID: v, Public: boolPtr(get, "device_public")}
	}
	if v := str(get, "ip"); v != "" {
		c.IP = &Entity{ID: v, Public: boolPtr(get, "ip_public")}
	}
	return c, nil
}

// TowProviders returns the tow provider (id, name) attached to each given claim.
// Tow is context-only, so it is fetched separately from the investigation.
func TowProviders(g *falkordb.Graph, claimIDs []string) (map[string]Entity, error) {
	out := map[string]Entity{}
	if len(claimIDs) == 0 {
		return out, nil
	}
	ids := make([]any, len(claimIDs))
	for i, id := range claimIDs {
		ids[i] = id
	}
	res, err := g.ROQuery(cypherTowProviders, map[string]any{"ids": ids}, nil)
	if err != nil {
		return nil, fmt.Errorf("tow providers: %w", err)
	}
	for res.Next() {
		get := res.Record().Get
		out[str(get, "claim_id")] = Entity{ID: str(get, "tow_id"), Name: str(get, "tow_name")}
	}
	return out, nil
}

type getter func(string) (any, bool)

func summary(get getter) Summary {
	s := Summary{
		ClaimID: str(get, "claim_id"), FilingDate: str(get, "filing_date"), IncidentDate: str(get, "incident_date"),
		Status: str(get, "status"), PolicyCheckStatus: str(get, "pcs"), Disposition: str(get, "disposition"),
	}
	if v, _ := get("amount"); v != nil {
		if n, ok := v.(int64); ok {
			s.Amount = n
		}
	}
	if s.PolicyCheckStatus == "" {
		s.PolicyCheckStatus = "unknown"
	}
	return s
}

func str(get getter, k string) string {
	v, _ := get(k)
	s, _ := v.(string)
	return s
}

func boolPtr(get getter, k string) *bool {
	v, _ := get(k)
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}
