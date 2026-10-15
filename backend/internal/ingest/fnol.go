// Package ingest validates synthetic auto-insurance FNOLs (First Notice of Loss)
// and writes them idempotently into FalkorDB.
package ingest

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	dateLayout = "2006-01-02"
	maxAmount  = 1_000_000 // sanity ceiling for a synthetic claim
	// policyPayoutLimit drives the demo-only "amount within limit" policy check.
	policyPayoutLimit = 50_000
)

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	vinPattern = regexp.MustCompile(`^[A-Za-z0-9]{17}$`)
)

// FNOL is a synthetic First Notice of Loss submission.
// Required: ClaimID, ClaimantID, ClaimantName, PolicyID + policy period/status, VIN,
// IncidentDate, FilingDate, Amount, PayoutAccountID + PayoutType, RepairShopID.
// Optional: TowProviderID, Contact, Address, Device, IP.
type FNOL struct {
	ClaimID          string `json:"claim_id"`
	ClaimantID       string `json:"claimant_id"`
	ClaimantName     string `json:"claimant_name"`
	PolicyID         string `json:"policy_id"`
	PolicyStart      string `json:"policy_start"`
	PolicyEnd        string `json:"policy_end"`
	PolicyStatus     string `json:"policy_status"` // "active" | "lapsed" | "cancelled"
	VIN              string `json:"vin"`
	IncidentDate     string `json:"incident_date"`
	FilingDate       string `json:"filing_date"`
	IncidentLocation string `json:"incident_location,omitempty"`
	LossType         string `json:"loss_type,omitempty"`
	Amount           int    `json:"amount"`
	PayoutAccountID  string `json:"payout_account_id"`
	PayoutType       string `json:"payout_type"` // "claimant" | "vendor_direct_settlement"
	RepairShopID     string `json:"repair_shop_id"`
	TowProviderID    string `json:"tow_provider_id,omitempty"`

	ContactType  string `json:"contact_type,omitempty"` // "phone" | "email"
	ContactValue string `json:"contact_value,omitempty"`
	AddressID    string `json:"address_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	DevicePublic bool   `json:"device_is_public,omitempty"`
	IPAddress    string `json:"ip_address,omitempty"`
}

// ValidationError lists every problem found in an FNOL.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid FNOL: " + strings.Join(e.Problems, "; ")
}

// Validate checks required fields, formats and cross-field rules.
// It never touches the database.
func (f *FNOL) Validate() error {
	var p []string
	req := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			p = append(p, name+" is required")
		}
	}
	id := func(name, val string) {
		if val != "" && !idPattern.MatchString(val) {
			p = append(p, name+" must be 1-64 chars of letters, digits, '-' or '_'")
		}
	}

	req("claim_id", f.ClaimID)
	req("claimant_id", f.ClaimantID)
	req("claimant_name", f.ClaimantName)
	req("policy_id", f.PolicyID)
	req("vin", f.VIN)
	req("payout_account_id", f.PayoutAccountID)
	req("repair_shop_id", f.RepairShopID)
	for name, val := range map[string]string{
		"claim_id": f.ClaimID, "claimant_id": f.ClaimantID, "policy_id": f.PolicyID,
		"payout_account_id": f.PayoutAccountID, "repair_shop_id": f.RepairShopID,
		"tow_provider_id": f.TowProviderID, "address_id": f.AddressID, "device_id": f.DeviceID,
	} {
		id(name, val)
	}

	for name, lim := range map[string]struct {
		val string
		max int
	}{
		"claimant_name":     {f.ClaimantName, 100},
		"incident_location": {f.IncidentLocation, 200},
		"loss_type":         {f.LossType, 50},
		"contact_value":     {f.ContactValue, 200},
	} {
		if len([]rune(lim.val)) > lim.max {
			p = append(p, fmt.Sprintf("%s must be at most %d characters", name, lim.max))
		}
	}

	if f.VIN != "" && !vinPattern.MatchString(f.VIN) {
		p = append(p, "vin must be 17 letters/digits")
	}
	if f.Amount <= 0 || f.Amount > maxAmount {
		p = append(p, fmt.Sprintf("amount must be between 1 and %d", maxAmount))
	}
	if f.PayoutType != "claimant" && f.PayoutType != "vendor_direct_settlement" {
		p = append(p, "payout_type must be 'claimant' or 'vendor_direct_settlement'")
	}
	switch f.PolicyStatus {
	case "active", "lapsed", "cancelled":
	default:
		p = append(p, "policy_status must be 'active', 'lapsed' or 'cancelled'")
	}

	dates := map[string]time.Time{}
	for name, val := range map[string]string{
		"policy_start": f.PolicyStart, "policy_end": f.PolicyEnd,
		"incident_date": f.IncidentDate, "filing_date": f.FilingDate,
	} {
		t, err := time.Parse(dateLayout, val)
		if err != nil {
			p = append(p, name+" must be a valid date (YYYY-MM-DD)")
			continue
		}
		dates[name] = t
	}
	if len(dates) == 4 {
		if dates["policy_end"].Before(dates["policy_start"]) {
			p = append(p, "policy_end must not be before policy_start")
		}
		if dates["filing_date"].Before(dates["incident_date"]) {
			p = append(p, "filing_date must not be before incident_date")
		}
	}

	// Optional contact: type and value must come together.
	if (f.ContactType != "") != (f.ContactValue != "") {
		p = append(p, "contact_type and contact_value must be supplied together")
	} else if f.ContactType != "" && f.ContactType != "phone" && f.ContactType != "email" {
		p = append(p, "contact_type must be 'phone' or 'email'")
	}
	if f.IPAddress != "" && net.ParseIP(f.IPAddress) == nil {
		p = append(p, "ip_address is not a valid IP")
	}

	if len(p) > 0 {
		sort.Strings(p) // map iteration above is unordered; keep messages deterministic
		return &ValidationError{Problems: p}
	}
	return nil
}

// PolicyCheck is one ordinary, deterministic check. These are a small demo,
// NOT an insurance rules engine.
type PolicyCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// PolicyChecks runs the ordinary single-claim checks. Call after Validate.
func (f *FNOL) PolicyChecks() []PolicyCheck {
	incident, _ := time.Parse(dateLayout, f.IncidentDate)
	start, _ := time.Parse(dateLayout, f.PolicyStart)
	end, _ := time.Parse(dateLayout, f.PolicyEnd)
	return []PolicyCheck{
		{"policy_active", f.PolicyStatus == "active"},
		{"incident_within_coverage_period", !incident.Before(start) && !incident.After(end)},
		{"amount_within_limit", f.Amount <= policyPayoutLimit},
	}
}

// PolicyCheckStatus is "passed" only if every ordinary check passes.
func PolicyCheckStatus(checks []PolicyCheck) string {
	for _, c := range checks {
		if !c.Passed {
			return "failed"
		}
	}
	return "passed"
}

// ValidID reports whether s is a safe synthetic identifier (same rule used for FNOL IDs).
func ValidID(s string) bool { return idPattern.MatchString(s) }

// IPIsPublic reports whether ip is a publicly routable address.
func IPIsPublic(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return !(parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() || parsed.IsUnspecified())
}
