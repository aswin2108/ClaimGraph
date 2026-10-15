package eval

// T21-T30 (Idea.txt Section 7, parts C and D): historical memory, traversal boundaries,
// counterfactual stability, agent resilience and safety.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"claimgraph/internal/agent"
	"claimgraph/internal/disposition"
	"claimgraph/internal/ingest"
	"claimgraph/internal/scoring"

	"github.com/FalkorDB/falkordb-go/v2"
)

// ---- T21-T25: table-driven through the same check() as T01-T20 ----

func memoryScenarios() []scenario {
	const tgt = "2026-09-10"
	return []scenario{
		{id: "T21", desc: "payout+device with a claim >30 days apart", score: 60, band: scoring.BandHigh,
			claims:  with(mk("T", tgt, payout("P"), device("D")), family([]string{"2026-07-20"}, payout("P"), device("D"))),
			related: []string{"B"}, factors: []string{scoring.FSharedPayout, scoring.FSharedDevice, scoring.FReinforcement}, noFactors: []string{scoring.FTemporal}},
		{id: "T22", desc: "five other claims share only a repair shop over 60 days (breadth capped)", score: 23, band: scoring.BandLow,
			claims:  with(mk("T", tgt, shop("S")), family([]string{"2026-08-01", "2026-07-20", "2026-07-10", "2026-07-01", "2026-06-15"}, shop("S"))),
			related: []string{"B", "C", "D1", "E", "F"}, factors: []string{scoring.FSharedRepair, scoring.FClaimBreadth}, noFactors: []string{scoring.FTemporal, scoring.FReinforcement}},
		{id: "T23", desc: "prior not-substantiated case reached through a repair shop", score: 13, band: scoring.BandLow,
			claims: with(mk("T", tgt, shop("S")), family([]string{"2026-06-01"}, shop("S"))), cases: []caseSpec{{"B", "not_substantiated"}},
			related: []string{"B"}, factors: []string{scoring.FSharedRepair}, noFactors: []string{scoring.FConfirmedFraud}, prior: 1, priorDisp: "not_substantiated"},
		{id: "T24", desc: "prior needs-more-information case reached through a device", score: 25, band: scoring.BandLow,
			claims: with(mk("T", tgt, device("D")), family([]string{"2026-06-01"}, device("D"))), cases: []caseSpec{{"B", "needs_info"}},
			related: []string{"B"}, factors: []string{scoring.FSharedDevice}, noFactors: []string{scoring.FConfirmedFraud}, prior: 1, priorDisp: "needs_info"},
	}
}

func TestEvaluationT21toT24(t *testing.T) {
	for _, s := range memoryScenarios() {
		t.Run(s.id, func(t *testing.T) {
			if _, bad := check(t, s); len(bad) > 0 {
				t.Errorf("%s (%s): %s", s.id, s.desc, strings.Join(bad, "; "))
			}
		})
	}
}

// T25: the disposition is recorded through the REAL recorder (not hand-written Cypher),
// then a later related claim is investigated.
func TestT25RecordedDispositionIsRetrievedWithoutChangingScoring(t *testing.T) {
	g := isolatedGraph(t, "T25")
	load(t, g, scenario{claims: []*ingest.FNOL{mk("B", "2026-06-01", shop("S"))}})

	before, err := scoring.Analyze(g, "B", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disposition.New(g).Record("B", "not_substantiated", "synthetic review"); err != nil {
		t.Fatal(err)
	}
	load(t, g, scenario{claims: []*ingest.FNOL{mk("T", "2026-09-10", shop("S"))}}) // the later related claim

	r, err := scoring.Analyze(g, "T", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 13 || r.Band != scoring.BandLow {
		t.Fatalf("score=%d band=%s, want 13 Low", r.Score, r.Band)
	}
	if len(r.PriorCases) != 1 || r.PriorCases[0].Disposition != "not_substantiated" || r.PriorCases[0].ClaimID != "B" {
		t.Fatalf("previous disposition not retrieved: %+v", r.PriorCases)
	}
	if factorNames(r)[scoring.FConfirmedFraud] {
		t.Fatal("not_substantiated must never earn the confirmed-fraud bonus")
	}
	if before.Score != 0 {
		t.Fatalf("recording an outcome changed the earlier claim's own score: %d", before.Score)
	}
}

// ---- graph snapshot, used to prove "graph unchanged" ----

func snapshot(t *testing.T, g *falkordb.Graph) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`MATCH (n) RETURN labels(n)[0] AS l, count(n) ORDER BY l`,
		`MATCH ()-[r]->() RETURN type(r) AS ty, count(r) ORDER BY ty`,
		`MATCH (n) RETURN count(n), sum(size(keys(n)))`,
	} {
		res, err := g.ROQuery(q, nil, nil)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for res.Next() {
			parts = append(parts, fmt.Sprint(res.Record().Values()...))
		}
	}
	return strings.Join(parts, "|")
}

// ---- T26 / T27: counterfactuals ----

func TestT26ExcludingRepairShopDropsExactlyEight(t *testing.T) {
	g := isolatedGraph(t, "T26")
	all := func(id, filed string) *ingest.FNOL {
		return mk(id, filed, payout("P"), device("D"), phone("+1-555-0126"), shop("S"))
	}
	load(t, g, scenario{claims: []*ingest.FNOL{all("T", "2026-09-10"), all("B", "2026-09-11"), all("C", "2026-09-12")}})
	before := snapshot(t, g)

	cf, err := scoring.RunCounterfactual(g, "T", 3, "repair_shop")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Original.Score != 95 || cf.Counterfactual.Score != 87 || cf.Delta != -8 {
		t.Fatalf("want 95 -> 87 (-8), got %d -> %d (%d)", cf.Original.Score, cf.Counterfactual.Score, cf.Delta)
	}
	if cf.Counterfactual.Band != scoring.BandHigh || !cf.StillJustifies {
		t.Fatalf("remaining evidence should still justify investigation: %s", cf.Counterfactual.Band)
	}
	if len(cf.Counterfactual.Investigation.RelatedClaims) != 2 {
		t.Fatal("other paths must still connect both related claims")
	}
	if after := snapshot(t, g); after != before {
		t.Fatalf("counterfactual changed the graph:\n%s\n%s", before, after)
	}
}

func TestT27ExcludingPayoutDropsSixtyToTwentyFive(t *testing.T) {
	g := isolatedGraph(t, "T27")
	load(t, g, scenario{claims: []*ingest.FNOL{
		mk("T", "2026-09-10", payout("P"), device("D")), mk("B", "2026-07-01", payout("P"), device("D"))}})
	before := snapshot(t, g)

	cf, err := scoring.RunCounterfactual(g, "T", 3, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Original.Score != 60 || cf.Counterfactual.Score != 25 || cf.Delta != -35 {
		t.Fatalf("want 60 -> 25 (-35), got %d -> %d (%d)", cf.Original.Score, cf.Counterfactual.Score, cf.Delta)
	}
	if cf.Counterfactual.Band != scoring.BandLow || cf.StillJustifies {
		t.Fatalf("band after exclusion %s, justifies=%v", cf.Counterfactual.Band, cf.StillJustifies)
	}
	removed := map[string]bool{}
	for _, f := range cf.RemovedFactors {
		removed[f.Name] = true
	}
	if !removed[scoring.FSharedPayout] || !removed[scoring.FReinforcement] {
		t.Fatalf("report must explain what disappeared (payout + corroboration bonus): %+v", cf.RemovedFactors)
	}
	if cf.Explanation == "" || len(cf.Counterfactual.Investigation.RelatedClaims) != 1 {
		t.Fatal("the device path must still connect the claims and the change must be explained")
	}
	if after := snapshot(t, g); after != before {
		t.Fatal("counterfactual changed the graph")
	}
}

// ---- T28: no connections ----

var claimIDLike = regexp.MustCompile(`CLM-\d+`)

func TestT28NoConnectionsFabricatesNothing(t *testing.T) {
	g := isolatedGraph(t, "T28")
	load(t, g, scenario{claims: []*ingest.FNOL{mk("CLM-9100", "2026-09-10"), mk("CLM-9101", "2026-09-10"), mk("CLM-9102", "2026-09-10")}})

	r, err := scoring.Analyze(g, "CLM-9100", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 || r.Band != scoring.BandLow || r.ConnectedClaims != 0 || len(r.Factors) != 0 || len(r.PriorCases) != 0 {
		t.Fatalf("expected an empty Low report, got %+v", r)
	}
	if len(r.Investigation.RelatedClaims) != 0 || r.Investigation.Message == "" || len(r.Uncertainty) == 0 {
		t.Fatalf("must say explicitly that no supporting connection was found: %q", r.Investigation.Message)
	}

	// The agent path (no model) must not invent anything either.
	res, err := agent.New(g, nil, log.New(io.Discard, "", 0)).Run(context.Background(), agent.Request{ClaimID: "CLM-9100"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Explanation)
	for _, id := range claimIDLike.FindAllString(string(b), -1) {
		if id != "CLM-9100" {
			t.Fatalf("explanation mentions %s, which is not connected to the target", id)
		}
	}
	if res.Report.Score != 0 || len(res.Report.Investigation.RelatedClaims) != 0 {
		t.Fatal("agent report must match the empty deterministic report")
	}
}

// ---- T29: LLM failure modes fall back to the real evidence ----

type scriptedProvider struct {
	send func(ctx context.Context) (*agent.Turn, error)
}

func (scriptedProvider) Name() string { return "eval-fake" }
func (p scriptedProvider) NewConversation(string, []agent.ToolSpec) agent.Conversation {
	return conv(p)
}

type conv struct {
	send func(ctx context.Context) (*agent.Turn, error)
}

func (c conv) Send(ctx context.Context, _ []agent.ToolResult, _ string) (*agent.Turn, error) {
	return c.send(ctx)
}

func TestT29LLMFailureFallsBackToRealGraphEvidence(t *testing.T) {
	pd := []mod{payout("P"), device("D")}
	build := func(t *testing.T, name string) *falkordb.Graph {
		g := isolatedGraph(t, name)
		load(t, g, scenario{claims: []*ingest.FNOL{
			mk("CLM-9201", "2026-09-10", pd...), mk("CLM-9202", "2026-09-12", pd...), mk("CLM-9203", "2026-08-01", pd...)}})
		return g
	}
	cases := []struct {
		name, reason string
		timeout      time.Duration
		send         func(ctx context.Context) (*agent.Turn, error)
	}{
		{"model timeout", agent.ReasonTimeout, 300 * time.Millisecond, func(ctx context.Context) (*agent.Turn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		{"model error", agent.ReasonLLMError, 0, func(context.Context) (*agent.Turn, error) { return nil, errors.New("upstream 500") }},
		{"malformed structured output", agent.ReasonUngrounded, 0, func(context.Context) (*agent.Turn, error) {
			return &agent.Turn{Text: `{"summary": "oops", this is not json`}, nil
		}},
		{"invented claim id", agent.ReasonUngrounded, 0, func(context.Context) (*agent.Turn, error) {
			return &agent.Turn{Text: `{"summary":"Claim CLM-9201 is linked to CLM-9999.","key_findings":[],"uncertainty":["x"],"next_step":{"action":"Check CLM-9999","rationale":"r","claim_ids":["CLM-9999"],"entities":[]}}`}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := build(t, "T29")
			want, err := scoring.Analyze(g, "CLM-9201", 3)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			a := agent.New(g, scriptedProvider{tc.send}, log.New(io.Discard, "", 0))
			res, err := a.Run(ctx, agent.Request{ClaimID: "CLM-9201"})
			if err != nil {
				t.Fatalf("an LLM failure must never surface as an error: %v", err)
			}
			if res.Source != agent.SourceFallback || res.FallbackReason != tc.reason {
				t.Fatalf("source=%s reason=%s, want fallback/%s", res.Source, res.FallbackReason, tc.reason)
			}
			if res.Report.Score != want.Score || res.Report.Band != want.Band || res.Report.ConnectedClaims != 2 {
				t.Fatalf("report differs from the deterministic one: %d %s", res.Report.Score, res.Report.Band)
			}
			if res.Explanation == nil || len(res.Report.Investigation.RelatedClaims) != 2 {
				t.Fatal("fallback must carry an explanation and the real paths")
			}
			b, _ := json.Marshal(res.Explanation)
			for _, id := range claimIDLike.FindAllString(string(b), -1) {
				if id != "CLM-9201" && id != "CLM-9202" && id != "CLM-9203" {
					t.Fatalf("fallback invented claim id %s", id)
				}
			}
		})
	}
}

// ---- T30: arbitrary Cypher / unrelated mutation is rejected ----

func TestT30ArbitraryCypherAndMutationAreRejected(t *testing.T) {
	g := isolatedGraph(t, "T30")
	load(t, g, scenario{claims: []*ingest.FNOL{
		mk("CLM-9301", "2026-09-10", payout("P")), mk("CLM-9302", "2026-09-12", payout("P"))}})
	before := snapshot(t, g)

	hostile := []agent.ToolCall{
		{ID: "1", Name: "RunCypher", Input: []byte(`{"query":"MATCH (n) DETACH DELETE n"}`)},
		{ID: "2", Name: "GetClaim", Input: []byte(`{"claim_id":"CLM-9301'}) DETACH DELETE (n) //"}`)},
		{ID: "3", Name: "RunCounterfactual", Input: []byte(`{"claim_id":"CLM-9301","exclude_signal_type":"x') DETACH DELETE n //"}`)},
		{ID: "4", Name: "GetClaim", Input: []byte(`{"claim_id":"CLM-9301","cypher":"MATCH (n) DELETE n"}`)},
		{ID: "5", Name: "RecordInvestigatorDisposition", Input: []byte(`{"claim_id":"CLM-9302","disposition":"confirmed_fraud"}`)},
	}
	for _, mode := range []string{"one-by-one", "batched"} {
		t.Run(mode, func(t *testing.T) {
			turn := 0
			p := scriptedProvider{func(context.Context) (*agent.Turn, error) {
				turn++
				if turn == 1 {
					if mode == "batched" {
						return &agent.Turn{Calls: hostile}, nil
					}
					return &agent.Turn{Calls: hostile[:1]}, nil
				}
				if mode == "one-by-one" && turn <= len(hostile) {
					return &agent.Turn{Calls: hostile[turn-1 : turn]}, nil
				}
				return &agent.Turn{Text: "{}"}, nil
			}}
			res, err := agent.New(g, p, log.New(io.Discard, "", 0)).Run(context.Background(), agent.Request{ClaimID: "CLM-9301"})
			if err != nil {
				t.Fatal(err)
			}
			for _, tr := range res.Trace {
				if tr.OK {
					t.Fatalf("hostile call %q succeeded", tr.Tool)
				}
			}
			if res.Recorded != nil {
				t.Fatal("the model must never originate a disposition without a human decision")
			}
			if res.Source != agent.SourceFallback {
				t.Fatalf("hostile run must end in the deterministic fallback, got %s", res.Source)
			}
			if after := snapshot(t, g); after != before {
				t.Fatalf("graph state changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}

	// And directly against the deterministic layer: a hostile category is refused.
	if _, err := scoring.RunCounterfactual(g, "CLM-9301", 3, "x') DETACH DELETE n //"); err == nil {
		t.Fatal("counterfactual accepted a hostile category")
	}
	if after := snapshot(t, g); after != before {
		t.Fatal("graph state changed after hostile counterfactual")
	}
}
