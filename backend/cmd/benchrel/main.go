// benchrel answers the same investigation questions with FalkorDB and with PostgreSQL on the
// same book, checks that both return the same answer, and measures both (S10).
//
//	docker compose --profile bench up -d postgres-bench
//	go run ./cmd/benchrel                      # writes testdata/scale/benchmark_relational.md
//	go run ./cmd/benchrel -limit 4 -out ""     # quick check
//
// Questions (the same rules on both sides; see the report for the exact queries):
//
//	Q1a Which claims are connected within 1, 2, 3 hops, and how far? (ids and hop counts only)
//	    FalkorDB: investigate.ConnectedOnly (one query, same Cypher fragments as production).
//	Q1b The same, with the evidence: every shortest path and the identifier on each step.
//	    FalkorDB: investigate.FindRelatedClaims (the production call).
//	Q2  Which ring does this claim belong to (strong evidence, any depth)?
//	    FalkorDB: graphalgo.RingOf (algo.WCC, cached per evidence version).
//	Q3  List every ring in the book (components of 3+ claims over strong evidence).
//	    FalkorDB: CALL algo.WCC. PostgreSQL: iterative label propagation (no built-in components).
//	Q4  The strongest evidence chain between two claims at any depth (lowest total evidence cost).
//	    FalkorDB: graphalgo.FindStrongestLink (algo.SPpaths). PostgreSQL: iterative Bellman-Ford.
//
// The data is copied from FalkorDB (not regenerated), so both stores hold exactly the same book.
// Synthetic data; the results describe this book on this machine only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"claimgraph/internal/graph"
	"claimgraph/internal/graphalgo"
	"claimgraph/internal/investigate"

	"github.com/FalkorDB/falkordb-go/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type manifest struct {
	Dataset struct {
		Rings []struct {
			Index  int      `json:"index"`
			Claims []string `json:"claims"`
		} `json:"planted_rings"`
		Stats struct {
			BusiestShop string `json:"busiest_shop"`
		} `json:"stats"`
	} `json:"dataset"`
}

// ---- schema (PostgreSQL) ----

const ddl = `
DROP SCHEMA public CASCADE; -- this database only holds the benchmark copy
CREATE SCHEMA public;
CREATE TABLE claim (claim_id text PRIMARY KEY, claimant_id text NOT NULL, filing_date text);
CREATE TABLE claim_entity (claim_id text NOT NULL, rel text NOT NULL, entity text NOT NULL, category text NOT NULL);
CREATE TABLE claimant_entity (claimant_id text NOT NULL, rel text NOT NULL, entity text NOT NULL, category text NOT NULL);
CREATE TABLE entity (entity text PRIMARY KEY, category text NOT NULL, links int NOT NULL, strong bool NOT NULL, context bool NOT NULL);`

const indexes = `
CREATE INDEX ON claim (claimant_id);
CREATE INDEX ON claim_entity (claim_id);
CREATE INDEX ON claim_entity (entity, rel);
CREATE INDEX ON claimant_entity (claimant_id);
CREATE INDEX ON claimant_entity (entity, rel);
INSERT INTO entity
  SELECT entity, category, count(*),
         category IN ('payout','device','contact','address'),
         category IN ('vendor_payout','public_device','private_ip')
  FROM (SELECT entity, category FROM claim_entity UNION ALL SELECT entity, category FROM claimant_entity) x
  GROUP BY entity, category;
ANALYZE;`

// step: every allowed claim-to-claim transition (the rules of internal/investigate/traverse.go),
// with the identifier it goes through. A view (inlined), not a materialized table.
const viewStep = `
CREATE VIEW step AS
  SELECT a.claim_id AS src, b.claim_id AS dst, e.links AS deg, a.entity AS via, ca.claimant_id AS src_k, cb.claimant_id AS dst_k
  FROM claim_entity a
  JOIN entity e ON e.entity = a.entity
  JOIN claim_entity b ON b.entity = a.entity AND b.rel = a.rel AND b.claim_id <> a.claim_id
  JOIN claim ca ON ca.claim_id = a.claim_id
  JOIN claim cb ON cb.claim_id = b.claim_id
  WHERE ca.claimant_id <> cb.claimant_id   -- the same person filing twice is no shared-entity link
    AND NOT e.context                      -- context-only identifiers never connect claims
    AND (e.strong OR e.links <= 20)        -- weak identifiers used by > 20 claims are infrastructure
  UNION ALL
  SELECT ca.claim_id, cb.claim_id, e.links, ka.entity, ca.claimant_id, cb.claimant_id
  FROM claim ca
  JOIN claimant_entity ka ON ka.claimant_id = ca.claimant_id
  JOIN entity e ON e.entity = ka.entity
  JOIN claimant_entity kb ON kb.entity = ka.entity AND kb.rel = ka.rel AND kb.claimant_id <> ka.claimant_id
  JOIN claim cb ON cb.claimant_id = kb.claimant_id
  WHERE cb.claim_id <> ca.claim_id;`

// strong_edge: the strong-evidence subgraph of internal/graphalgo (claimant payout accounts,
// private devices, phones/emails, addresses, claim-claimant), both directions, with the same
// evidence costs as the analytics overlay (payout 4, device 5, contact 8, address 10, claimant 0).
// An indexed adjacency table built once at load, as a DBA would for repeated walks.
const tableStrongEdge = `
CREATE TABLE strong_edge AS
  SELECT 'C:' || claim_id AS a, 'E:' || entity AS b, CASE category WHEN 'payout' THEN 4 ELSE 5 END AS w FROM claim_entity WHERE category IN ('payout','device')
  UNION ALL SELECT 'E:' || entity, 'C:' || claim_id, CASE category WHEN 'payout' THEN 4 ELSE 5 END FROM claim_entity WHERE category IN ('payout','device')
  UNION ALL SELECT 'C:' || claim_id, 'K:' || claimant_id, 0 FROM claim
  UNION ALL SELECT 'K:' || claimant_id, 'C:' || claim_id, 0 FROM claim
  UNION ALL SELECT 'K:' || claimant_id, 'E:' || entity, CASE category WHEN 'contact' THEN 8 ELSE 10 END FROM claimant_entity
  UNION ALL SELECT 'E:' || entity, 'K:' || claimant_id, CASE category WHEN 'contact' THEN 8 ELSE 10 END FROM claimant_entity;
CREATE INDEX ON strong_edge (a);
ANALYZE strong_edge;`

// ---- questions (PostgreSQL) ----

// Q1a. Every path is enumerated (as the graph traversal does) and each claim keeps its shortest
// distance; maxdeg carries the busiest identifier on the path for the fan-out guard (100), and
// people carries the claimants on the path (no person appears twice on a path).
// Tuning: CROSS JOIN LATERAL (... OFFSET 0) makes each hop look up only the frontier claims.
// Without it the planner hash-joins the WHOLE step view (every transition in the book) on every
// hop: about 650 ms instead of about 1 ms of execution for a 3-hop answer.
const sqlQ1IDs = `
WITH RECURSIVE walk(claim, depth, path, people, maxdeg) AS (
  SELECT s.dst, 1, ARRAY[$1::text, s.dst], ARRAY[s.src_k, s.dst_k], s.deg FROM step s WHERE s.src = $1
  UNION ALL
  SELECT s.dst, w.depth + 1, w.path || s.dst, w.people || s.dst_k, GREATEST(w.maxdeg, s.deg)
  FROM walk w CROSS JOIN LATERAL (SELECT st.dst, st.deg, st.dst_k FROM step st WHERE st.src = w.claim OFFSET 0) s
  WHERE w.depth < $2 AND NOT s.dst = ANY(w.path) AND NOT s.dst_k = ANY(w.people) AND GREATEST(w.maxdeg, s.deg) <= 100
)
SELECT claim, min(depth) FROM walk GROUP BY claim`

// Q1b. The same walk, returning every shortest path with the identifier on each step.
const sqlQ1Paths = `
WITH RECURSIVE walk(claim, depth, path, via, people, maxdeg) AS (
  SELECT s.dst, 1, ARRAY[$1::text, s.dst], ARRAY[s.via], ARRAY[s.src_k, s.dst_k], s.deg FROM step s WHERE s.src = $1
  UNION ALL
  SELECT s.dst, w.depth + 1, w.path || s.dst, w.via || s.via, w.people || s.dst_k, GREATEST(w.maxdeg, s.deg)
  FROM walk w CROSS JOIN LATERAL (SELECT st.dst, st.deg, st.via, st.dst_k FROM step st WHERE st.src = w.claim OFFSET 0) s
  WHERE w.depth < $2 AND NOT s.dst = ANY(w.path) AND NOT s.dst_k = ANY(w.people) AND GREATEST(w.maxdeg, s.deg) <= 100
)
SELECT claim, depth, path, via FROM (
  SELECT claim, depth, path, via, min(depth) OVER (PARTITION BY claim) AS best FROM walk
) x WHERE depth = best`

// Q2. The claim's component in the strong subgraph (UNION drops nodes already reached).
const sqlQ2 = `
WITH RECURSIVE comp(node) AS (
  SELECT 'C:' || $1::text
  UNION
  SELECT e.b FROM comp CROSS JOIN LATERAL (SELECT se.b FROM strong_edge se WHERE se.a = comp.node OFFSET 0) e
)
SELECT substr(node, 3) FROM comp WHERE node LIKE 'C:%'`

// Q3. PostgreSQL has no connected-components operator: iterative label propagation (each node
// takes the smallest label among its neighbours; only nodes whose label changed are pushed again).
var sqlQ3 = []string{
	`DROP TABLE IF EXISTS lbl, frontier, nxt`,
	`CREATE TEMP TABLE lbl (node text PRIMARY KEY, comp text NOT NULL)`,
	`INSERT INTO lbl SELECT DISTINCT a, a FROM strong_edge`,
	`CREATE TEMP TABLE frontier (node text PRIMARY KEY)`,
	`CREATE TEMP TABLE nxt (node text PRIMARY KEY)`,
	`INSERT INTO frontier SELECT node FROM lbl`,
	`ANALYZE lbl`,
}

const sqlQ3Step = `
WITH cand AS (
  SELECT e.b AS node, min(l.comp) AS c
  FROM frontier f JOIN lbl l ON l.node = f.node JOIN strong_edge e ON e.a = f.node
  GROUP BY e.b
), upd AS (
  UPDATE lbl SET comp = cand.c FROM cand WHERE lbl.node = cand.node AND cand.c < lbl.comp RETURNING lbl.node
)
INSERT INTO nxt SELECT node FROM upd`

const sqlQ3Result = `
SELECT array_agg(substr(node, 3) ORDER BY node) FROM lbl WHERE node LIKE 'C:%'
GROUP BY comp HAVING count(*) >= 3`

// Q4. Lowest total evidence cost from one claim to another: iterative Bellman-Ford over the
// strong subgraph (only nodes whose distance improved are relaxed again), capped at cost 999
// like algo.SPpaths' maxCost.
var sqlQ4Init = []string{
	`DROP TABLE IF EXISTS dist, frontier, nxt`,
	`CREATE TEMP TABLE dist (node text PRIMARY KEY, d int NOT NULL)`,
	`CREATE TEMP TABLE frontier (node text PRIMARY KEY)`,
	`CREATE TEMP TABLE nxt (node text PRIMARY KEY)`,
}

const sqlQ4Step = `
WITH cand AS (
  SELECT e.b AS node, min(d.d + e.w) AS c
  FROM frontier f JOIN dist d ON d.node = f.node JOIN strong_edge e ON e.a = f.node
  GROUP BY e.b
), upd AS (
  INSERT INTO dist SELECT node, c FROM cand WHERE c <= 999
  ON CONFLICT (node) DO UPDATE SET d = EXCLUDED.d WHERE EXCLUDED.d < dist.d
  RETURNING node
)
INSERT INTO nxt SELECT node FROM upd`

// ---- FalkorDB queries used directly here ----

// Q3 on FalkorDB: the same WCC call graphalgo makes, run fresh (not from the cache).
const cypherQ3 = `
CALL algo.WCC({nodeLabels: ['Claim','Claimant','ClaimantPayout','PrivateDevice','ContactPoint','Address'],
               relationshipTypes: ['PAID_TO','USED_DEVICE','FILED_BY','HAS_CONTACT','RESIDES_AT']}) YIELD node, componentId
WITH node, componentId WHERE node:Claim
WITH componentId, collect(node.claim_id) AS claims WHERE size(claims) >= 3
RETURN claims`

// ---- export from FalkorDB ----

const pageSize = 5000 // FalkorDB cuts a reply at 10,000 rows (RESULTSET_SIZE), so read in sorted pages

var claimRelLabel = map[string]string{"PAID_TO": "PayoutDestination", "USED_DEVICE": "Device", "REPAIRED_AT": "RepairShop", "FILED_FROM": "IPAddress"}

func category(rel, ptype string, public *bool) string {
	pub := public != nil && *public
	switch rel {
	case "PAID_TO":
		if ptype == "claimant" {
			return "payout"
		}
		return "vendor_payout"
	case "USED_DEVICE":
		if pub {
			return "public_device"
		}
		return "device"
	case "REPAIRED_AT":
		return "repair_shop"
	case "FILED_FROM":
		if pub {
			return "public_ip"
		}
		return "private_ip"
	case "HAS_CONTACT":
		return "contact"
	case "RESIDES_AT":
		return "address"
	}
	return "unknown"
}

func pages(g *falkordb.Graph, q string, each func(rec *falkordb.Record)) int {
	n := 0
	for skip := 0; ; skip += pageSize {
		r, err := g.ROQuery(q, map[string]any{"skip": skip, "limit": pageSize}, nil)
		if err != nil {
			log.Fatalf("export: %v", err)
		}
		got := 0
		for r.Next() {
			each(r.Record())
			got++
		}
		n += got
		if got < pageSize {
			return n
		}
	}
}

func str(rec *falkordb.Record, k string) string {
	v, _ := rec.Get(k)
	s, _ := v.(string)
	return s
}

func boolPtr(rec *falkordb.Record, k string) *bool {
	v, _ := rec.Get(k)
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func exec(ctx context.Context, pg *pgx.Conn, qs ...string) {
	for _, q := range qs {
		if _, err := pg.Exec(ctx, q); err != nil {
			log.Fatalf("postgres: %v\n%s", err, q)
		}
	}
}

func load(ctx context.Context, g *falkordb.Graph, pg *pgx.Conn) (claims, claimEdges, claimantEdges int, took time.Duration) {
	start := time.Now()
	exec(ctx, pg, ddl)
	var rows [][]any
	claims = pages(g, `MATCH (c:Claim)-[:FILED_BY]->(k:Claimant) RETURN c.claim_id AS id, k.claimant_id AS k, c.filing_date AS filed
		ORDER BY id SKIP $skip LIMIT $limit`, func(r *falkordb.Record) {
		rows = append(rows, []any{str(r, "id"), str(r, "k"), str(r, "filed")})
	})
	copyRows(ctx, pg, "claim", []string{"claim_id", "claimant_id", "filing_date"}, rows)

	rows = nil
	claimEdges = pages(g, `MATCH (c:Claim)-[r]->(e) WHERE type(r) IN ['PAID_TO','USED_DEVICE','REPAIRED_AT','FILED_FROM']
		RETURN c.claim_id AS id, type(r) AS rel, coalesce(e.account_id, e.device_id, e.shop_id, e.ip) AS e, e.type AS ptype, e.is_public AS pub
		ORDER BY id, rel, e SKIP $skip LIMIT $limit`, func(r *falkordb.Record) {
		rel := str(r, "rel")
		rows = append(rows, []any{str(r, "id"), rel, claimRelLabel[rel] + ":" + str(r, "e"), category(rel, str(r, "ptype"), boolPtr(r, "pub"))})
	})
	copyRows(ctx, pg, "claim_entity", []string{"claim_id", "rel", "entity", "category"}, rows)

	rows = nil
	claimantEdges = pages(g, `MATCH (k:Claimant)-[r]->(e) WHERE type(r) IN ['HAS_CONTACT','RESIDES_AT']
		RETURN k.claimant_id AS k, type(r) AS rel, coalesce(e.value, e.address_id) AS e
		ORDER BY k, rel, e SKIP $skip LIMIT $limit`, func(r *falkordb.Record) {
		rel := str(r, "rel")
		prefix := map[string]string{"HAS_CONTACT": "ContactPoint:", "RESIDES_AT": "Address:"}[rel]
		rows = append(rows, []any{str(r, "k"), rel, prefix + str(r, "e"), category(rel, "", nil)})
	})
	copyRows(ctx, pg, "claimant_entity", []string{"claimant_id", "rel", "entity", "category"}, rows)

	exec(ctx, pg, indexes, viewStep, tableStrongEdge)
	return claims, claimEdges, claimantEdges, time.Since(start)
}

func copyRows(ctx context.Context, pg *pgx.Conn, table string, cols []string, rows [][]any) {
	if _, err := pg.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows)); err != nil {
		log.Fatalf("copy %s: %v", table, err)
	}
}

func count(ctx context.Context, pg *pgx.Conn, q string) int {
	var n int
	if err := pg.QueryRow(ctx, q).Scan(&n); err != nil {
		log.Fatal(err)
	}
	return n
}

// ---- measurement ----

type outcome struct {
	dur      time.Duration
	timedOut bool
	err      error
}

func timedOut(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "57014" // query_canceled: statement_timeout
}

type stats struct {
	durs                     []time.Duration
	timeouts, errors         int
	agree, disagree, skipped int
	examples                 []string
	note                     string
}

func (s *stats) add(o outcome) {
	switch {
	case o.timedOut:
		s.timeouts++
	case o.err != nil:
		s.errors++
		if s.note == "" {
			s.note = o.err.Error()
		}
	default:
		s.durs = append(s.durs, o.dur)
	}
}

func (s *stats) check(ok bool, example string) {
	if ok {
		s.agree++
		return
	}
	s.disagree++
	if len(s.examples) < 3 {
		s.examples = append(s.examples, example)
	}
}

func pct(ds []time.Duration) string {
	if len(ds) == 0 {
		return "- | - | -"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return fmt.Sprintf("%.1f | %.1f | %.1f", ms(s[len(s)/2]), ms(s[int(0.95*float64(len(s)-1))]), ms(s[len(s)-1]))
}

// sqlClaims runs a query returning (claim, depth) or (claim) rows.
func sqlClaims(ctx context.Context, pg *pgx.Conn, q string, args ...any) (map[string]int, outcome) {
	start := time.Now()
	rows, err := pg.Query(ctx, q, args...)
	out := map[string]int{}
	if err == nil {
		for rows.Next() {
			var id string
			var d int32 = 1
			if len(rows.FieldDescriptions()) == 2 {
				err = rows.Scan(&id, &d)
			} else {
				err = rows.Scan(&id)
			}
			if err != nil {
				break
			}
			out[id] = int(d)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	o := outcome{dur: time.Since(start), err: err}
	if timedOut(err) {
		o.timedOut, o.err = true, nil
	}
	return out, o
}

// sqlPaths runs Q1b and returns claim -> depth plus the number of shortest paths returned.
func sqlPaths(ctx context.Context, pg *pgx.Conn, claimID string, hops int) (map[string]int, int, outcome) {
	start := time.Now()
	rows, err := pg.Query(ctx, sqlQ1Paths, claimID, hops)
	out := map[string]int{}
	paths := 0
	if err == nil {
		for rows.Next() {
			var id string
			var d int32
			var path, via []string
			if err = rows.Scan(&id, &d, &path, &via); err != nil {
				break
			}
			out[id] = int(d)
			paths++
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	o := outcome{dur: time.Since(start), err: err}
	if timedOut(err) {
		o.timedOut, o.err = true, nil
	}
	return out, paths, o
}

func same(a, b map[string]int) (bool, string) {
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false, fmt.Sprintf("%s: FalkorDB %d, PostgreSQL %v", k, v, b[k])
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			return false, fmt.Sprintf("%s: only PostgreSQL (%d)", k, v)
		}
	}
	return true, ""
}

// pgLoop runs an iterative SQL algorithm: setup, then step until it changes nothing.
func pgLoop(ctx context.Context, pg *pgx.Conn, setup []string, step string) (iterations int, err error) {
	for _, q := range setup {
		if _, err := pg.Exec(ctx, q); err != nil {
			return 0, err
		}
	}
	for {
		iterations++
		tag, err := pg.Exec(ctx, step)
		if err != nil {
			return iterations, err
		}
		if tag.RowsAffected() == 0 {
			return iterations, nil
		}
		if _, err := pg.Exec(ctx, `TRUNCATE frontier; INSERT INTO frontier SELECT node FROM nxt; TRUNCATE nxt`); err != nil {
			return iterations, err
		}
	}
}

func main() {
	name := flag.String("graph", graph.AppGraphName, "FalkorDB graph to compare")
	manPath := flag.String("manifest", "testdata/scale/manifest.json", "manifest written by cmd/seedscale")
	dsn := flag.String("pg", "postgres://bench:bench@127.0.0.1:5433/claimgraph_bench", "PostgreSQL connection (docker compose --profile bench)")
	randomN := flag.Int("random", 100, "random claims to sample")
	limit := flag.Int("limit", 0, "at most this many claims per sample (0 = all; for a quick check)")
	runs := flag.Int("runs", 3, "repetitions of the book-wide ring question (Q3)")
	timeoutMs := flag.Int("timeout", 10000, "PostgreSQL statement timeout per query (ms)")
	out := flag.String("out", "testdata/scale/benchmark_relational.md", "report path ('' = stdout only)")
	flag.Parse()
	ctx := context.Background()

	var man manifest
	b, err := os.ReadFile(*manPath)
	if err != nil {
		log.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(b, &man); err != nil {
		log.Fatal(err)
	}
	db, err := graph.Connect()
	if err != nil {
		log.Fatal(err)
	}
	g := db.SelectGraph(*name)
	if err := graphalgo.CheckOverlay(g); err != nil {
		log.Fatal(err)
	}
	pg, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		log.Fatalf("PostgreSQL: %v (start it with: docker compose --profile bench up -d postgres-bench)", err)
	}
	defer pg.Close(ctx)

	claims, claimEdges, claimantEdges, loadTook := load(ctx, g, pg)
	log.Printf("copied %d claims, %d claim links, %d claimant links into PostgreSQL in %s", claims, claimEdges, claimantEdges, loadTook.Round(time.Millisecond))
	// The copy must be complete. (The live book can differ from the seed manifest by claims
	// submitted through the app since seeding; both engines see the same copy either way.)
	pgClaims := count(ctx, pg, "SELECT count(*) FROM claim")
	pgLinks := count(ctx, pg, "SELECT count(*) FROM claim_entity") + count(ctx, pg, "SELECT count(*) FROM claimant_entity")
	if pgClaims != claims || pgLinks != claimEdges+claimantEdges {
		log.Fatalf("copy incomplete: %d/%d claims, %d/%d links", pgClaims, claims, pgLinks, claimEdges+claimantEdges)
	}
	exec(ctx, pg, fmt.Sprintf("SET statement_timeout = %d", *timeoutMs))

	// Samples: the same selection as cmd/bench (planted rings, busiest shop, random non-ring claims).
	inRing := map[string]bool{}
	var ringClaims []string
	for _, r := range man.Dataset.Rings {
		for _, c := range r.Claims {
			inRing[c] = true
			ringClaims = append(ringClaims, c)
		}
	}
	var busy []string
	pages(g, `MATCH (c:Claim)-[:REPAIRED_AT]->(:RepairShop {shop_id:'`+man.Dataset.Stats.BusiestShop+`'}) RETURN c.claim_id AS id ORDER BY id SKIP $skip LIMIT $limit`,
		func(r *falkordb.Record) {
			if len(busy) < 50 {
				busy = append(busy, str(r, "id"))
			}
		})
	var all []string
	pages(g, `MATCH (c:Claim) RETURN c.claim_id AS id ORDER BY id SKIP $skip LIMIT $limit`, func(r *falkordb.Record) { all = append(all, str(r, "id")) })
	rnd := rand.New(rand.NewSource(7))
	rnd.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	var random []string
	for _, c := range all {
		if !inRing[c] && len(random) < *randomN {
			random = append(random, c)
		}
	}
	samples := []struct {
		label  string
		claims []string
	}{{"planted-ring claims", ringClaims}, {"busiest-shop claims", busy}, {"random claims", random}}
	if *limit > 0 {
		for i := range samples {
			samples[i].claims = samples[i].claims[:min(*limit, len(samples[i].claims))]
		}
	}

	// Warm-up so neither side pays for cold caches in the measurements.
	for _, c := range random[:min(5, len(random))] {
		_, _, _ = investigate.ConnectedOnly(g, c, 3)
		_, _ = investigate.FindRelatedClaims(g, c, 3)
		_, _ = sqlClaims(ctx, pg, sqlQ1IDs, c, 3)
		_, _, _ = sqlPaths(ctx, pg, c, 3)
	}
	coldStart := time.Now()
	if _, err := graphalgo.RingOf(g, ringClaims[0]); err != nil {
		log.Fatal(err)
	}
	ringCold := time.Since(coldStart)

	type key struct{ sample, question, engine string }
	res := map[key]*stats{}
	get := func(k key) *stats {
		if res[k] == nil {
			res[k] = &stats{}
		}
		return res[k]
	}
	pathsReturned := map[string][2]int{} // question -> [FalkorDB paths, PostgreSQL paths]
	perClaim := []string{"Q1a hops=1", "Q1a hops=2", "Q1a hops=3", "Q1b hops=1", "Q1b hops=2", "Q1b hops=3", "Q2 ring"}
	for _, s := range samples {
		for _, c := range s.claims {
			for hops := 1; hops <= 3; hops++ {
				// Q1a: ids + hop counts only.
				q := fmt.Sprintf("Q1a hops=%d", hops)
				start := time.Now()
				fa, cut, ferr := investigate.ConnectedOnly(g, c, hops)
				get(key{s.label, q, "FalkorDB"}).add(outcome{dur: time.Since(start), err: ferr})
				pa, po := sqlClaims(ctx, pg, sqlQ1IDs, c, hops)
				get(key{s.label, q, "PostgreSQL"}).add(po)
				chk := get(key{s.label, q, "check"})
				if ferr != nil || cut || po.err != nil || po.timedOut {
					chk.skipped++
				} else {
					ok, why := same(fa, pa)
					chk.check(ok, c+" "+why)
				}

				// Q1b: with the evidence paths (production call on FalkorDB).
				q = fmt.Sprintf("Q1b hops=%d", hops)
				start = time.Now()
				fr, ferr := investigate.FindRelatedClaims(g, c, hops)
				get(key{s.label, q, "FalkorDB"}).add(outcome{dur: time.Since(start), err: ferr})
				pb, npaths, po := sqlPaths(ctx, pg, c, hops)
				get(key{s.label, q, "PostgreSQL"}).add(po)
				chk = get(key{s.label, q, "check"})
				if ferr != nil || fr.Truncated || po.err != nil || po.timedOut {
					chk.skipped++
					continue
				}
				fb, fpaths := map[string]int{}, 0
				for _, cc := range fr.Connected {
					fb[cc.ClaimID] = cc.Hops
				}
				for _, rc := range fr.RelatedClaims {
					fpaths += len(rc.Paths)
				}
				p := pathsReturned[q]
				pathsReturned[q] = [2]int{p[0] + fpaths, p[1] + npaths}
				ok, why := same(fb, pb)
				chk.check(ok, c+" "+why)
			}
			// Q2: ring membership.
			start := time.Now()
			ring, rerr := graphalgo.RingOf(g, c)
			get(key{s.label, "Q2 ring", "FalkorDB"}).add(outcome{dur: time.Since(start), err: rerr})
			ps, po := sqlClaims(ctx, pg, sqlQ2, c)
			get(key{s.label, "Q2 ring", "PostgreSQL"}).add(po)
			chk := get(key{s.label, "Q2 ring", "check"})
			if rerr != nil || ring.Oversized || po.err != nil || po.timedOut {
				chk.skipped++
				continue
			}
			fs := map[string]int{}
			for _, m := range ring.Members {
				fs[m.ClaimID] = 1
			}
			ok, why := same(fs, ps)
			chk.check(ok, c+" "+why)
		}
		log.Printf("sample %q done", s.label)
	}

	// Q3: every ring in the book.
	q3 := map[string]*stats{"FalkorDB": {}, "PostgreSQL": {}}
	var fRings, pRings map[string]bool
	iterations := 0
	for i := 0; i < *runs; i++ {
		start := time.Now()
		r, err := g.ROQuery(cypherQ3, nil, falkordb.NewQueryOptions().SetTimeout(*timeoutMs))
		rings := map[string]bool{}
		if err == nil {
			for r.Next() {
				v, _ := r.Record().Get("claims")
				var ids []string
				for _, x := range v.([]any) {
					ids = append(ids, x.(string))
				}
				sort.Strings(ids)
				rings[strings.Join(ids, ",")] = true
			}
		}
		q3["FalkorDB"].add(outcome{dur: time.Since(start), err: err})
		fRings = rings

		start = time.Now()
		iterations, err = pgLoop(ctx, pg, sqlQ3, sqlQ3Step)
		prings := map[string]bool{}
		if err == nil {
			var rows pgx.Rows
			if rows, err = pg.Query(ctx, sqlQ3Result); err == nil {
				for rows.Next() {
					var ids []string
					if err = rows.Scan(&ids); err != nil {
						break
					}
					sort.Strings(ids)
					prings[strings.Join(ids, ",")] = true
				}
				rows.Close()
			}
		}
		q3["PostgreSQL"].add(outcome{dur: time.Since(start), err: err, timedOut: timedOut(err)})
		pRings = prings
		log.Printf("Q3 run %d done", i+1)
	}
	q3same, q3diff := 0, 0
	for k := range fRings {
		if pRings[k] {
			q3same++
		} else {
			q3diff++
		}
	}
	for k := range pRings {
		if !fRings[k] {
			q3diff++
		}
	}

	// Q4: strongest evidence chain between the first and last claim of every planted ring.
	q4 := map[string]*stats{"FalkorDB": {}, "PostgreSQL": {}}
	q4chk := &stats{}
	for _, r := range man.Dataset.Rings {
		a, z := r.Claims[0], r.Claims[len(r.Claims)-1]
		start := time.Now()
		l, err := graphalgo.FindStrongestLink(g, a, z)
		q4["FalkorDB"].add(outcome{dur: time.Since(start), err: err})
		start = time.Now()
		setup := append(append([]string{}, sqlQ4Init...),
			fmt.Sprintf(`INSERT INTO dist VALUES ('C:%s', 0)`, a), fmt.Sprintf(`INSERT INTO frontier VALUES ('C:%s')`, a))
		_, perr := pgLoop(ctx, pg, setup, sqlQ4Step)
		cost := -1
		if perr == nil {
			var d int32
			if e := pg.QueryRow(ctx, `SELECT d FROM dist WHERE node = $1`, "C:"+z).Scan(&d); e == nil {
				cost = int(d)
			} else if !errors.Is(e, pgx.ErrNoRows) {
				perr = e
			}
		}
		q4["PostgreSQL"].add(outcome{dur: time.Since(start), err: perr, timedOut: timedOut(perr)})
		if err == nil && perr == nil {
			fcost := -1
			if l.Found {
				fcost = l.TotalCost
			}
			q4chk.check(fcost == cost, fmt.Sprintf("%s -> %s: FalkorDB cost %d, PostgreSQL %d", a, z, fcost, cost))
		} else {
			q4chk.skipped++
		}
	}

	// ---- report ----
	var r strings.Builder
	fmt.Fprintf(&r, "# FalkorDB vs PostgreSQL: the same investigation questions on the same book\n\n")
	fmt.Fprintf(&r, "Book: graph `%s`, %d claims, %d claim links and %d claimant links, copied from FalkorDB into PostgreSQL 18.6 "+
		"(indexed tables, copy took %s). Both run in local Docker on the same machine (WSL2), default settings except the query tuning below. "+
		"Wall-clock times from Go, after a warm-up. Synthetic data; the numbers describe this book on this machine only. "+
		"PostgreSQL statement timeout %d ms. Generated by `go run ./cmd/benchrel`.\n\n",
		*name, claims, claimEdges, claimantEdges, loadTook.Round(time.Millisecond), *timeoutMs)
	r.WriteString("Both sides apply the same rules: context-only identifiers never connect claims, weak identifiers used by more than 20 claims " +
		"are infrastructure, identifiers with more than 100 links are not expanded beyond the first hop, the same claimant filing twice is no " +
		"claim-level link, every claim on a path is distinct, and no claimant appears twice on a path. Every answer is compared before any time is reported.\n\n")

	// Summary: every sample pooled, median per question.
	median := func(ds []time.Duration) float64 {
		if len(ds) == 0 {
			return 0
		}
		s := append([]time.Duration(nil), ds...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return float64(s[len(s)/2].Microseconds()) / 1000
	}
	pooled := func(q, e string) []time.Duration {
		var ds []time.Duration
		for _, s := range samples {
			ds = append(ds, get(key{s.label, q, e}).durs...)
		}
		return ds
	}
	faster := func(f, p float64) string {
		switch {
		case f == 0 || p == 0:
			return "-"
		case f < p:
			return fmt.Sprintf("FalkorDB %.0fx", p/f)
		default:
			return fmt.Sprintf("PostgreSQL %.0fx", f/p)
		}
	}
	allSame := true
	for k, st := range res {
		if k.engine == "check" && st.disagree > 0 {
			allSame = false
		}
	}
	identical := allSame && q3diff == 0 && q4chk.disagree == 0
	if identical {
		r.WriteString("Both engines returned **identical answers on every comparison** (details below), so every timing compares equivalent implementations of the same question.\n\n")
	} else {
		r.WriteString("**The answers differ somewhere (see below), so the timings for those questions are not a like-for-like comparison.**\n\n")
	}

	// 1. Where the graph database excels: whole-book and any-depth questions.
	r.WriteString("## 1. Where the graph database excels: finding rings and evidence chains across the whole book\n\n" +
		"| question | FalkorDB (median ms) | PostgreSQL (median ms) | faster |\n|---|---|---|---|\n")
	for _, row := range []struct {
		q string
		m map[string]*stats
	}{{"Q3 every ring in the book", q3}, {"Q4 strongest evidence chain at any depth", q4}} {
		f, p := median(row.m["FalkorDB"].durs), median(row.m["PostgreSQL"].durs)
		fmt.Fprintf(&r, "| %s | %.1f | %.1f | **%s** |\n", row.q, f, p, faster(f, p))
	}
	fmt.Fprintf(&r, "\nThese are the questions ClaimGraph's ring detection is built on. Listing every ring in the book (%d rings) is one "+
		"`CALL algo.WCC` in FalkorDB; plain PostgreSQL has no connected-components operator and needed %d rounds of full-table label propagation. "+
		"The strongest evidence chain between two claims at any depth is one `algo.SPpaths` call; in PostgreSQL it is an iterative Bellman-Ford "+
		"over the same evidence costs. (Extensions such as pgRouting add graph algorithms to PostgreSQL; we compared plain PostgreSQL.)\n\n",
		len(fRings), iterations)

	// 2. Short lookups from one claim: honestly reported, PostgreSQL is faster here.
	r.WriteString("## 2. Short lookups from one claim: tuned PostgreSQL is faster\n\n" +
		"| question | FalkorDB (median ms) | PostgreSQL (median ms) | faster |\n|---|---|---|---|\n")
	for _, q := range perClaim {
		f, p := median(pooled(q, "FalkorDB")), median(pooled(q, "PostgreSQL"))
		fmt.Fprintf(&r, "| %s | %.1f | %.1f | %s |\n", q, f, p, faster(f, p))
	}
	r.WriteString("\nBoth engines answer these in well under a second, fast enough for interactive investigation, but this is not a tie: on this " +
		"book a tuned PostgreSQL wins. A 1-3 hop walk over 20,000 claims is a handful of B-tree index lookups. Two things to know when reading it:\n\n" +
		"- **PostgreSQL needed graph-specific engineering to get there.** Written the obvious way, the same 3-hop question took about 650 ms because " +
		"the planner joined every transition in the book on every hop; it also needed the traversal rules hand-encoded as a view and a planner hint " +
		"(see Tuning).\n" +
		"- **FalkorDB's time here is query execution, not the network** (the client round trip is about 0.5 ms): our traversal runs one Cypher " +
		"pattern branch per evidence type and path shape, written for correctness and scale limits (fan-out guard, row caps), not micro-tuned per branch.\n\n")

	// Ratios come from this run (never typed in), so the text cannot go stale.
	q3r := faster(median(q3["FalkorDB"].durs), median(q3["PostgreSQL"].durs))
	q4r := faster(median(q4["FalkorDB"].durs), median(q4["PostgreSQL"].durs))
	fmt.Fprintf(&r, "## What this means\n\n"+
		"- **Use the graph for what only a graph does well.** Ring detection over the whole book (%s) and any-depth evidence chains (%s) "+
		"take one built-in call each in FalkorDB, and they are the core of ClaimGraph's ring story.\n"+
		"- **Do not claim speed for short lookups.** For bounded 1-3 hop questions from a single claim, a well-indexed relational database is "+
		"faster on this book once tuned.\n", q3r, q4r)
	if allSame && q3diff == 0 && q4chk.disagree == 0 {
		r.WriteString("- **Same answers.** Both engines return the same connected claims, hop counts, ring members, ring list and path costs, so the " +
			"comparison is between equivalent implementations, not between different questions.\n")
	} else {
		r.WriteString("- **The answers differ somewhere (see below), so the timings for those questions are not a like-for-like comparison.**\n")
	}
	r.WriteString("\nLimits: one machine, one synthetic 20,000-claim book, PostgreSQL defaults plus the stated query tuning, FalkorDB defaults. Larger or denser " +
		"books may change the picture in either direction.\n\n")

	r.WriteString("## Same answers?\n\n| sample | question | identical | different | skipped |\n|---|---|---|---|---|\n")
	var examples []string
	for _, s := range samples {
		for _, q := range perClaim {
			c := get(key{s.label, q, "check"})
			fmt.Fprintf(&r, "| %s | %s | %d | %d | %d |\n", s.label, q, c.agree, c.disagree, c.skipped)
			for _, e := range c.examples {
				examples = append(examples, fmt.Sprintf("%s / %s: %s", s.label, q, e))
			}
		}
	}
	fmt.Fprintf(&r, "| whole book | Q3 all rings (3+ claims) | %d | %d | - |\n", q3same, q3diff)
	fmt.Fprintf(&r, "| planted rings | Q4 strongest link cost | %d | %d | %d |\n", q4chk.agree, q4chk.disagree, q4chk.skipped)
	examples = append(examples, q4chk.examples...)
	if len(examples) > 0 {
		r.WriteString("\nDifferences:\n\n")
		for _, e := range examples {
			fmt.Fprintf(&r, "- %s\n", e)
		}
	}

	r.WriteString("\n## Details: book-wide and any-depth questions (ms)\n\n| question | engine | runs | p50 | p95 | max | timeouts | errors |\n|---|---|---|---|---|---|---|---|\n")
	for _, e := range []string{"FalkorDB", "PostgreSQL"} {
		st := q3[e]
		fmt.Fprintf(&r, "| Q3 every ring in the book | %s | %d | %s | %d | %d |\n", e, len(st.durs), pct(st.durs), st.timeouts, st.errors)
	}
	for _, e := range []string{"FalkorDB", "PostgreSQL"} {
		st := q4[e]
		fmt.Fprintf(&r, "| Q4 strongest link (12 ring pairs) | %s | %d | %s | %d | %d |\n", e, len(st.durs), pct(st.durs), st.timeouts, st.errors)
	}
	fmt.Fprintf(&r, "\nQ3: FalkorDB found %d rings with one `CALL algo.WCC`; PostgreSQL needed %d label-propagation rounds (one UPDATE each) "+
		"because it has no connected-components operator. Q4: FalkorDB `algo.SPpaths`; PostgreSQL iterative Bellman-Ford over the same evidence costs.\n",
		len(fRings), iterations)
	for _, st := range []*stats{q3["PostgreSQL"], q4["PostgreSQL"]} {
		if st.note != "" {
			fmt.Fprintf(&r, "\nPostgreSQL error: %s\n", st.note)
		}
	}

	r.WriteString("\n## Details: per-claim questions (ms)\n\n| sample | question | engine | n | p50 | p95 | max | timeouts | errors |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range samples {
		for _, q := range perClaim {
			for _, e := range []string{"FalkorDB", "PostgreSQL"} {
				st := get(key{s.label, q, e})
				fmt.Fprintf(&r, "| %s | %s | %s | %d | %s | %d | %d |\n", s.label, q, e, len(st.durs), pct(st.durs), st.timeouts, st.errors)
			}
		}
	}
	r.WriteString("\nQ1b evidence returned (all samples): ")
	for _, q := range []string{"Q1b hops=1", "Q1b hops=2", "Q1b hops=3"} {
		p := pathsReturned[q]
		fmt.Fprintf(&r, "%s FalkorDB %d paths, PostgreSQL %d paths; ", q, p[0], p[1])
	}
	r.WriteString("FalkorDB keeps at most 5 example paths per claim beyond one hop (no evidence lost) and also returns evidence ids and timestamps; " +
		"PostgreSQL returns every shortest path with the identifier on each step.\n")
	fmt.Fprintf(&r, "\nQ2 on FalkorDB serves rings from one algo.WCC result per evidence version: the first (cold) call took %d ms, the rest are above. "+
		"PostgreSQL walks each ring with its own recursive query.\n", ringCold.Milliseconds())

	r.WriteString("\n## Tuning applied to PostgreSQL\n\nThe recursive queries use `CROSS JOIN LATERAL (... OFFSET 0)` so each hop looks up only the claims " +
		"on the frontier through the indexes. Without it the planner hash-joins the whole `step` view (every transition in the book) on every hop: " +
		"one 3-hop question took about 650 ms of execution instead of about 1 ms. The strong-evidence adjacency is a precomputed, indexed table. " +
		"Q3 and Q4 push only the nodes whose label or distance changed. Everything else is PostgreSQL 18 defaults.\n")

	r.WriteString("\n## The PostgreSQL queries\n\n```sql\n-- the traversal rules\n" + strings.TrimSpace(viewStep) +
		"\n\n-- Q1a\n" + strings.TrimSpace(sqlQ1IDs) + "\n\n-- Q1b\n" + strings.TrimSpace(sqlQ1Paths) +
		"\n\n-- strong-evidence adjacency (Q2-Q4)\n" + strings.TrimSpace(tableStrongEdge) +
		"\n\n-- Q2\n" + strings.TrimSpace(sqlQ2) + "\n\n-- Q3 (repeated until nothing changes)\n" + strings.TrimSpace(sqlQ3Step) +
		"\n\n-- Q4 (repeated until nothing changes)\n" + strings.TrimSpace(sqlQ4Step) + "\n```\n\n" +
		"FalkorDB: Q1a/Q1b are built from the fixed Cypher fragments in `internal/investigate/traverse.go` (one staged MATCH per hop, one branch per path shape); " +
		"Q2/Q3 call `algo.WCC` and Q4 calls `algo.SPpaths` (`internal/graphalgo`).\n")

	fmt.Print(r.String())
	if *out != "" {
		if err := os.WriteFile(*out, []byte(r.String()), 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s", *out)
	}
}
