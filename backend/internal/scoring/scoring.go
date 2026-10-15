// Package scoring turns investigation evidence into a deterministic, explainable
// investigation-priority score (0-100). The score ranks claims for HUMAN review;
// it is not the probability of fraud. Same evidence + same rules => same result.
package scoring

import (
	"fmt"
	"sort"
	"time"

	"claimgraph/internal/investigate"
)

// Points per evidence signal (each category counts at most once per target claim).
const (
	PtsPayout      = 25
	PtsDevice      = 20
	PtsContact     = 12
	PtsAddress     = 10
	PtsRepairShop  = 8
	PtsPublicIP    = 3
	PtsPerClaim    = 5  // each additional distinct connected claim...
	PtsClaimsCap   = 15 // ...capped here
	PtsTemporal    = 10 // a related claim filed within TemporalDays
	PtsConfirmed   = 15 // older, human-confirmed fraud case among related claims
	PtsReinforce   = 10 // >= 2 independent strong categories, not all explained by one household
	PtsBurst       = 10 // >= BurstMinClaims other connected claims filed within BurstDays
	PtsNewPolicy   = 10 // this claim and a connected claim are both on new policies
	TemporalDays   = 7
	BurstDays      = 90
	BurstMinClaims = 3
	NewPolicyDays  = 60 // a policy is "new" when the incident is at most this many days after it started
	MaxScore       = 100
	BandReviewMin  = 30
	BandHighMin    = 60
	DispConfirmed  = "confirmed_fraud"
	BandLow        = "Low"
	BandReview     = "Review"
	BandHigh       = "High Priority"
	Disclaimer     = "This is an investigation-priority score for human review. It is not a fraud determination."
)

// Factor names (stable identifiers used by the UI, agent and counterfactuals).
const (
	FSharedPayout   = "shared_claimant_payout"
	FSharedDevice   = "shared_non_public_device"
	FSharedContact  = "shared_contact"
	FSharedAddress  = "shared_address"
	FSharedRepair   = "shared_repair_shop"
	FSharedPublicIP = "shared_public_ip"
	FClaimBreadth   = "connected_claims_breadth"
	FTemporal       = "temporal_proximity"
	FConfirmedFraud = "confirmed_fraud_history"
	FReinforcement  = "multi_signal_reinforcement"
	FBurst          = "claim_burst"
	FNewPolicy      = "new_policy_cluster"
)

// signalDefs maps an evidence category to its factor name and points, in report order.
var signalDefs = []struct {
	Category, Factor string
	Points           int
	Label            string
}{
	{investigate.CatPayout, FSharedPayout, PtsPayout, "claimant payout destination shared with other claims"},
	{investigate.CatDevice, FSharedDevice, PtsDevice, "non-public device shared with other claims"},
	{investigate.CatContact, FSharedContact, PtsContact, "claimant phone/email shared with other claims"},
	{investigate.CatAddress, FSharedAddress, PtsAddress, "address shared with other claims"},
	{investigate.CatRepairShop, FSharedRepair, PtsRepairShop, "repair shop shared with other claims"},
	{investigate.CatPublicIP, FSharedPublicIP, PtsPublicIP, "public IP address shared with other claims"},
}

// strongCategories are the independent categories that trigger the reinforcement bonus.
var strongCategories = map[string]bool{
	investigate.CatPayout: true, investigate.CatDevice: true,
	investigate.CatContact: true, investigate.CatAddress: true,
}

// obs accumulates what was observed for one evidence category.
type obs struct {
	claims, entities, evidence map[string]bool
}

// PriorCase is a previously recorded investigation outcome on a related claim.
type PriorCase struct {
	CaseID      string `json:"case_id"`
	ClaimID     string `json:"claim_id"`
	Disposition string `json:"disposition"` // confirmed_fraud | not_substantiated | needs_info
	Notes       string `json:"notes,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// Factor is one traceable score contribution.
type Factor struct {
	Name        string   `json:"name"`
	Points      int      `json:"points"`
	Reason      string   `json:"reason"`
	Claims      []string `json:"claims,omitempty"`       // related claim IDs behind this factor
	Entities    []string `json:"entities,omitempty"`     // shared entities behind this factor
	EvidenceIDs []string `json:"evidence_ids,omitempty"` // evidence references on the edges
}

// Report is the deterministic investigation outcome for one target claim.
type Report struct {
	TargetClaim      string              `json:"target_claim"`
	Score            int                 `json:"score"`
	Band             string              `json:"band"`
	BandMeaning      string              `json:"band_meaning"`
	ConnectedClaims  int                 `json:"connected_claims"`
	Factors          []Factor            `json:"factors"`
	PriorCases       []PriorCase         `json:"prior_cases"`
	Uncertainty      []string            `json:"uncertainty"`
	ExcludedCategory string              `json:"excluded_category,omitempty"`
	Investigation    *investigate.Result `json:"investigation"`
	Disclaimer       string              `json:"disclaimer"`
	// HouseholdExplained: every strong identifier is shared only with people at the
	// claimant's own address (a joint account, a family phone or device), so the
	// multi-signal bonus is withheld. The category points still count.
	HouseholdExplained bool `json:"household_explained"`
}

// Band classifies a score.
func Band(score int) (band, meaning string) {
	switch {
	case score >= BandHighMin:
		return BandHigh, "Prioritize human SIU investigation."
	case score >= BandReviewMin:
		return BandReview, "Additional examination may be warranted."
	default:
		return BandLow, "Insufficient evidence to prioritize this claim on the basis of this graph analysis."
	}
}

// Compute scores an investigation result. It is a pure function: no database access.
//
// Rules used:
//   - Signal categories come from the target's DIRECT (1-hop) connections only; each
//     category counts at most once, however many claims share it.
//   - Breadth and temporal proximity use every connected claim within the traversal depth.
//   - The confirmed-fraud bonus needs a case with disposition confirmed_fraud on a related
//     claim filed before the target ("older"). Other dispositions never score.
func Compute(res *investigate.Result, cases []PriorCase) *Report {
	return ComputeWithPolicies(res, cases, nil)
}

// ComputeWithPolicies is Compute with policy ages: policyDays maps a claim id (the target
// and connected claims) to the number of days between its policy start and its incident.
// Claims without an entry never trigger the new-policy signal.
func ComputeWithPolicies(res *investigate.Result, cases []PriorCase, policyDays map[string]int) *Report {
	r := &Report{
		TargetClaim: res.TargetClaim, ExcludedCategory: res.ExcludedCategory,
		Investigation: res, Factors: []Factor{}, PriorCases: cases, Uncertainty: []string{},
		Disclaimer: Disclaimer,
	}
	connected := connectedClaims(res)
	r.ConnectedClaims = len(connected)
	if r.PriorCases == nil {
		r.PriorCases = []PriorCase{}
	}

	// 1. Signal categories from direct connections. A related claim is co-resident when its
	// claimant shares the target claimant's address (one of its direct paths is an address).
	seen := map[string]*obs{}
	coResident := map[string]bool{}
	for _, rc := range res.RelatedClaims {
		if rc.Hops != 1 {
			continue
		}
		for _, p := range rc.Paths {
			if investigate.SignalCategory(p.Steps[0]) == investigate.CatAddress {
				coResident[rc.ClaimID] = true
			}
		}
	}
	for _, rc := range res.RelatedClaims {
		if rc.Hops != 1 {
			continue
		}
		for _, p := range rc.Paths {
			st := p.Steps[0]
			cat := investigate.SignalCategory(st)
			o := seen[cat]
			if o == nil {
				o = &obs{map[string]bool{}, map[string]bool{}, map[string]bool{}}
				seen[cat] = o
			}
			o.claims[rc.ClaimID] = true
			o.entities[st.EntityType+":"+st.EntityID] = true
			for _, e := range []string{st.FromEvidence, st.ToEvidence} {
				if e != "" {
					o.evidence[e] = true
				}
			}
		}
	}

	// Shared infrastructure (a weak/context identifier used by more than
	// investigate.InfrastructureMinClaims claims) is evidence about the target itself: its
	// category counts once, like any direct signal, but its many users are not connected
	// claims, so it adds nothing to breadth, timing or the confirmed-fraud check.
	for _, l := range res.InfrastructureLinks {
		o := seen[l.Category]
		if o == nil {
			o = &obs{map[string]bool{}, map[string]bool{}, map[string]bool{}}
			seen[l.Category] = o
		}
		o.entities[l.EntityType+":"+l.EntityID] = true
		if l.EvidenceID != "" {
			o.evidence[l.EvidenceID] = true
		}
	}

	total := 0
	strong := 0
	for _, d := range signalDefs {
		o := seen[d.Category]
		if o == nil {
			continue
		}
		r.Factors = append(r.Factors, Factor{
			Name: d.Factor, Points: d.Points, Reason: d.Label,
			Claims: keys(o.claims), Entities: keys(o.entities), EvidenceIDs: keys(o.evidence),
		})
		total += d.Points
		if strongCategories[d.Category] {
			strong++
		}
	}

	// 2. Breadth: +5 per distinct connected claim, capped at +15.
	if n := len(connected); n > 0 {
		pts := n * PtsPerClaim
		if pts > PtsClaimsCap {
			pts = PtsClaimsCap
		}
		ids := make([]string, 0, n)
		for _, rc := range connected {
			ids = append(ids, rc.ClaimID)
		}
		sort.Strings(ids)
		if len(ids) > maxFactorClaims {
			ids = ids[:maxFactorClaims]
		}
		r.Factors = append(r.Factors, Factor{
			Name: FClaimBreadth, Points: pts, Claims: ids,
			Reason: fmt.Sprintf("%d distinct connected claim(s), %d pts each, capped at %d", n, PtsPerClaim, PtsClaimsCap),
		})
		total += pts
	}

	// 3. Temporal proximity: any connected claim filed within 7 days of the target.
	if target, ok := parseDate(res.TargetFilingDate); ok {
		var near []string
		for _, rc := range connected {
			if d, ok := parseDate(rc.FilingDate); ok && absDays(target, d) <= TemporalDays {
				near = append(near, rc.ClaimID)
			}
		}
		if len(near) > 0 {
			sort.Strings(near)
			r.Factors = append(r.Factors, Factor{
				Name: FTemporal, Points: PtsTemporal, Claims: near,
				Reason: fmt.Sprintf("a connected claim was filed within %d days of this claim", TemporalDays),
			})
			total += PtsTemporal
		}
	}

	// 4. Older, human-confirmed fraud case on a related claim.
	if f := confirmedFraudFactor(res, cases); f != nil {
		r.Factors = append(r.Factors, *f)
		total += f.Points
	}

	// 5. Multi-signal reinforcement, unless one household explains every strong link: a
	// payout account, device or phone shared only with people at the claimant's own address
	// is what a joint account or a family looks like. A ring shares them across addresses.
	unexplained := false
	for _, cat := range []string{investigate.CatPayout, investigate.CatDevice, investigate.CatContact} {
		if o := seen[cat]; o != nil {
			for c := range o.claims {
				if !coResident[c] {
					unexplained = true
				}
			}
		}
	}
	r.HouseholdExplained = strong >= 1 && !unexplained
	if strong >= 2 && !r.HouseholdExplained {
		r.Factors = append(r.Factors, Factor{
			Name: FReinforcement, Points: PtsReinforce,
			Reason: fmt.Sprintf("%d independent strong categories (payout, device, contact, address)", strong),
		})
		total += PtsReinforce
	}

	// 6. Claim burst: several STRONGLY connected claims (every step payout, device, contact
	// or address) filed around the same time. Weak links such as a shared shop never count.
	if target, ok := parseDate(res.TargetFilingDate); ok {
		var burst []string
		for _, rc := range connected {
			if !rc.Strong {
				continue
			}
			if d, ok := parseDate(rc.FilingDate); ok && absDays(target, d) <= BurstDays {
				burst = append(burst, rc.ClaimID)
			}
		}
		if len(burst) >= BurstMinClaims {
			sort.Strings(burst)
			if len(burst) > maxFactorClaims {
				burst = burst[:maxFactorClaims]
			}
			r.Factors = append(r.Factors, Factor{Name: FBurst, Points: PtsBurst, Claims: burst,
				Reason: fmt.Sprintf("%d or more claims linked by strong evidence were filed within %d days of this claim", BurstMinClaims, BurstDays)})
			total += PtsBurst
		}
	}

	// 7. New-policy cluster: this claim and at least one connected claim were filed soon after
	// their policies started (claims shortly after inception are a common review trigger).
	if d, ok := policyDays[res.TargetClaim]; ok && d >= 0 && d <= NewPolicyDays {
		var newer []string
		for _, rc := range connected {
			if !rc.Strong {
				continue
			}
			if pd, ok := policyDays[rc.ClaimID]; ok && pd >= 0 && pd <= NewPolicyDays {
				newer = append(newer, rc.ClaimID)
			}
		}
		if len(newer) > 0 {
			sort.Strings(newer)
			if len(newer) > maxFactorClaims {
				newer = newer[:maxFactorClaims]
			}
			r.Factors = append(r.Factors, Factor{Name: FNewPolicy, Points: PtsNewPolicy, Claims: newer,
				Reason: fmt.Sprintf("this claim and claims linked to it by strong evidence were filed within %d days of their policies starting", NewPolicyDays)})
			total += PtsNewPolicy
		}
	}

	if total > MaxScore {
		total = MaxScore
	}
	r.Score = total
	r.Band, r.BandMeaning = Band(total)
	r.Uncertainty = uncertaintyNotes(seen, cases, len(connected)+len(res.InfrastructureLinks))
	if r.HouseholdExplained {
		r.Uncertainty = append(r.Uncertainty,
			"Every shared payout account, device or phone is shared only with people at the claimant's own address, which a household or joint account explains; the multi-signal bonus was not applied.")
	}
	for _, l := range res.InfrastructureLinks {
		r.Uncertainty = append(r.Uncertainty, fmt.Sprintf(
			"%s %s is shared infrastructure used by %d other claims; it counts once as weak evidence and those claims are not treated as connections.",
			l.EntityType, l.EntityID, l.OtherClaims))
	}
	return r
}

// maxFactorClaims bounds the claim ids listed on one factor (the count is in the reason).
const maxFactorClaims = 50

// connectedClaims is every connected claim. Results built by hand (tests) may only have
// RelatedClaims.
func connectedClaims(res *investigate.Result) []investigate.ConnectedClaim {
	if res.Connected != nil || len(res.RelatedClaims) == 0 {
		return res.Connected
	}
	out := make([]investigate.ConnectedClaim, len(res.RelatedClaims))
	for i, rc := range res.RelatedClaims {
		out[i] = investigate.ConnectedClaim{ClaimID: rc.ClaimID, Hops: rc.Hops, FilingDate: rc.FilingDate,
			Strong: investigate.StrongPath(rc)}
	}
	return out
}

func confirmedFraudFactor(res *investigate.Result, cases []PriorCase) *Factor {
	target, tok := parseDate(res.TargetFilingDate)
	filed := map[string]string{}
	for _, rc := range connectedClaims(res) {
		filed[rc.ClaimID] = rc.FilingDate
	}
	var claims, ids []string
	for _, c := range cases {
		if c.Disposition != DispConfirmed {
			continue
		}
		d, dok := parseDate(filed[c.ClaimID])
		if !tok || !dok || !d.Before(target) {
			continue // not an "older" case
		}
		claims = append(claims, c.ClaimID)
		ids = append(ids, c.CaseID)
	}
	if len(claims) == 0 {
		return nil
	}
	sort.Strings(claims)
	sort.Strings(ids)
	return &Factor{
		Name: FConfirmedFraud, Points: PtsConfirmed, Claims: claims, EvidenceIDs: ids,
		Reason: "connected to an older claim with a human-confirmed fraud outcome",
	}
}

// uncertaintyNotes lists innocent explanations an investigator should weigh.
// They are fixed text keyed by what was found, so they never invent facts.
func uncertaintyNotes(seen map[string]*obs, cases []PriorCase, related int) []string {
	notes := []string{}
	if related == 0 {
		return append(notes, "No supporting connection to other claims was found, so this graph analysis cannot prioritize the claim.")
	}
	// Fixed order, only for categories actually observed.
	explain := []struct{ cat, note string }{
		{investigate.CatPayout, "A shared payout destination can be legitimate, e.g. a joint account used by household members."},
		{investigate.CatDevice, "A shared device can be legitimate, e.g. family members or a shared work phone."},
		{investigate.CatContact, "A shared phone or email can be legitimate, e.g. family members or a shared business line."},
		{investigate.CatAddress, "A shared address can be legitimate, e.g. a household, apartment building or business premises."},
		{investigate.CatRepairShop, "A shared repair shop is weak evidence on its own; busy shops serve many unrelated customers."},
		{investigate.CatPublicIP, "A shared public IP is weak evidence; it can be a shared network such as a carrier, library or office."},
		{investigate.CatPrivateIP, "A shared private/company-network IP is not evidence of coordination on its own."},
		{investigate.CatVendorPayout, "A repair vendor's direct-settlement account is not a claimant payout destination and earns no payout points."},
		{investigate.CatTowProvider, "A shared tow provider is context only and earns no points."},
	}
	for _, e := range explain {
		if seen[e.cat] != nil {
			notes = append(notes, e.note)
		}
	}
	for _, c := range cases {
		switch c.Disposition {
		case "not_substantiated":
			notes = append(notes, fmt.Sprintf("Prior case %s on %s was not substantiated; it earns no score and is context only.", c.CaseID, c.ClaimID))
		case "needs_info":
			notes = append(notes, fmt.Sprintf("Prior case %s on %s is unresolved (needs more information); it earns no score.", c.CaseID, c.ClaimID))
		}
	}
	return append(notes, "Connections are leads for verification, not proof of wrongdoing.")
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

func absDays(a, b time.Time) int {
	d := int(a.Sub(b).Hours() / 24)
	if d < 0 {
		d = -d
	}
	return d
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
