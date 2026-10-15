package api

import (
	"net/http"
	"strconv"

	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
)

// GET /api/claims/{id}/ring — the claim's strong-evidence ring (algo.WCC) and its
// minimum evidence backbone (algo.MSF). Context only; never changes the score.
func (s *Server) handleRing(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	ring, err := graphalgo.RingOf(s.g, id)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, ring)
}

// GET /api/claims/{id}/link/{other} — the strongest evidence chain between two claims
// (algo.SPpaths weighted by evidence cost).
func (s *Server) handleStrongestLink(w http.ResponseWriter, r *http.Request) {
	id, err := claimID(r)
	if s.handle(w, err) {
		return
	}
	other := r.PathValue("other")
	if !ingest.ValidID(other) {
		s.handle(w, badRequest("invalid claim id"))
		return
	}
	if other == id {
		s.handle(w, badRequest("the two claims must be different"))
		return
	}
	link, err := graphalgo.FindStrongestLink(s.g, id, other)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// GET /api/rings?min_size=3&limit=20 — rings across the whole book (algo.WCC), in review order.
func (s *Server) handleRings(w http.ResponseWriter, r *http.Request) {
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
	minSize, ok := intParam("min_size", 2, graphalgo.MaxRingClaims)
	if !ok {
		return
	}
	limit, ok := intParam("limit", 1, graphalgo.MaxListRings)
	if !ok {
		return
	}
	list, err := graphalgo.ListRings(s.g, minSize, limit)
	if s.handle(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, list)
}
