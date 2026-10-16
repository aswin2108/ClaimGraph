package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/disposition"
)

// Closing an investigation seals the report the investigator read. The agent is stateless,
// so the API keeps the most recent reports for a while and the browser closes by run_id;
// the browser never sends report content, so a sealed report cannot be edited on the way.

const (
	maxKeptReports = 64
	reportTTL      = 2 * time.Hour
)

type keptReport struct {
	claimID string
	sealed  disposition.Sealed
	at      time.Time
}

type reportStore struct {
	mu   sync.Mutex
	runs map[string]keptReport
	now  func() time.Time
}

func newReportStore() *reportStore {
	return &reportStore{runs: map[string]keptReport{}, now: time.Now}
}

// put gives res a run id and keeps its JSON for sealing.
func (rs *reportStore) put(claimID string, res *agent.Result) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	res.RunID = hex.EncodeToString(b[:])
	body, err := json.Marshal(res)
	if err != nil {
		res.RunID = ""
		return err
	}
	sealed := disposition.Sealed{JSON: body, Source: res.Source}
	if res.Report != nil {
		sealed.Score, sealed.Band = res.Report.Score, res.Report.Band
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.expire()
	for len(rs.runs) >= maxKeptReports { // drop the oldest
		var oldest string
		for id, k := range rs.runs {
			if oldest == "" || k.at.Before(rs.runs[oldest].at) {
				oldest = id
			}
		}
		delete(rs.runs, oldest)
	}
	rs.runs[res.RunID] = keptReport{claimID: claimID, sealed: sealed, at: rs.now()}
	return nil
}

// get returns the kept report of runID if it belongs to claimID and has not expired.
func (rs *reportStore) get(runID, claimID string) (disposition.Sealed, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.expire()
	k, ok := rs.runs[runID]
	if !ok || k.claimID != claimID {
		return disposition.Sealed{}, false
	}
	return k.sealed, true
}

func (rs *reportStore) drop(runID string) {
	rs.mu.Lock()
	delete(rs.runs, runID)
	rs.mu.Unlock()
}

// clear forgets every kept report (after a demo reset the graph they describe is gone).
func (rs *reportStore) clear() {
	rs.mu.Lock()
	rs.runs = map[string]keptReport{}
	rs.mu.Unlock()
}

func (rs *reportStore) expire() {
	for id, k := range rs.runs {
		if rs.now().Sub(k.at) > reportTTL {
			delete(rs.runs, id)
		}
	}
}

type closeRequest struct {
	RunID   string `json:"run_id"`
	Outcome string `json:"outcome"` // approved | rejected
	Reason  string `json:"reason"`
}

// POST /api/claims/{id}/close {"run_id":"...","outcome":"approved|rejected","reason":"..."}
// The investigator's final decision after reading the report of run_id. 201 with the closure;
// 409 when already closed or when the report is no longer kept (run the agent again).
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	var req closeRequest
	if s.handle(w, decodeJSON(w, r, &req)) {
		return
	}
	if disposition.OutcomeDisposition(req.Outcome) == "" {
		s.handle(w, badRequest("outcome must be approved or rejected"))
		return
	}
	if req.RunID == "" {
		s.handle(w, badRequest("run_id is required: close the investigation from a generated report"))
		return
	}
	sealed, ok := s.reports.get(req.RunID, id)
	if !ok {
		s.handle(w, &statusError{http.StatusConflict, "this report is no longer available to seal; run the investigation agent again"})
		return
	}
	c, err := s.rec.Close(id, req.Outcome, req.Reason, sealed)
	if s.handle(w, err) {
		return
	}
	s.reports.drop(req.RunID)
	writeJSON(w, http.StatusCreated, c)
}

// GET /api/claims/{id}/sealed-report: the report sealed when the investigation was closed,
// exactly as the investigator read it. 404 when the investigation is not closed.
func (s *Server) handleSealedReport(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	body, err := disposition.SealedReport(s.g, id)
	if s.handle(w, err) {
		return
	}
	if body == "" {
		s.handle(w, &statusError{http.StatusNotFound, "this investigation is not closed"})
		return
	}
	if !json.Valid([]byte(body)) {
		s.handle(w, errors.New("sealed report is not valid JSON"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
