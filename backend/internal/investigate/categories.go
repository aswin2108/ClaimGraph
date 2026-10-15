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

// InfrastructureMinClaims: an identifier in a weak or context-only category (repair shop,
// public IP, vendor settlement account, public device, private IP) that MORE than this many
// claims use is shared infrastructure (a busy shop, a mobile-carrier NAT address, a
// kiosk). It still counts once as weak evidence about the target, but it is not treated
// as a link to the individual claims that also use it: those claims are not listed as
// related, earn no breadth or timing points, and are not expanded in multi-hop paths.
// Strong categories (payout, device, contact, address) are never treated as
// infrastructure: a payout account shared by 30 claims is a strong signal, not noise.
// The value is above every T01-T30 fixture (at most 6 claims share an identifier there).
const InfrastructureMinClaims = 20

// WeakOrContext reports whether a category can be infrastructure.
func WeakOrContext(category string) bool {
	switch category {
	case CatRepairShop, CatPublicIP, CatVendorPayout, CatPublicDevice, CatPrivateIP, CatTowProvider:
		return true
	}
	return false
}

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
