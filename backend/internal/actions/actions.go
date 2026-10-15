// Package actions turns graph evidence into recommended VERIFICATION steps for an
// investigator: "agents that act on connected data", without ever acting on a claim.
//
// Every action is derived deterministically from the scored report and the claim's ring,
// cites the claims, identifiers and graph path behind it, and requires a human. None of
// them pays, denies, cancels or accuses; "hold for review" is a simulated queue state that
// an investigator would have to approve. The same evidence always yields the same actions.
package actions

import (
	"fmt"
	"sort"
	"strings"

	"claimgraph/internal/graphalgo"
	"claimgraph/internal/scoring"
)

// Action kinds.
const (
	KindVerifyPayoutHolder   = "verify_payout_account_holder"
	KindReviewDeviceRecords  = "review_device_records"
	KindConfirmContactOwner  = "confirm_contact_ownership"
	KindReviewPriorCase      = "review_prior_case"
	KindVerifyPolicyOrigins  = "verify_policy_applications"
	KindHoldRingForReview    = "hold_linked_claims_for_review"
	KindConfirmHousehold     = "confirm_household_relationship"
	KindProcessOrdinarily    = "no_graph_verification_needed"
	maxClaimsPerAction       = 25
	maxActions               = 8
	disclaimerRecommendation = "Recommended verification for a human investigator; it is not a decision about the claim."
)

// Action is one recommended verification step.
type Action struct {
	Kind          string   `json:"kind"`
	Priority      int      `json:"priority"` // 1 = first
	Title         string   `json:"title"`
	Rationale     string   `json:"rationale"`
	Claims        []string `json:"claims,omitempty"`
	Entities      []string `json:"entities,omitempty"`
	EvidencePath  string   `json:"evidence_path,omitempty"` // one real graph path behind the action
	Simulated     bool     `json:"simulated,omitempty"`     // a queue-state change that is only simulated
	RequiresHuman bool     `json:"requires_human_approval"`
}

// Plan is the ordered list of recommended actions.
type Plan struct {
	TargetClaim string   `json:"target_claim"`
	Actions     []Action `json:"actions"`
	Note        string   `json:"note"`
}

// Recommend derives the verification plan. ring may be nil (no ring lookup available).
func Recommend(rep *scoring.Report, ring *graphalgo.Ring) *Plan {
	p := &Plan{TargetClaim: rep.TargetClaim, Actions: []Action{}, Note: disclaimerRecommendation}
	f := map[string]scoring.Factor{}
	for _, x := range rep.Factors {
		f[x.Name] = x
	}
	add := func(a Action) {
		a.RequiresHuman = true
		if len(a.Claims) > maxClaimsPerAction {
			a.Claims = a.Claims[:maxClaimsPerAction]
		}
		p.Actions = append(p.Actions, a)
	}

	// Ring-level: many unrelated people linked, filed in a burst, or tied to a confirmed case.
	if ring != nil && ring.Size >= 3 && (len(ring.Traits.Indicators) >= 2 || ring.ConfirmedFraudCases > 0) {
		var open []string
		for _, m := range ring.Members {
			if m.ClaimID != rep.TargetClaim && len(m.Dispositions) == 0 {
				open = append(open, m.ClaimID)
			}
		}
		add(Action{Kind: KindHoldRingForReview, Priority: 1, Simulated: true,
			Title: fmt.Sprintf("Hold the %d open claims of %s for joint review (simulated)", len(open), ring.RingID),
			Rationale: fmt.Sprintf("The claim belongs to %s: %d claims by %d claimants at %d addresses, indicators: %s. Reviewing them together avoids paying one claim of a ring before the others are examined.",
				ring.RingID, ring.Size, ring.Traits.Claimants, ring.Traits.Addresses, strings.Join(ring.Traits.Indicators, ", ")),
			Claims: open})
	}

	if x, ok := f[scoring.FConfirmedFraud]; ok {
		add(Action{Kind: KindReviewPriorCase, Priority: 1,
			Title:     "Review the human-confirmed prior case(s) " + strings.Join(x.EvidenceIDs, ", "),
			Rationale: "A connected, older claim has a confirmed-fraud outcome; check whether the same identifiers and pattern recur.",
			Claims:    x.Claims, EvidencePath: pathTo(rep, x.Claims)})
	}
	if x, ok := f[scoring.FSharedPayout]; ok && !rep.HouseholdExplained {
		for _, e := range x.Entities {
			id := strings.TrimPrefix(e, "PayoutDestination:")
			add(Action{Kind: KindVerifyPayoutHolder, Priority: 2,
				Title:     fmt.Sprintf("Verify the account holder of payout account %s against every claimant who uses it", id),
				Rationale: "Different claimants at different addresses are paid into the same claimant account.",
				Claims:    x.Claims, Entities: []string{e}, EvidencePath: pathVia(rep, id)})
		}
	}
	if x, ok := f[scoring.FSharedDevice]; ok && !rep.HouseholdExplained {
		for _, e := range x.Entities {
			id := strings.TrimPrefix(e, "Device:")
			add(Action{Kind: KindReviewDeviceRecords, Priority: 3,
				Title:     fmt.Sprintf("Request submission/session records for device %s", id),
				Rationale: "One private device submitted claims for different claimants.",
				Claims:    x.Claims, Entities: []string{e}, EvidencePath: pathVia(rep, id)})
		}
	}
	if x, ok := f[scoring.FNewPolicy]; ok {
		add(Action{Kind: KindVerifyPolicyOrigins, Priority: 3,
			Title:     "Verify the applications of the recently started policies",
			Rationale: fmt.Sprintf("This claim and %d linked claim(s) were filed within %d days of their policies starting.", len(x.Claims), scoring.NewPolicyDays),
			Claims:    append([]string{rep.TargetClaim}, x.Claims...)})
	}
	if x, ok := f[scoring.FSharedContact]; ok && !rep.HouseholdExplained {
		add(Action{Kind: KindConfirmContactOwner, Priority: 4,
			Title:     "Confirm who owns the shared phone/e-mail",
			Rationale: "Different claimants listed the same contact point.",
			Claims:    x.Claims, Entities: x.Entities, EvidencePath: pathTo(rep, x.Claims)})
	}
	if rep.HouseholdExplained {
		add(Action{Kind: KindConfirmHousehold, Priority: 4,
			Title:     "Confirm the household relationship before escalating",
			Rationale: "Every shared account, device or phone is shared only with people at the claimant's address, which a joint account or family explains.",
			Claims:    claimsOf(f, scoring.FSharedPayout, scoring.FSharedDevice, scoring.FSharedContact, scoring.FSharedAddress)})
	}
	if len(p.Actions) == 0 {
		add(Action{Kind: KindProcessOrdinarily, Priority: 5,
			Title:     "No graph-based verification needed; continue ordinary claim handling",
			Rationale: outcomeReason(rep)})
	}
	sort.SliceStable(p.Actions, func(i, j int) bool { return p.Actions[i].Priority < p.Actions[j].Priority })
	if len(p.Actions) > maxActions {
		p.Actions = p.Actions[:maxActions]
	}
	return p
}

func outcomeReason(rep *scoring.Report) string {
	if rep.Investigation != nil && rep.Investigation.Message != "" {
		return rep.Investigation.Message
	}
	return "The graph shows no strong shared evidence for this claim."
}

func claimsOf(f map[string]scoring.Factor, names ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		for _, c := range f[n].Claims {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// pathVia returns the first reported path that goes through the given entity id.
func pathVia(rep *scoring.Report, entityID string) string {
	if rep.Investigation == nil {
		return ""
	}
	for _, rc := range rep.Investigation.RelatedClaims {
		for _, p := range rc.Paths {
			for _, s := range p.Steps {
				if s.EntityID == entityID {
					return p.Description
				}
			}
		}
	}
	return ""
}

// pathTo returns the first reported path to any of the given claims.
func pathTo(rep *scoring.Report, claims []string) string {
	if rep.Investigation == nil {
		return ""
	}
	want := map[string]bool{}
	for _, c := range claims {
		want[c] = true
	}
	for _, rc := range rep.Investigation.RelatedClaims {
		if want[rc.ClaimID] && len(rc.Paths) > 0 {
			return rc.Paths[0].Description
		}
	}
	return ""
}
