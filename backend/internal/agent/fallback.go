package agent

import (
	"fmt"
	"strings"

	"claimgraph/internal/scoring"
)

// Finding is one grounded statement with the claims and entities it rests on.
type Finding struct {
	Statement string   `json:"statement"`
	ClaimIDs  []string `json:"claim_ids"`
	Entities  []string `json:"entities"`
}

// NextStep is the recommended verification action.
type NextStep struct {
	Action    string   `json:"action"`
	Rationale string   `json:"rationale"`
	ClaimIDs  []string `json:"claim_ids"`
	Entities  []string `json:"entities"`
}

// Explanation is the natural-language layer on top of the deterministic report.
type Explanation struct {
	Summary     string    `json:"summary"`
	KeyFindings []Finding `json:"key_findings"`
	Uncertainty []string  `json:"uncertainty"`
	NextStep    NextStep  `json:"next_step"`
}

// factor name -> next verification step template (strongest evidence first).
var nextStepByFactor = []struct {
	factor, action, rationale string
}{
	{scoring.FSharedPayout,
		"Verify the account holder of payout destination %s against the claimants of record on claims %s, and document whether a legitimate shared-account relationship (for example a joint account) exists.",
		"A claimant payout destination shared across claims is the strongest signal in the graph."},
	{scoring.FSharedDevice,
		"Review the submission records for device %s across claims %s and ask the claimants, using policy-of-record contact details, whether they share that device (household or workplace).",
		"A shared non-public device links these submissions."},
	{scoring.FSharedContact,
		"Confirm who owns contact %s on claims %s by contacting each claimant independently through the policy-of-record details.",
		"A shared phone or email links these claimants."},
	{scoring.FSharedAddress,
		"Verify residency at address %s for the claimants on claims %s (household members or unrelated parties) using policy-of-record information.",
		"A shared address links these claimants."},
	{scoring.FSharedRepair,
		"Request itemized estimates, invoices and photos from repair shop %s for claims %s and compare them for inconsistencies; a shared shop alone is weak evidence.",
		"The only remaining link is a shared repair shop, which is weak on its own."},
	{scoring.FSharedPublicIP,
		"Treat shared public IP %s as weak context and confirm only whether it is a shared network (carrier, library or office) before spending further effort.",
		"A shared public IP is the weakest signal in the model."},
}

// buildFallback produces a fully deterministic explanation from the report. It cites
// only claims, entities and factors that exist in the report, so it can never invent
// evidence. It is used whenever the LLM is unavailable or its output fails validation.
func buildFallback(rep *scoring.Report) *Explanation {
	e := &Explanation{KeyFindings: []Finding{}, Uncertainty: []string{}}

	if rep.ConnectedClaims == 0 {
		e.Summary = fmt.Sprintf("Claim %s has an investigation-priority score of %d (%s). No supporting connection to other claims was found in the graph, so no evidence paths are reported.",
			rep.TargetClaim, rep.Score, rep.Band)
		e.NextStep = NextStep{
			Action:    "No graph-based verification is indicated; continue standard claim handling.",
			Rationale: "The graph contains no shared entities linking this claim to others.",
			ClaimIDs:  []string{}, Entities: []string{},
		}
		e.Uncertainty = append(e.Uncertainty, rep.Uncertainty...)
		return e
	}

	var parts []string
	for _, f := range rep.Factors {
		parts = append(parts, fmt.Sprintf("%s (+%d)", f.Name, f.Points))
		e.KeyFindings = append(e.KeyFindings, Finding{
			Statement: fmt.Sprintf("%s: %s (+%d points).", f.Name, f.Reason, f.Points),
			ClaimIDs:  nonNil(f.Claims), Entities: nonNil(f.Entities),
		})
	}
	e.Summary = fmt.Sprintf("Claim %s has an investigation-priority score of %d (%s) from graph evidence linking it to %d other claim(s): %s. This ranks the claim for human review; it is not a fraud determination.",
		rep.TargetClaim, rep.Score, rep.Band, rep.ConnectedClaims, strings.Join(parts, ", "))

	e.NextStep = NextStep{
		Action:    "Review the listed evidence paths and apply standard claim handling; the graph evidence does not by itself indicate a specific verification step.",
		Rationale: "No scoring signal category was found among the direct connections.",
		ClaimIDs:  []string{}, Entities: []string{},
	}
	for _, ns := range nextStepByFactor {
		for _, f := range rep.Factors {
			if f.Name != ns.factor {
				continue
			}
			ent := ""
			if len(f.Entities) > 0 {
				ent = bareID(f.Entities[0])
			}
			e.NextStep = NextStep{
				Action:    fmt.Sprintf(ns.action, ent, strings.Join(f.Claims, ", ")),
				Rationale: ns.rationale,
				ClaimIDs:  nonNil(f.Claims), Entities: nonNil(f.Entities),
			}
			goto done
		}
	}
done:
	e.Uncertainty = append(e.Uncertainty, rep.Uncertainty...)
	return e
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// bareID turns "PayoutDestination:PAY-01" into "PAY-01".
func bareID(s string) string {
	if i := strings.Index(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}
