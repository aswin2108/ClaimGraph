package graphalgo

import (
	"fmt"
	"strings"

	"github.com/FalkorDB/falkordb-go/v2"
)

// Alert levels.
const (
	AlertReview  = "review_recommended" // >= 2 ring indicators, or a confirmed case in the ring
	AlertContext = "context"            // part of a group, but nothing ring-like about it
)

// RingAlert is the intake-time summary of the ring a new claim joins.
type RingAlert struct {
	ClaimID             string     `json:"claim_id"`
	RingID              string     `json:"ring_id"`
	Size                int        `json:"size"`
	ConfirmedFraudCases int        `json:"confirmed_fraud_cases"`
	Traits              RingTraits `json:"traits"`
	Level               string     `json:"level"`
	Message             string     `json:"message"`
}

var indicatorText = map[string]string{
	IndicatorAddresses: "claimants at %d different addresses",
	IndicatorBurst:     "%d claims filed within 90 days",
	IndicatorNewPolicy: "%d claims on policies under 60 days old",
	IndicatorConfirmed: "%d human-confirmed prior case(s)",
}

// AlertFor returns the ring alert for a claim, or nil when the claim shares no strong
// identifier with another claim. It never changes the claim's score.
func AlertFor(g *falkordb.Graph, claimID string) (*RingAlert, error) {
	ring, err := RingSummaryOf(g, claimID)
	if err != nil {
		return nil, err
	}
	return AlertFromRing(ring), nil
}

// AlertFromRing builds the alert from an already computed ring (nil when there is no ring).
func AlertFromRing(ring *Ring) *RingAlert {
	if ring == nil || ring.Size < 2 {
		return nil
	}
	claimID := ring.TargetClaim
	a := &RingAlert{ClaimID: claimID, RingID: ring.RingID, Size: ring.Size, ConfirmedFraudCases: ring.ConfirmedFraudCases,
		Traits: ring.Traits, Level: AlertContext}
	if len(ring.Traits.Indicators) >= 2 || ring.ConfirmedFraudCases > 0 {
		a.Level = AlertReview
	}
	var parts []string
	for _, ind := range ring.Traits.Indicators {
		switch ind {
		case IndicatorAddresses:
			parts = append(parts, fmt.Sprintf(indicatorText[ind], ring.Traits.Addresses))
		case IndicatorBurst:
			parts = append(parts, fmt.Sprintf(indicatorText[ind], ring.Traits.Burst90))
		case IndicatorNewPolicy:
			parts = append(parts, fmt.Sprintf(indicatorText[ind], ring.Traits.NewPolicyClaims))
		case IndicatorConfirmed:
			parts = append(parts, fmt.Sprintf(indicatorText[ind], ring.ConfirmedFraudCases))
		}
	}
	a.Message = fmt.Sprintf("Claim %s joins %s: %d claims by %d claimants linked by strong shared identifiers.",
		claimID, ring.RingID, ring.Size, ring.Traits.Claimants)
	if len(parts) > 0 {
		a.Message += " Ring indicators: " + strings.Join(parts, "; ") + "."
	}
	if a.Level == AlertReview {
		a.Message += " Investigator review is recommended; this is a lead for verification, not a finding of fraud."
	} else {
		a.Message += " No ring indicators; shared identifiers like these are common in households."
	}
	return a
}
