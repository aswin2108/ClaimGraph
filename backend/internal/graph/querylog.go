package graph

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// ExecutedQuery is one Cypher query a feature sent to FalkorDB, kept so the UI can show
// exactly what produced a result ("Show Cypher"). Cypher is the hardcoded template; Params
// are the values bound to it (user input is only ever a parameter, never query text).
type ExecutedQuery struct {
	Purpose    string         `json:"purpose"`
	Cypher     string         `json:"cypher"`
	Params     map[string]any `json:"params,omitempty"`
	Results    int            `json:"results"`             // rows or paths the query returned (-1 = not counted)
	DurationMs int64          `json:"duration_ms"`         // 0 when served from a cached run
	Cached     bool           `json:"cached,omitempty"`    // the answer came from an earlier whole-book run
	TimedOut   bool           `json:"timed_out,omitempty"` // FalkorDB stopped it at the time limit
}

// Record returns an ExecutedQuery for q with its text tidied for display. Call it after the
// query ran; start is when it was sent.
func Record(purpose, q string, params map[string]any, results int, start time.Time) ExecutedQuery {
	return ExecutedQuery{Purpose: purpose, Cypher: Tidy(q), Params: params, Results: results,
		DurationMs: time.Since(start).Milliseconds()}
}

// Tidy removes the Go source indentation from a query template and drops blank lines.
func Tidy(q string) string {
	lines := strings.Split(strings.ReplaceAll(q, "\r\n", "\n"), "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if indent > 0 && len(l) >= indent {
			l = l[indent:]
		}
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return strings.Join(out, "\n")
}

var paramRef = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// MissingParams lists the $parameters the query text uses that have no logged value (tests
// use it to prove the "Show Cypher" view is complete).
func (q ExecutedQuery) MissingParams() []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range paramRef.FindAllStringSubmatch(q.Cypher, -1) {
		if _, ok := q.Params[m[1]]; !ok && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}
