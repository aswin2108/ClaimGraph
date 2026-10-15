package investigate

// Evidence categories. A Step is classified by the kind of shared entity it goes
// through. Only the categories listed in ScoringCategories can earn score points;
// the others are shown as context and are worth 0.
const (
	CatPayout       = "payout"        // shared claimant payout destination
	CatDevice       = "device"        // shared non-public device
	CatContact      = "contact"       // shared claimant phone or email
	CatAddress      = "address"       // shared address
	CatRepairShop   = "repair_shop"   // shared repair shop
	CatPublicIP     = "public_ip"     // shared public IP address
	CatVendorPayout = "vendor_payout" // repair vendor's direct-settlement account (0 pts)
	CatPublicDevice = "public_device" // device flagged public (0 pts)
	CatPrivateIP    = "private_ip"    // company/private network IP (0 pts)
	CatTowProvider  = "tow_provider"  // context only (0 pts)
	CatUnknown      = "unknown"
)

// ScoringCategories are the six categories that can contribute points
// and that an investigator may exclude in a counterfactual.
var ScoringCategories = []string{CatPayout, CatDevice, CatContact, CatAddress, CatRepairShop, CatPublicIP}

// ValidCategory reports whether c is a category that can be excluded.
func ValidCategory(c string) bool {
	for _, s := range ScoringCategories {
		if s == c {
			return true
		}
	}
	return false
}

// SignalCategory classifies a step by its shared entity.
func SignalCategory(s Step) string {
	switch s.EntityType {
	case "PayoutDestination":
		if s.EntitySubtype == "claimant" {
			return CatPayout
		}
		return CatVendorPayout // never mistaken for a claimant payout
	case "Device":
		if s.EntityPublic != nil && *s.EntityPublic {
			return CatPublicDevice
		}
		return CatDevice
	case "ContactPoint":
		return CatContact
	case "Address":
		return CatAddress
	case "RepairShop":
		return CatRepairShop
	case "IPAddress":
		if s.EntityPublic != nil && *s.EntityPublic {
			return CatPublicIP
		}
		return CatPrivateIP
	case "TowProvider":
		return CatTowProvider
	}
	return CatUnknown
}
