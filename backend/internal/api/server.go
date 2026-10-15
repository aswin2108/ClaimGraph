// Package api exposes the investigation pipeline over HTTP/JSON.
//
// It is a thin layer: every decision (validation, scoring, counterfactuals,
// graph access) lives in the internal packages. This layer only decodes requests,
// maps errors to status codes and encodes responses. It has no authentication and
// is intended for the local demo only.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/disposition"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/ingest"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Server holds the dependencies shared by all handlers.
type Server struct {
	g          *falkordb.Graph
	rec        *disposition.Recorder
	agent      *agent.Agent
	corsOrigin string          // the single browser origin allowed to call the API ("" = none)
	hosts      map[string]bool // Host header values accepted (DNS-rebinding defence)
	logger     *log.Logger
	mux        *http.ServeMux
}

// New builds a Server over the given graph. corsOrigin is the frontend origin
// (e.g. http://localhost:5173); only that origin receives CORS headers.
func New(g *falkordb.Graph, corsOrigin string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{g: g, rec: disposition.New(g), agent: agent.New(g, nil, logger), corsOrigin: corsOrigin, logger: logger, mux: http.NewServeMux(),
		hosts: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}}
	s.routes()
	return s
}

// SetAllowedHosts adds Host header values (without port) the API will answer to, in
// addition to localhost, 127.0.0.1 and ::1. Needed when serving from another hostname.
func (s *Server) SetAllowedHosts(hosts []string) {
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.hosts[h] = true
		}
	}
}

// SetAgent replaces the investigation agent (e.g. with one backed by an LLM provider).
// Without it, agent runs return the deterministic explanation.
func (s *Server) SetAgent(a *agent.Agent) { s.agent = a }

// Handler returns the root handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.logRequests(s.hostGuard(s.securityHeaders(s.cors(s.mux)))))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /api/meta", s.handleMeta)
	s.mux.HandleFunc("POST /api/fnol", s.handleSubmitFNOL)
	s.mux.HandleFunc("GET /api/claims", s.handleListClaims)
	s.mux.HandleFunc("GET /api/claims/{id}", s.handleGetClaim)
	s.mux.HandleFunc("GET /api/claims/{id}/investigation", s.handleInvestigation)
	s.mux.HandleFunc("GET /api/claims/{id}/graph", s.handleGraph)
	s.mux.HandleFunc("POST /api/claims/{id}/counterfactual", s.handleCounterfactual)
	s.mux.HandleFunc("POST /api/claims/{id}/disposition", s.handleDisposition)
	s.mux.HandleFunc("POST /api/claims/{id}/agent", s.handleAgent)
	s.mux.HandleFunc("GET /api/claims/{id}/ring", s.handleRing)
	s.mux.HandleFunc("GET /api/claims/{id}/link/{other}", s.handleStrongestLink)
	s.mux.HandleFunc("GET /api/rings", s.handleRings)
	s.mux.HandleFunc("GET /api/graph/stats", s.handleGraphStats)
}

// ---- response helpers ----

type errorBody struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, problems ...string) {
	writeJSON(w, status, errorBody{Error: msg, Problems: problems})
}

// fail maps domain errors to HTTP statuses. Unexpected errors are logged and
// reported generically so internal details never reach the client.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var ve *ingest.ValidationError
	var br *badRequestError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid FNOL", ve.Problems...)
	case errors.Is(err, investigate.ErrClaimNotFound), errors.Is(err, graphalgo.ErrClaimNotFound):
		writeError(w, http.StatusNotFound, "claim not found")
	case errors.Is(err, graphalgo.ErrOverlayMissing):
		writeError(w, http.StatusServiceUnavailable, graphalgo.ErrOverlayMissing.Error())
	case errors.Is(err, graphalgo.ErrTimeout), errors.Is(err, investigate.ErrTraversalTimeout):
		writeError(w, http.StatusServiceUnavailable, "the graph query took too long; try again or narrow the request")
	case errors.As(err, &br):
		writeError(w, http.StatusBadRequest, br.msg)
	case errors.Is(err, agent.ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), agent.ErrInvalid.Error()+": "))
	case errors.Is(err, agent.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "the agent is busy; try again shortly")
	case errors.Is(err, disposition.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// badRequestError is a client mistake that maps to HTTP 400.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badRequest(msg string) error { return &badRequestError{msg: msg} }

// ---- request helpers ----

// decodeJSON reads a size-limited, strict JSON body into dst.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return &statusError{http.StatusUnsupportedMediaType, "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &statusError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return badRequest("invalid JSON body: " + err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("body must contain a single JSON object")
	}
	return nil
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// claimID validates the {id} path parameter.
func claimID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !ingest.ValidID(id) {
		return "", badRequest("invalid claim id")
	}
	return id, nil
}

// hopsParam parses the optional ?hops=1..3 query parameter (0 = default).
func hopsParam(r *http.Request) (int, error) {
	v := r.URL.Query().Get("hops")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > investigate.MaxHopsLimit {
		return 0, badRequest("hops must be an integer between 1 and 3")
	}
	return n, nil
}

// handle wraps a handler that returns an error so all handlers share error mapping.
func (s *Server) handle(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		writeError(w, se.code, se.msg)
		return true
	}
	s.fail(w, err)
	return true
}

// ---- middleware ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real connection through this wrapper. Without it,
// per-request deadline changes (see handleAgent) silently fail and long agent runs are cut off.
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.logger.Printf("panic: %v", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// hostGuard rejects requests whose Host header is not an allowed name. A web page cannot
// reach this API through a rebound DNS name because the browser sends the attacker's host.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if !s.hosts[host] {
			writeError(w, http.StatusForbidden, "host not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// cors allows exactly one configured origin; every other origin gets no CORS headers.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.corsOrigin != "" && origin == s.corsOrigin {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
