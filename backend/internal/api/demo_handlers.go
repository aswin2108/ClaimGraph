package api

import (
	"errors"
	"net/http"
	"time"

	"claimgraph/internal/demo"
)

// resetCooldown stops a double click (or a script) from restoring the graph in a loop.
const resetCooldown = 5 * time.Second

type resetRequest struct {
	Confirm string `json:"confirm"` // must be "reset"
}

func (s *Server) demoResetAvailable() bool {
	if s.demoDB == nil {
		return false
	}
	ok, err := demo.HasSnapshot(s.demoDB, s.g.Id)
	return err == nil && ok
}

// POST /api/demo/reset {"confirm":"reset"} — restore the graph's pristine snapshot, so the
// next visitor starts from the same book. Discards every claim submitted and every outcome
// recorded since the snapshot. Disabled unless the server enabled it (DEMO_RESET).
func (s *Server) handleDemoReset(w http.ResponseWriter, r *http.Request) {
	if s.demoDB == nil {
		s.handle(w, &statusError{http.StatusNotFound, "demo reset is disabled on this server"})
		return
	}
	var req resetRequest
	if s.handle(w, decodeJSON(w, r, &req)) {
		return
	}
	if req.Confirm != "reset" {
		s.handle(w, badRequest(`send {"confirm":"reset"} to restore the demo graph`))
		return
	}
	if !s.resetMu.TryLock() {
		s.handle(w, &statusError{http.StatusConflict, "a reset is already running"})
		return
	}
	defer s.resetMu.Unlock()
	if wait := resetCooldown - time.Since(s.lastReset); wait > 0 {
		s.handle(w, &statusError{http.StatusTooManyRequests, "the demo was just reset; try again in a few seconds"})
		return
	}
	res, err := demo.Restore(s.demoDB, s.g.Id)
	if errors.Is(err, demo.ErrNoSnapshot) {
		s.handle(w, &statusError{http.StatusConflict, err.Error()})
		return
	}
	if s.handle(w, err) {
		return
	}
	s.lastReset = time.Now()
	s.reports.clear() // reports of the replaced graph can no longer be sealed
	s.logger.Printf("demo reset: graph %s restored from %s (%d claims, %d ms)", res.Graph, res.Snapshot, res.Claims, res.DurationMs)
	writeJSON(w, http.StatusOK, res)
}
