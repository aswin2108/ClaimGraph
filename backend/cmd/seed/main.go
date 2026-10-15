// seed loads the synthetic graph into FalkorDB and runs sanity queries.
package main

import (
	"fmt"
	"os"

	"claimgraph/internal/graph"
	"claimgraph/internal/seed"

	"github.com/FalkorDB/falkordb-go/v2"
)

func main() {
	db, err := graph.Connect()
	if err != nil {
		die(err)
	}
	g := db.SelectGraph(graph.DefaultGraphName)

	n, err := seed.Run(g)
	if err != nil {
		die(err)
	}
	fmt.Printf("Seeded graph %q (%d statements)\n\n", graph.DefaultGraphName, n)

	show(g, "Node counts by label",
		`MATCH (n) RETURN labels(n)[0] AS label, count(n) AS cnt ORDER BY label`)
	show(g, "Edge counts by type",
		`MATCH ()-[r]->() RETURN type(r) AS rel, count(r) AS cnt ORDER BY rel`)
	show(g, "Claims sharing a CLAIMANT payout destination with CLM-1001 (via entity, no direct claim edge)",
		`MATCH (a:Claim {claim_id:$id})-[:PAID_TO]->(p:PayoutDestination {type:'claimant'})<-[:PAID_TO]-(b:Claim)
		 WHERE b <> a RETURN b.claim_id AS related, p.account_id AS shared_payout ORDER BY related`)
	show(g, "Claims sharing a non-public device with CLM-1001",
		`MATCH (a:Claim {claim_id:$id})-[:USED_DEVICE]->(d:Device {is_public:false})<-[:USED_DEVICE]-(b:Claim)
		 WHERE b <> a RETURN b.claim_id AS related, d.device_id AS shared_device ORDER BY related`)
	show(g, "Claims sharing the repair shop with CLM-1001",
		`MATCH (a:Claim {claim_id:$id})-[:REPAIRED_AT]->(s:RepairShop)<-[:REPAIRED_AT]-(b:Claim)
		 WHERE b <> a RETURN b.claim_id AS related, s.name AS shop ORDER BY related`)
	show(g, "Vendor direct-settlement account (CLM-1004) must NOT count as a claimant payout",
		`MATCH (a:Claim {claim_id:'CLM-1004'})-[:PAID_TO]->(p:PayoutDestination)
		 RETURN p.account_id AS account, p.type AS type`)
}

// show runs a read-only query with $id = CLM-1001 and prints the rows.
func show(g *falkordb.Graph, title, q string) {
	fmt.Println("==", title)
	res, err := g.ROQuery(q, map[string]interface{}{"id": "CLM-1001"}, nil)
	if err != nil {
		die(err)
	}
	if res.Empty() {
		fmt.Println("   (no rows)")
	}
	for res.Next() {
		r := res.Record()
		fmt.Print("   ")
		for i, k := range r.Keys() {
			v, _ := r.GetByIndex(i)
			fmt.Printf("%s=%v  ", k, v)
		}
		fmt.Println()
	}
	fmt.Println()
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
