package disposition

import (
	"fmt"
	"strings"
	"time"

	"claimgraph/internal/ingest"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Closing an investigation: the investigator has read the report and makes the final call.
// The case keeps the same disposition values (so a rejected claim still earns the +15
// confirmed-case points on later linked claims), plus status "closed", the outcome, the
// reason and a sealed copy of the report the investigator read. A closed case is final:
// Record and Close refuse it, and the agent refuses to run on the claim.

// Case status and closing outcomes.
const (
	StatusClosed    = "closed"
	OutcomeApproved = "approved" // no fraud found; the claim returns to ordinary claims handling
	OutcomeRejected = "rejected" // fraud confirmed by the investigator

	MaxSealedReportBytes = 256 << 10
)

// OutcomeDisposition maps a closing outcome to the stored disposition ("" if invalid).
func OutcomeDisposition(outcome string) string {
	switch outcome {
	case OutcomeApproved:
		return NotSubstantiated
	case OutcomeRejected:
		return ConfirmedFraud
	}
	return ""
}

// Closure is a closed investigation as shown to the investigator. The sealed report itself
// is fetched separately (SealedReport).
type Closure struct {
	CaseID       string `json:"case_id"`
	ClaimID      string `json:"claim_id"`
	Outcome      string `json:"outcome"`
	Disposition  string `json:"disposition"`
	Reason       string `json:"reason"`
	ClosedAt     string `json:"closed_at"`
	ReportScore  int64  `json:"report_score"`
	ReportBand   string `json:"report_band"`
	ReportSource string `json:"report_source"` // llm | deterministic_fallback
}

// Sealed is the report being sealed with the closure.
type Sealed struct {
	JSON   []byte // the full report as the investigator saw it
	Score  int
	Band   string
	Source string
}

const (
	cypherClose = `
		MATCH (c:Claim {claim_id:$claim_id})
		MERGE (ic:InvestigationCase {case_id:$case_id})
		WITH c, ic WHERE coalesce(ic.status, '') <> 'closed'
		  SET ic.disposition=$disposition, ic.notes=$reason, ic.timestamp=$ts, ic.source=$src,
		      ic.status='closed', ic.outcome=$outcome, ic.closed_at=$ts, ic.sealed_report=$report,
		      ic.report_score=$score, ic.report_band=$band, ic.report_source=$rsrc
		MERGE (ic)-[r:INVESTIGATES]->(c)
		  SET r.source=$src, r.evidence_id=$evid, r.recorded_at=$ts
		RETURN ic.case_id AS case_id`

	cypherClosure = `
		MATCH (:Claim {claim_id:$claim_id})<-[:INVESTIGATES]-(ic:InvestigationCase)
		WHERE ic.status = 'closed'
		RETURN ic.case_id AS case_id, ic.outcome AS outcome, ic.disposition AS disposition, ic.notes AS reason,
		       ic.closed_at AS closed_at, ic.report_score AS score, ic.report_band AS band, ic.report_source AS rsrc
		LIMIT 1`

	cypherSealed = `
		MATCH (:Claim {claim_id:$claim_id})<-[:INVESTIGATES]-(ic:InvestigationCase)
		WHERE ic.status = 'closed'
		RETURN ic.sealed_report AS report LIMIT 1`
)

// Close records the investigator's final decision with the sealed report and closes the
// investigation. The reason is required. It returns ErrClosed when the case is already closed.
func (r *Recorder) Close(claimID, outcome, reason string, sealed Sealed) (*Closure, error) {
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("%w: invalid claim_id %q", ErrInvalid, claimID)
	}
	disp := OutcomeDisposition(outcome)
	if disp == "" {
		return nil, fmt.Errorf("%w: outcome must be %s or %s", ErrInvalid, OutcomeApproved, OutcomeRejected)
	}
	reason, err := cleanNotes(reason)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required to close an investigation", ErrInvalid)
	}
	if len(sealed.JSON) == 0 || len(sealed.JSON) > MaxSealedReportBytes {
		return nil, fmt.Errorf("%w: the report to seal is missing or too large", ErrInvalid)
	}

	caseID := CaseID(claimID)
	_, status, err := r.lookup(claimID, caseID)
	if err != nil {
		return nil, err
	}
	if status == StatusClosed {
		return nil, ErrClosed
	}
	ts := r.Now().UTC().Format(time.RFC3339)
	res, err := r.Graph.Query(cypherClose, map[string]any{
		"claim_id": claimID, "case_id": caseID, "disposition": disp, "reason": reason, "ts": ts,
		"src": sourceTag, "evid": "EVID-" + caseID, "outcome": outcome, "report": string(sealed.JSON),
		"score": sealed.Score, "band": sealed.Band, "rsrc": sealed.Source,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("close investigation: %w", err)
	}
	if !res.Next() {
		return nil, ErrClosed // closed by a concurrent request
	}
	return &Closure{CaseID: caseID, ClaimID: claimID, Outcome: outcome, Disposition: disp, Reason: reason,
		ClosedAt: ts, ReportScore: int64(sealed.Score), ReportBand: sealed.Band, ReportSource: sealed.Source}, nil
}

// ClosureOf returns the closed investigation of a claim, or nil when it is open. It includes
// the investigator's reason, so it is for the human UI only, never for the model.
func ClosureOf(g *falkordb.Graph, claimID string) (*Closure, error) {
	if !ingest.ValidID(claimID) {
		return nil, fmt.Errorf("%w: invalid claim_id %q", ErrInvalid, claimID)
	}
	res, err := g.ROQuery(cypherClosure, map[string]any{"claim_id": claimID}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return nil, nil
		}
		return nil, fmt.Errorf("closure: %w", err)
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
	c := &Closure{CaseID: str("case_id"), ClaimID: claimID, Outcome: str("outcome"), Disposition: str("disposition"),
		Reason: str("reason"), ClosedAt: str("closed_at"), ReportBand: str("band"), ReportSource: str("rsrc")}
	if v, _ := rec.Get("score"); v != nil {
		c.ReportScore, _ = v.(int64)
	}
	return c, nil
}

// IsClosed reports whether the claim's investigation is closed.
func IsClosed(g *falkordb.Graph, claimID string) (bool, error) {
	c, err := ClosureOf(g, claimID)
	return c != nil, err
}

// SealedReport returns the sealed report JSON of a closed investigation ("" when open).
func SealedReport(g *falkordb.Graph, claimID string) (string, error) {
	if !ingest.ValidID(claimID) {
		return "", fmt.Errorf("%w: invalid claim_id %q", ErrInvalid, claimID)
	}
	res, err := g.ROQuery(cypherSealed, map[string]any{"claim_id": claimID}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "empty key") {
			return "", nil
		}
		return "", fmt.Errorf("sealed report: %w", err)
	}
	if !res.Next() {
		return "", nil
	}
	v, _ := res.Record().Get("report")
	s, _ := v.(string)
	return s, nil
}
