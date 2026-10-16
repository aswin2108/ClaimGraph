package api

import (
	"claimgraph/internal/actions"
	"net/http"
	"strconv"
	"strings"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/claims"
	"claimgraph/internal/disposition"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"
	"claimgraph/internal/scoring"
)

// GET /healthz — confirms the API can reach FalkorDB.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := claims.List(s.g, 1); err != nil {
		s.logger.Printf("health check failed: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GET /api/meta — the fixed vocabularies the UI needs for its controls.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"counterfactual_categories": investigate.ScoringCategories,
		"dispositions":              []string{disposition.ConfirmedFraud, disposition.NotSubstantiated, disposition.NeedsInfo},
		"max_hops":                  investigate.MaxHopsLimit,
		"disclaimer":                scoring.Disclaimer,
		"llm_enabled":               s.agent.Provider != nil,
		"agent_budget":              s.agent.Budget.Status(),
		"graph":                     s.g.Id,
		"demo_reset_available":      s.demoResetAvailable(),
	})
}

// POST /api/fnol — validate and idempotently ingest a synthetic FNOL.
// 201 when a new claim was created, 200 when an existing claim was updated.
func (s *Server) handleSubmitFNOL(w http.ResponseWriter, r *http.Request) {
	var f ingest.FNOL
	if s.handle(w, decodeJSON(w, r, &f)) {
		return
	}
	res, err := ingest.Ingest(s.g, &f)
	if s.handle(w, err) {
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	// Intake ring alert: does the new claim join a ring? The FNOL is already stored; a failed
	// ring lookup is logged and never fails the submission.
	alert, err := graphalgo.AlertFor(s.g, res.ClaimID)
	if err != nil {
		s.logger.Printf("ring alert for %s: %v", res.ClaimID, err)
	}
	writeJSON(w, status, struct {
		*ingest.Result
		RingAlert *graphalgo.RingAlert `json:"ring_alert,omitempty"`
	}{res, alert})
}

// GET /api/claims?limit=N&offset=M&q=TEXT — the claim queue, newest filings first, with the
// total count; q keeps claims whose id contains TEXT.
func (s *Server) handleListClaims(w http.ResponseWriter, r *http.Request) {
	intParam := func(name string, lo, hi int) (int, bool) {
		v := r.URL.Query().Get(name)
		if v == "" {
			return 0, true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			s.handle(w, badRequest(name+" must be an integer between "+strconv.Itoa(lo)+" and "+strconv.Itoa(hi)))
			return 0, false
		}
		return n, true
	}
	limit, ok := intParam("limit", 1, claims.MaxListLimit)
	if !ok {
		return
	}
	offset, ok := intParam("offset", 0, claims.MaxListOffset)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) != "" && (len(q) > claims.MaxSearchLength || !ingest.ValidID(strings.TrimSpace(q))) {
		s.handle(w, badRequest("q must be up to 64 letters, digits, '-' or '_'"))
		return
	}
	page, err := claims.ListPage(s.g, limit, offset, q)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// GET /api/claims/{id} — claim detail with ordinary policy-check status.
func (s *Server) handleGetClaim(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	c, err := claims.Get(s.g, id)
	if s.handle(w, err) {
		return
	}
	// The latest recorded outcome with its date and notes, so an investigator returning to the
	// claim sees the earlier decision. Added here, not in claims.Get: the agent's GetClaim tool
	// is built on claims.Get and must never receive free-text notes.
	decision, err := disposition.Latest(s.g, id)
	if err != nil {
		s.logger.Printf("latest decision for %s: %v", id, err)
		decision = nil
	}
	writeJSON(w, http.StatusOK, struct {
		*claims.Claim
		Decision *disposition.Case `json:"recorded_decision,omitempty"`
	}{c, decision})
}

// GET /api/claims/{id}/investigation?hops=N — related claims, evidence paths,
// deterministic score, prior outcomes and uncertainty notes.
func (s *Server) handleInvestigation(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	hops, err := hopsParam(r)
	if s.handle(w, err) {
		return
	}
	rep, err := scoring.Analyze(s.g, id, hops)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, withActions(s, rep))
}

// investigationResponse is the report plus the recommended verification plan (the report's
// fields stay at the top level, so existing clients are unaffected).
type investigationResponse struct {
	*scoring.Report
	RecommendedActions *actions.Plan        `json:"recommended_actions"`
	RingAlert          *graphalgo.RingAlert `json:"ring_alert,omitempty"`
}

func withActions(s *Server, rep *scoring.Report) investigationResponse {
	ring, err := graphalgo.RingSummaryOf(s.g, rep.TargetClaim)
	if err != nil {
		s.logger.Printf("ring summary for %s: %v", rep.TargetClaim, err)
		ring = nil
	}
	return investigationResponse{Report: rep, RecommendedActions: actions.Recommend(rep, ring), RingAlert: graphalgo.AlertFromRing(ring)}
}

type counterfactualRequest struct {
	Exclude string `json:"exclude"`
	Hops    int    `json:"hops,omitempty"`
}

// POST /api/claims/{id}/counterfactual — recompute with one evidence category
// ignored. Read-only: the graph is never modified.
func (s *Server) handleCounterfactual(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	var req counterfactualRequest
	if s.handle(w, decodeJSON(w, r, &req)) {
		return
	}
	if !investigate.ValidCategory(req.Exclude) {
		s.handle(w, badRequest("exclude must be one of the categories listed by GET /api/meta"))
		return
	}
	if req.Hops < 0 || req.Hops > investigate.MaxHopsLimit {
		s.handle(w, badRequest("hops must be between 1 and 3"))
		return
	}
	cf, err := scoring.RunCounterfactual(s.g, id, req.Hops, req.Exclude)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, cf)
}

type dispositionRequest struct {
	Disposition string `json:"disposition"`
	Notes       string `json:"notes,omitempty"`
}

// POST /api/claims/{id}/disposition — record a simulated investigator decision.
// 201 for a new case, 200 when an existing case was updated.
func (s *Server) handleDisposition(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	var req dispositionRequest
	if s.handle(w, decodeJSON(w, r, &req)) {
		return
	}
	if !disposition.ValidDisposition(req.Disposition) {
		s.handle(w, badRequest("disposition must be one of confirmed_fraud, not_substantiated, needs_info"))
		return
	}
	c, err := s.rec.Record(id, req.Disposition, req.Notes)
	if s.handle(w, err) {
		return
	}
	status := http.StatusOK
	if c.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, c)
}

// GET /api/claims/{id}/graph?hops=N&exclude=category — Cytoscape-ready nodes and
// edges for the target claim, its connected claims and the evidence between them.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	hops, err := hopsParam(r)
	if s.handle(w, err) {
		return
	}
	exclude := r.URL.Query().Get("exclude")
	if exclude != "" && !investigate.ValidCategory(exclude) {
		s.handle(w, badRequest("exclude must be one of the categories listed by GET /api/meta"))
		return
	}
	res, err := investigate.FindRelatedClaimsExcluding(s.g, id, hops, exclude)
	if s.handle(w, err) {
		return
	}
	ids := []string{res.TargetClaim}
	for _, rc := range res.RelatedClaims {
		ids = append(ids, rc.ClaimID)
	}
	tows, err := claims.TowProviders(s.g, ids)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, BuildGraphView(res, tows))
}

type agentRequest struct {
	Hops                 int             `json:"hops,omitempty"`
	InvestigatorDecision *agent.Decision `json:"investigator_decision,omitempty"`
}

// POST /api/claims/{id}/agent - run the bounded investigation agent. The body is
// optional. The deterministic report is always returned; the explanation comes from
// the LLM only if it passes validation, otherwise from the deterministic fallback.
// "investigator_decision" relays a human's disposition; the agent cannot originate one.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	// Always require a JSON content type, even with an empty body. A browser can send a
	// body-less cross-site POST without a CORS preflight, which would let any web page
	// spend LLM budget; a JSON content type forces a preflight that CORS then blocks.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		s.handle(w, &statusError{http.StatusUnsupportedMediaType, "Content-Type must be application/json"})
		return
	}
	var req agentRequest
	if r.ContentLength != 0 {
		if s.handle(w, decodeJSON(w, r, &req)) {
			return
		}
	}
	if req.Hops < 0 || req.Hops > investigate.MaxHopsLimit {
		s.handle(w, badRequest("hops must be between 1 and 3"))
		return
	}
	// An agent run can outlast the server-wide write timeout.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(agent.RunTimeout + 15*time.Second)); err != nil {
		s.logger.Printf("cannot extend the write deadline for an agent run (long runs may be cut off): %v", err)
	}

	res, err := s.agent.Run(r.Context(), agent.Request{ClaimID: id, Hops: req.Hops, Decision: req.InvestigatorDecision})
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, res)
}
