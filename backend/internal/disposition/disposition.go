// Package disposition records a (simulated) human investigator decision on a claim
// as an InvestigationCase in FalkorDB. These outcomes are the system's graph-based
// memory: later investigations of related claims retrieve them as context.
//
// Outcomes are stored and retrieved only. They never change scoring weights, and
// only "confirmed_fraud" can earn the historical-outcome points.
package disposition

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Allowed dispositions.
const (
	ConfirmedFraud   = "confirmed_fraud"
	NotSubstantiated = "not_substantiated"
	NeedsInfo        = "needs_info"

	MaxNotesLen = 500
	sourceTag   = "investigator_simulated"
)

// ErrInvalid marks a rejected input (bad claim id, disposition or notes).
var ErrInvalid = errors.New("invalid input")

// ErrClaimNotFound is returned when the claim being closed out does not exist.
var ErrClaimNotFound = investigate.ErrClaimNotFound

// Case is the stored outcome for a claim.
type Case struct {
	CaseID      string `json:"case_id"`
	ClaimID     string `json:"claim_id"`
	Disposition string `json:"disposition"`
	Notes       string `json:"notes,omitempty"`
	RecordedAt  string `json:"recorded_at"`
	Source      string `json:"source"`
	// Created is true for a new case, false when an existing case was updated/re-recorded.
	Created bool `json:"created"`
	// Previous holds the earlier disposition when this call changed it.
	Previous string `json:"previous_disposition,omitempty"`
}

// Recorder writes dispositions. Now is injectable so tests are deterministic.
type Recorder struct {
	Graph *falkordb.Graph
	Now   func() time.Time
}

// New returns a Recorder using the real clock.
func New(g *falkordb.Graph) *Recorder { return &Recorder{Graph: g, Now: time.Now} }

// ValidDisposition reports whether d is an allowed value.
func ValidDisposition(d string) bool {
	return d == ConfirmedFraud || d == NotSubstantiated || d == NeedsInfo
}

const (
	cypherLookup = `
		MATCH (c:Claim {claim_id:$claim_id})
		OPTIONAL MATCH (ic:InvestigationCase {case_id:$case_id})
		RETURN c.claim_id AS claim_id, ic.disposition AS existing`

	// MERGE on the deterministic case id keeps one case per claim: re-recording updates it.
	cypherRecord = `
		MATCH (c:Claim {claim_id:$claim_id})
		MERGE (ic:InvestigationCase {case_id:$case_id})
		  SET ic.disposition=$disposition, ic.notes=$notes, ic.timestamp=$ts, ic.source=$src
		MERGE (ic)-[r:INVESTIGATES]->(c)
		  SET r.source=$src, r.evidence_id=$evid, r.recorded_at=$ts`
)

// CaseID is the stable identifier of the single case kept per claim.
func CaseID(claimID string) string { return "CASE-" + claimID }

// Record stores the investigator's decision for a claim. It validates every input,
// uses a hardcoded parameterized query, and never modifies the claim or its evidence.
func (r *Recorder) Record(claimID, disp, notes string) (*Case, error) {
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("%w: invalid claim_id %q", ErrInvalid, claimID)
	}
	if !ValidDisposition(disp) {
		return nil, fmt.Errorf("%w: disposition must be one of %s, %s, %s", ErrInvalid, ConfirmedFraud, NotSubstantiated, NeedsInfo)
	}
	notes, err := cleanNotes(notes)
	if err != nil {
		return nil, err
	}

	caseID := CaseID(claimID)
	existing, err := r.lookup(claimID, caseID)
	if err != nil {
		return nil, err
	}

	ts := r.Now().UTC().Format(time.RFC3339)
	_, err = r.Graph.Query(cypherRecord, map[string]any{
		"claim_id": claimID, "case_id": caseID, "disposition": disp, "notes": notes,
		"ts": ts, "src": sourceTag, "evid": "EVID-" + caseID,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("record disposition: %w", err)
	}

	c := &Case{CaseID: caseID, ClaimID: claimID, Disposition: disp, Notes: notes,
		RecordedAt: ts, Source: sourceTag, Created: existing == ""}
	if existing != "" && existing != disp {
		c.Previous = existing
	}
	return c, nil
}

// lookup confirms the claim exists and returns the current disposition ("" if none).
func (r *Recorder) lookup(claimID, caseID string) (string, error) {
	res, err := r.Graph.ROQuery(cypherLookup, map[string]any{"claim_id": claimID, "case_id": caseID}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return "", ErrClaimNotFound
		}
		return "", fmt.Errorf("lookup claim: %w", err)
	}
	if !res.Next() {
		return "", ErrClaimNotFound
	}
	v, _ := res.Record().Get("existing")
	s, _ := v.(string)
	return s, nil
}

// cleanNotes trims notes, removes control characters and enforces the length limit.
func cleanNotes(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) > MaxNotesLen {
		return "", fmt.Errorf("%w: notes must be at most 500 characters", ErrInvalid)
	}
	return s, nil
}

// cypherLatest returns the most recent recorded outcome on a claim (seeded cases included).
const cypherLatest = `
	MATCH (ic:InvestigationCase)-[:INVESTIGATES]->(c:Claim {claim_id:$claim_id})
	RETURN ic.case_id AS case_id, ic.disposition AS disposition, ic.notes AS notes,
	       ic.timestamp AS ts, ic.source AS src
	ORDER BY ic.timestamp DESC LIMIT 1`

// Latest returns the most recent recorded outcome on a claim, or nil when there is none.
// It includes the investigator's free-text notes, so it is for the human UI only: the agent's
// tools never call it (notes are never sent to the model).
func Latest(g *falkordb.Graph, claimID string) (*Case, error) {
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("%w: invalid claim_id %q", ErrInvalid, claimID)
	}
	res, err := g.ROQuery(cypherLatest, map[string]any{"claim_id": claimID}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return nil, nil
		}
		return nil, fmt.Errorf("latest disposition: %w", err)
	}
	if !res.Next() {
		return nil, nil
	}
	rec := res.Record()
	str := func(k string) string {
		v, _ := rec.Get(k)
		s, _ := v.(string)
		return s
	}
	return &Case{CaseID: str("case_id"), ClaimID: claimID, Disposition: str("disposition"), Notes: str("notes"),
		RecordedAt: str("ts"), Source: str("src")}, nil
}
