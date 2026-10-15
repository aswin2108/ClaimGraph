package graphalgo

// Ring indicators.
//
// Size alone cannot tell a ring from a family: on the 20,000-claim synthetic book, 288
// legitimate groups (households, repeat claimants) have 6 or more claims. What separates
// coordinated groups is HOW their claims relate:
//
//	                                    legitimate groups (2,986)    planted rings (12)
//	distinct addresses among claimants  1.0 on average, never >= 4   4 to 14
//	most claims filed within 90 days    1.9 on average (max 6)       all of them (6 to 16)
//	claims on policies < 60 days old    18%                          100%
//
// (The rings are synthetic and were generated with these traits, so this is evidence about
// the method, not a real-world accuracy figure; the traits themselves are standard SIU
// review triggers.) Indicators order the ring list and are shown with each ring. They are
// review context and queue order only; they never change a claim's score.

import (
	"sort"
	"time"
)

const (
	IndicatorAddresses  = "spans_households" // claimants live at >= MinRingAddresses different addresses
	IndicatorBurst      = "filing_burst"     // >= MinRingBurst claims filed within RingBurstDays
	IndicatorNewPolicy  = "new_policies"     // >= half the claims were filed within NewPolicyDays of policy start
	IndicatorConfirmed  = "confirmed_case"   // a human-confirmed fraud case on a member claim
	MinRingAddresses    = 3
	MinRingBurst        = 4
	RingBurstDays       = 90
	RingNewPolicyDays   = 60
	indicatorsOrderNote = "rings with more indicators first (spans households, filing burst, new policies, confirmed case), then a confirmed case, more distinct addresses, larger burst, larger ring, shorter span, ring id (queue ordering, not a score)"
)

// RingTraits are the measured traits behind the indicators.
type RingTraits struct {
	Claimants       int      `json:"claimants"` // distinct claimants
	Addresses       int      `json:"addresses"` // distinct addresses of those claimants
	Burst90         int      `json:"max_claims_in_90_days"`
	NewPolicyClaims int      `json:"claims_on_new_policies"`
	Indicators      []string `json:"indicators"`
}

// traitsOf computes the traits from per-claim facts.
func traitsOf(size int, filed []string, claimants, addresses map[string]bool, newPolicy, confirmed int) RingTraits {
	t := RingTraits{Claimants: len(claimants), Addresses: len(addresses), NewPolicyClaims: newPolicy, Indicators: []string{}}
	sorted := append([]string(nil), filed...)
	sort.Strings(sorted)
	for i := range sorted {
		n := 0
		for j := i; j < len(sorted) && daysBetween(sorted[i], sorted[j]) <= RingBurstDays; j++ {
			n++
		}
		if n > t.Burst90 {
			t.Burst90 = n
		}
	}
	if t.Addresses >= MinRingAddresses {
		t.Indicators = append(t.Indicators, IndicatorAddresses)
	}
	if t.Burst90 >= MinRingBurst {
		t.Indicators = append(t.Indicators, IndicatorBurst)
	}
	if size > 0 && newPolicy*2 >= size {
		t.Indicators = append(t.Indicators, IndicatorNewPolicy)
	}
	if confirmed > 0 {
		t.Indicators = append(t.Indicators, IndicatorConfirmed)
	}
	return t
}

// newPolicy reports whether an incident happened within RingNewPolicyDays of policy start.
func newPolicy(incident, policyStart string) bool {
	a, err1 := time.Parse("2006-01-02", policyStart)
	b, err2 := time.Parse("2006-01-02", incident)
	if err1 != nil || err2 != nil {
		return false
	}
	d := int(b.Sub(a).Hours() / 24)
	return d >= 0 && d <= RingNewPolicyDays
}

// lessRing is the review order of the ring list.
func lessRing(a, b RingSummary) bool {
	if len(a.Indicators) != len(b.Indicators) {
		return len(a.Indicators) > len(b.Indicators)
	}
	if (a.ConfirmedFraudCases > 0) != (b.ConfirmedFraudCases > 0) {
		return a.ConfirmedFraudCases > 0
	}
	if a.Addresses != b.Addresses {
		return a.Addresses > b.Addresses
	}
	if a.Burst90 != b.Burst90 {
		return a.Burst90 > b.Burst90
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	if a.SpanDays != b.SpanDays {
		return a.SpanDays < b.SpanDays
	}
	return a.RingID < b.RingID
}
