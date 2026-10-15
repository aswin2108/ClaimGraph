package ingest

import (
	"fmt"
	"strings"

	"github.com/FalkorDB/falkordb-go/v2"
)

// indexes covers every property used to look a node up (MERGE keys and investigation
// entry points). Without them each MERGE / MATCH by id is a label scan, which is fine
// for a demo graph and unusable at tens of thousands of claims.
var indexes = [][2]string{
	{"Claim", "claim_id"}, {"Claimant", "claimant_id"}, {"Policy", "policy_id"}, {"Vehicle", "vin"},
	{"RepairShop", "shop_id"}, {"PayoutDestination", "account_id"}, {"TowProvider", "tow_id"},
	{"ContactPoint", "value"}, {"Address", "address_id"}, {"Device", "device_id"}, {"IPAddress", "ip"},
	{"InvestigationCase", "case_id"},
}

// EnsureIndexes creates the lookup indexes. It is idempotent: an index that already
// exists is left alone. Runs at API start-up, after seeding and before bulk loads.
func EnsureIndexes(g *falkordb.Graph) error {
	for _, ix := range indexes {
		q := fmt.Sprintf("CREATE INDEX FOR (n:%s) ON (n.%s)", ix[0], ix[1]) // constants only
		if _, err := g.Query(q, nil, nil); err != nil && !alreadyIndexed(err) {
			return fmt.Errorf("create index %s(%s): %w", ix[0], ix[1], err)
		}
	}
	return nil
}

func alreadyIndexed(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already indexed") || strings.Contains(m, "already exists")
}
