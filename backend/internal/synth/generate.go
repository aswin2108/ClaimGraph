// Package synth generates and bulk-loads a large, fully synthetic auto-insurance claim
// book with realistic noise and a few planted rings, for scale and latency evidence.
//
// Everything is synthetic: names come from short generic lists, phone numbers use the
// fictional 555 range, e-mails use the reserved example.test domain, and IP addresses
// come only from the documentation ranges (198.51.100.0/24, 2001:db8::/32) and private
// 10.0.0.0/8. Generation is deterministic for a given Config.
//
// Noise that must NOT look like a ring (hard negatives at scale):
//   - a long tail of repair shops where the busiest serves several percent of all claims;
//   - repair-shop direct-settlement (vendor) accounts shared by thousands of claims;
//   - public kiosk devices and mobile-carrier NAT addresses shared by many claimants;
//   - households sharing an address, a home IP, sometimes a phone or a joint account;
//   - repeat claimants (the same person filing two or three claims over two years);
//   - a few company networks (private IPs) shared by colleagues.
//
// Planted rings (ground truth, written only to the manifest, never into the graph):
// groups of synthetic identities that share claimant payout accounts, private devices,
// phones and addresses within a short filing window. Shapes: "mule" (shared payout
// accounts), "device_farm" (shared devices), "chain" (each claim linked to the next by
// a different identifier, so the ring is longer than the 3-transition investigation) and
// "mixed". Ring 0 is the demo showcase: a chain whose newest claim is withheld from the
// load and written out as a ready-to-submit FNOL.
package synth

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"claimgraph/internal/ingest"
)

const day = 24 * time.Hour

// Config controls the generated book.
type Config struct {
	Claims int       // approximate total number of loaded claims (legitimate + ring)
	Rings  int       // planted rings (>= 1; ring 0 is the showcase chain)
	Seed   int64     // RNG seed; same Config => same dataset
	Start  time.Time // first filing date
	End    time.Time // last filing date (the showcase claim is filed on End)
}

// DefaultConfig is the book used for the demo and the benchmark.
func DefaultConfig() Config {
	return Config{Claims: 20000, Rings: 12, Seed: 20261015,
		Start: time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
}

// Case is a prior investigation outcome to load with the book.
type Case struct {
	ClaimID     string `json:"claim_id"`
	Disposition string `json:"disposition"`
	Notes       string `json:"notes"`
	Timestamp   string `json:"timestamp"` // RFC3339 UTC, like disposition.Recorder
}

// PlantedRing is ground truth for evaluation. It is never written into the graph.
type PlantedRing struct {
	Index         int      `json:"index"`
	Shape         string   `json:"shape"`
	Claims        []string `json:"claims"`    // loaded claims only (the showcase claim is listed in Showcase)
	Claimants     []string `json:"claimants"` // synthetic identities used by the ring
	FirstFiled    string   `json:"first_filed"`
	LastFiled     string   `json:"last_filed"`
	ConfirmedCase string   `json:"confirmed_case_claim,omitempty"` // older member with a human-confirmed case
}

// Showcase is the demo scenario: a claim that is not loaded, to be submitted live.
type Showcase struct {
	RingIndex   int          `json:"ring_index"`
	DemoClaimID string       `json:"demo_claim_id"`
	DemoFNOL    *ingest.FNOL `json:"demo_fnol"`
	RingSize    int          `json:"ring_size_after_submission"`
}

// Stats summarizes the generated book.
type Stats struct {
	Claims            int    `json:"claims"`
	LegitClaims       int    `json:"legit_claims"`
	RingClaims        int    `json:"ring_claims"`
	Claimants         int    `json:"claimants"`
	Households        int    `json:"households"`
	RepairShops       int    `json:"repair_shops"`
	BusiestShop       string `json:"busiest_shop"`
	BusiestShopClaims int    `json:"busiest_shop_claims"`
	VendorPaidClaims  int    `json:"vendor_paid_claims"`
	KioskClaims       int    `json:"kiosk_device_claims"`
	CarrierIPClaims   int    `json:"carrier_ip_claims"`
	PriorCases        int    `json:"prior_cases"`
}

// Dataset is a generated book.
type Dataset struct {
	Config   Config         `json:"config"`
	FNOLs    []*ingest.FNOL `json:"-"` // in filing order
	Cases    []Case         `json:"-"`
	Rings    []PlantedRing  `json:"planted_rings"`
	Showcase Showcase       `json:"showcase"`
	Stats    Stats          `json:"stats"`
}

var (
	firstNames = []string{"Alex", "Sam", "Jordan", "Taylor", "Morgan", "Casey", "Riley", "Jamie", "Avery", "Quinn",
		"Robin", "Drew", "Jesse", "Cameron", "Rowan", "Skyler", "Emerson", "Hayden", "Reese", "Parker",
		"Dakota", "Kendall", "Logan", "Peyton", "Sage", "Remy", "Blake", "Charlie", "Finley", "Harper"}
	lastNames = []string{"Smith", "Garcia", "Chen", "Patel", "Okafor", "Novak", "Silva", "Kowalski", "Nguyen", "Haddad",
		"Fischer", "Rossi", "Tanaka", "Moreau", "Larsen", "Ivanova", "Mensah", "Costa", "Murphy", "Sato",
		"Weber", "Lopez", "Kim", "Ali", "Brown", "Jensen", "Dubois", "Singh", "Reyes", "Walsh"}
	lossTypes  = []string{"collision", "collision", "collision", "collision", "theft", "vandalism", "weather", "glass", "glass"}
	cities     = []string{"Synthetic City", "Mocksville", "Testford", "Sampleton", "Fakebury", "Demo Heights"}
	shapeOrder = []string{"mule", "device_farm", "mixed", "chain"}
)

// person is one synthetic claimant identity.
type person struct {
	id, name, phone, contactType, account, device, vin, address, homeIP string
}

// draft is a claim before ids are assigned (ids follow filing order).
type draft struct {
	f    ingest.FNOL
	ring int // -1 for legitimate claims
	key  float64
	idx  int
}

type gen struct {
	cfg        Config
	r          *rand.Rand
	shopCDF    []float64
	people     int
	households int
	accounts   int
	devices    int
	policies   int
}

// Generate builds a dataset. It panics only on an invalid Config (programming error).
func Generate(cfg Config) *Dataset {
	if cfg.Claims < 50 || cfg.Rings < 1 || !cfg.End.After(cfg.Start.Add(200*day)) {
		panic("synth: invalid config (need >= 50 claims, >= 1 ring, a window > 200 days)")
	}
	g := &gen{cfg: cfg, r: rand.New(rand.NewSource(cfg.Seed))}
	nShops := maxInt(20, cfg.Claims/80)
	// Long-tail popularity: weight 1/(k+3). The busiest shop gets ~7% of claims.
	total := 0.0
	for k := 0; k < nShops; k++ {
		total += 1 / float64(k+3)
		g.shopCDF = append(g.shopCDF, total)
	}
	for k := range g.shopCDF {
		g.shopCDF[k] /= total
	}

	ds := &Dataset{Config: cfg}
	var drafts []*draft

	// ---- planted rings first, so the legitimate book fills the remaining volume ----
	ringDrafts := make([][]*draft, cfg.Rings)
	ringPeople := make([][]string, cfg.Rings)
	for i := 0; i < cfg.Rings; i++ {
		shape := "chain"
		size := 11 // showcase: 10 loaded claims + the withheld demo claim
		if i > 0 {
			shape = shapeOrder[(i-1)%len(shapeOrder)]
			size = 6 + g.r.Intn(11)
		}
		rd, ps := g.ring(i, shape, size)
		ringDrafts[i], ringPeople[i] = rd, ps
		drafts = append(drafts, rd...)
	}
	ringClaims := len(drafts)

	// ---- legitimate book ----
	legitTarget := cfg.Claims - (ringClaims - 1) // the showcase claim is not loaded
	for legit := 0; legit < legitTarget; {
		for _, d := range g.household(nShops) {
			if legit < legitTarget {
				drafts = append(drafts, d)
				legit++
			}
		}
	}

	// ---- ids follow filing order; the showcase claim is filed last ----
	showRing := ringDrafts[0]
	demo := showRing[len(showRing)-1] // the chain's last claim (built with the latest date)
	for i, d := range drafts {
		d.idx = i
		d.key = g.r.Float64()
	}
	sort.SliceStable(drafts, func(i, j int) bool {
		a, b := drafts[i], drafts[j]
		if a == demo || b == demo {
			return b == demo && a != demo
		}
		if a.f.FilingDate != b.f.FilingDate {
			return a.f.FilingDate < b.f.FilingDate
		}
		return a.key < b.key
	})
	for i, d := range drafts {
		d.f.ClaimID = fmt.Sprintf("CLM-%06d", 100001+i)
	}

	for _, d := range drafts {
		if d == demo {
			continue
		}
		f := d.f
		ds.FNOLs = append(ds.FNOLs, &f)
	}
	demoF := demo.f
	ds.Showcase = Showcase{RingIndex: 0, DemoClaimID: demoF.ClaimID, DemoFNOL: &demoF, RingSize: len(showRing)}

	// ---- planted ring manifest and confirmed prior cases (memory) ----
	for i, rd := range ringDrafts {
		pr := PlantedRing{Index: i, Shape: shapeOf(i), Claimants: uniq(ringPeople[i])}
		var loaded []*draft
		for _, d := range rd {
			if d != demo {
				loaded = append(loaded, d)
				pr.Claims = append(pr.Claims, d.f.ClaimID)
			}
		}
		sort.Strings(pr.Claims)
		sort.Slice(loaded, func(a, b int) bool { return loaded[a].f.FilingDate < loaded[b].f.FilingDate })
		pr.FirstFiled, pr.LastFiled = loaded[0].f.FilingDate, loaded[len(loaded)-1].f.FilingDate
		// The showcase ring and every other ring carry a human-confirmed case on their oldest claim.
		if i == 0 || i%2 == 1 {
			oldest := loaded[0]
			pr.ConfirmedCase = oldest.f.ClaimID
			ds.Cases = append(ds.Cases, Case{ClaimID: oldest.f.ClaimID, Disposition: "confirmed_fraud",
				Notes: "synthetic prior review", Timestamp: tsAfter(oldest.f.FilingDate, 30)})
		}
		ds.Rings = append(ds.Rings, pr)
	}

	// ---- prior outcomes on legitimate claims (never confirmed fraud) ----
	cutoff := cfg.End.Add(-60 * day).Format("2006-01-02")
	for _, d := range drafts {
		if d.ring >= 0 || d.f.FilingDate >= cutoff || g.r.Float64() >= 0.015 {
			continue
		}
		disp := "not_substantiated"
		if g.r.Float64() < 0.3 {
			disp = "needs_info"
		}
		ds.Cases = append(ds.Cases, Case{ClaimID: d.f.ClaimID, Disposition: disp, Notes: "synthetic prior review",
			Timestamp: tsAfter(d.f.FilingDate, 20+g.r.Intn(40))})
	}
	sort.Slice(ds.Cases, func(i, j int) bool { return ds.Cases[i].ClaimID < ds.Cases[j].ClaimID })

	ds.Stats = stats(ds, ringClaims-1, nShops)
	return ds
}

func shapeOf(i int) string {
	if i == 0 {
		return "chain"
	}
	return shapeOrder[(i-1)%len(shapeOrder)]
}

// newPerson creates a synthetic identity with its own account, device, vehicle and phone.
func (g *gen) newPerson(address, homeIP string) person {
	g.people++
	g.accounts++
	g.devices++
	p := person{
		id:      fmt.Sprintf("DRV-%06d", g.people),
		name:    firstNames[g.r.Intn(len(firstNames))] + " " + lastNames[g.r.Intn(len(lastNames))],
		account: fmt.Sprintf("ACCT-%07d", g.accounts),
		device:  fmt.Sprintf("DEV-%07d", g.devices),
		vin:     fmt.Sprintf("SYN%014d", g.people),
		address: address, homeIP: homeIP,
	}
	if g.r.Float64() < 0.3 {
		p.contactType, p.phone = "email", fmt.Sprintf("c%06d@example.test", g.people)
	} else {
		p.contactType, p.phone = "phone", fmt.Sprintf("+1-555-%07d", g.people)
	}
	return p
}

// household generates one household of 1-4 claimants and their legitimate claims.
func (g *gen) household(nShops int) []*draft {
	g.households++
	h := g.households
	size := pick(g.r, []float64{0.5, 0.3, 0.12, 0.08}) + 1
	address := fmt.Sprintf("ADDR-%06d", h)
	homeIP := fmt.Sprintf("2001:db8:%x:%x::1", h>>16, h&0xffff)
	var members []person
	for i := 0; i < size; i++ {
		members = append(members, g.newPerson(address, homeIP))
	}
	if size >= 2 && g.r.Float64() < 0.45 { // joint claimant account
		g.accounts++
		joint := fmt.Sprintf("ACCT-%07d", g.accounts)
		for i := range members {
			members[i].account = joint
		}
	}
	if size >= 2 && g.r.Float64() < 0.15 { // a shared household phone
		members[1].contactType, members[1].phone = members[0].contactType, members[0].phone
	}
	var out []*draft
	for _, p := range members {
		n := pick(g.r, []float64{0.78, 0.16, 0.06}) + 1 // repeat claimants
		for k := 0; k < n; k++ {
			out = append(out, g.legitClaim(p, nShops))
		}
	}
	return out
}

func (g *gen) legitClaim(p person, nShops int) *draft {
	span := int(g.cfg.End.Sub(g.cfg.Start)/day) - 1
	filed := g.cfg.Start.Add(time.Duration(g.r.Intn(span)) * day)
	incident := filed.Add(-time.Duration(g.r.Intn(8)) * day)
	shop := g.shop()
	f := g.baseFNOL(p, filed, incident, shop)
	f.Amount = 300 + int(g.r.ExpFloat64()*3500)
	if g.r.Float64() < 0.02 {
		f.Amount = 50001 + g.r.Intn(70000) // fails the ordinary amount check
	}
	if g.r.Float64() < 0.25 { // the shop is paid directly
		f.PayoutAccountID, f.PayoutType = vendorAccount(shop), "vendor_direct_settlement"
	}
	switch x := g.r.Float64(); {
	case x < 0.65:
		f.DeviceID, f.DevicePublic = p.device, false
	case x < 0.77:
		f.DeviceID, f.DevicePublic = fmt.Sprintf("DEV-KIOSK-%02d", g.r.Intn(20)), true
	}
	switch x := g.r.Float64(); {
	case x < 0.55:
		f.IPAddress = p.homeIP
	case x < 0.80:
		f.IPAddress = fmt.Sprintf("198.51.100.%d", 10+g.r.Intn(40)) // mobile-carrier NAT
	case x < 0.85:
		f.IPAddress = fmt.Sprintf("10.%d.0.1", 1+g.r.Intn(30)) // company network
	}
	if g.r.Float64() < 0.10 {
		f.ContactType, f.ContactValue = "", ""
	}
	if g.r.Float64() < 0.05 {
		f.AddressID = ""
	}
	g.policy(&f, incident, 330)
	if x := g.r.Float64(); x < 0.02 {
		f.PolicyStatus = "lapsed"
	} else if x < 0.04 {
		f.PolicyStatus = "cancelled"
	}
	return &draft{f: f, ring: -1}
}

func (g *gen) baseFNOL(p person, filed, incident time.Time, shop string) ingest.FNOL {
	f := ingest.FNOL{
		ClaimantID: p.id, ClaimantName: p.name, VIN: p.vin,
		IncidentDate: incident.Format("2006-01-02"), FilingDate: filed.Format("2006-01-02"),
		IncidentLocation: cities[g.r.Intn(len(cities))], LossType: lossTypes[g.r.Intn(len(lossTypes))],
		PayoutAccountID: p.account, PayoutType: "claimant", RepairShopID: shop,
		ContactType: p.contactType, ContactValue: p.phone, AddressID: p.address,
	}
	if g.r.Float64() < 0.3 {
		f.TowProviderID = fmt.Sprintf("TOW-%03d", g.r.Intn(40))
	}
	return f
}

// policy gives the claim a one-year policy that started up to maxAgeDays before the incident.
func (g *gen) policy(f *ingest.FNOL, incident time.Time, maxAgeDays int) {
	g.policies++
	start := incident.Add(-time.Duration(g.r.Intn(maxAgeDays+1)) * day)
	f.PolicyID = fmt.Sprintf("POL-%07d", g.policies)
	f.PolicyStart, f.PolicyEnd, f.PolicyStatus = start.Format("2006-01-02"), start.Add(365*day).Format("2006-01-02"), "active"
}

func (g *gen) shop() string {
	x := g.r.Float64()
	k := sort.SearchFloat64s(g.shopCDF, x)
	if k >= len(g.shopCDF) {
		k = len(g.shopCDF) - 1
	}
	return fmt.Sprintf("SHOP-%04d", k)
}

func vendorAccount(shop string) string { return "PAY-V-" + shop[len("SHOP-"):] }

// ring builds one planted ring of the given shape and number of claims.
func (g *gen) ring(index int, shape string, size int) ([]*draft, []string) {
	window := 20 + g.r.Intn(56)
	var end time.Time
	if index == 0 {
		end = g.cfg.End // the showcase claim is the newest claim in the book
	} else {
		span := int(g.cfg.End.Sub(g.cfg.Start)/day) - 100 - window
		end = g.cfg.Start.Add(time.Duration(100+window+g.r.Intn(span)) * day)
	}
	ringShop := fmt.Sprintf("SHOP-%04d", 20+g.r.Intn(40)) // a mid-sized shop most members use
	ringIP := fmt.Sprintf("2001:db8:ffff:%x::1", index)

	// Synthetic identities: mostly one claim each, some two (chain: exactly one each).
	var people []person
	var owner []int // claim -> person index
	for len(owner) < size {
		h := index*1000 + len(people) // ring addresses/IPs use their own id ranges
		p := g.newPerson(fmt.Sprintf("ADDR-R%06d", h), fmt.Sprintf("2001:db8:fffe:%x::1", h))
		people = append(people, p)
		owner = append(owner, len(people)-1)
		if shape != "chain" && len(owner) < size && g.r.Float64() < 0.2 {
			owner = append(owner, len(people)-1)
		}
	}
	claims := make([]*draft, size)
	for i := 0; i < size; i++ {
		var filed time.Time
		if index == 0 {
			// Chain order is time order; the last link is filed within days of the one before.
			filed = end.Add(-time.Duration((size-1-i)*window/size) * day)
			if i == size-2 {
				filed = end.Add(-3 * day)
			}
		} else {
			filed = end.Add(-time.Duration(g.r.Intn(window+1)) * day)
		}
		incident := filed.Add(-time.Duration(g.r.Intn(5)) * day)
		// Most ring members use one mid-sized shop and some file from one network (weak,
		// realistic signals). The showcase ring does not: its story is that only the strong
		// identifiers, followed beyond three transitions, reveal it.
		shop := g.shop()
		if index > 0 && g.r.Float64() < 0.75 {
			shop = ringShop
		}
		p := people[owner[i]]
		f := g.baseFNOL(p, filed, incident, shop)
		f.Amount = 4000 + g.r.Intn(9000)
		f.DeviceID, f.DevicePublic = p.device, false
		f.IPAddress = p.homeIP
		if index > 0 && g.r.Float64() < 0.4 {
			f.IPAddress = ringIP
		}
		g.policy(&f, incident, 45) // recently started policies
		claims[i] = &draft{f: f, ring: index}
	}

	share := func(field string, members []int, value string) {
		for _, m := range members {
			f := &claims[m].f
			switch field {
			case "payout":
				f.PayoutAccountID, f.PayoutType = value, "claimant"
			case "device":
				f.DeviceID, f.DevicePublic = value, false
			}
		}
	}
	newAccount := func() string { g.accounts++; return fmt.Sprintf("ACCT-%07d", g.accounts) }
	newDevice := func() string { g.devices++; return fmt.Sprintf("DEV-%07d", g.devices) }
	all := seq(size)

	switch shape {
	case "mule":
		// Two mule accounts (even/odd claims), bridged by one device used by a claim of each.
		mules := []string{newAccount(), newAccount()}
		for _, m := range all {
			share("payout", []int{m}, mules[m%2])
		}
		share("device", []int{0, 1}, newDevice())
	case "device_farm":
		farm := []string{newDevice(), newDevice(), newDevice()}
		for _, m := range all {
			share("device", []int{m}, farm[m%3])
		}
		for m := 0; m+1 < size; m += 3 { // phones and addresses tie the farm devices together
			sharePerson(people, owner, claims, m, m+1, "contact")
		}
		for m := 1; m+1 < size; m += 3 {
			sharePerson(people, owner, claims, m, m+1, "address")
		}
	case "mixed":
		mule := newAccount()
		half := all[:size/2+1]
		share("payout", half, mule)
		share("device", all[size/2:], newDevice())
		sharePerson(people, owner, claims, 0, size-1, "contact")
	case "chain":
		// Link i joins claims i and i+1 through a different identifier type each time, and
		// the last link (to the demo claim) is a shared claimant payout account.
		types := []string{"payout", "device", "contact", "address"}
		off := (4 - (size-2)%4) % 4
		for i := 0; i+1 < size; i++ {
			switch t := types[(i+off)%4]; t {
			case "payout":
				share("payout", []int{i, i + 1}, newAccount())
			case "device":
				share("device", []int{i, i + 1}, newDevice())
			default:
				sharePerson(people, owner, claims, i, i+1, t)
			}
		}
	}
	var ids []string
	for _, p := range people {
		ids = append(ids, p.id)
	}
	return claims, ids
}

// sharePerson makes claim b's claimant use claim a's phone/e-mail or address.
func sharePerson(people []person, owner []int, claims []*draft, a, b int, what string) {
	pa, pb := &people[owner[a]], &people[owner[b]]
	if pa.id == pb.id {
		return
	}
	switch what {
	case "contact":
		pb.contactType, pb.phone = pa.contactType, pa.phone
	case "address":
		pb.address = pa.address
	}
	for i := range claims {
		if owner[i] == owner[b] {
			claims[i].f.ContactType, claims[i].f.ContactValue = pb.contactType, pb.phone
			claims[i].f.AddressID = pb.address
		}
	}
}

func stats(ds *Dataset, ringClaims, nShops int) Stats {
	s := Stats{Claims: len(ds.FNOLs), RingClaims: ringClaims, RepairShops: nShops, PriorCases: len(ds.Cases)}
	s.LegitClaims = s.Claims - ringClaims
	claimants, addresses, shops := map[string]bool{}, map[string]bool{}, map[string]int{}
	for _, f := range ds.FNOLs {
		claimants[f.ClaimantID] = true
		if f.AddressID != "" {
			addresses[f.AddressID] = true
		}
		shops[f.RepairShopID]++
		if f.PayoutType == "vendor_direct_settlement" {
			s.VendorPaidClaims++
		}
		if f.DevicePublic {
			s.KioskClaims++
		}
		if len(f.IPAddress) > 11 && f.IPAddress[:11] == "198.51.100." {
			s.CarrierIPClaims++
		}
	}
	s.Claimants, s.Households = len(claimants), len(addresses)
	for k, v := range shops {
		if v > s.BusiestShopClaims || (v == s.BusiestShopClaims && k < s.BusiestShop) {
			s.BusiestShop, s.BusiestShopClaims = k, v
		}
	}
	return s
}

// ---- small helpers ----

func pick(r *rand.Rand, weights []float64) int {
	x := r.Float64()
	for i, w := range weights {
		if x < w {
			return i
		}
		x -= w
	}
	return len(weights) - 1
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func tsAfter(date string, days int) string {
	t, _ := time.Parse("2006-01-02", date)
	return t.Add(time.Duration(days)*day + 10*time.Hour).UTC().Format(time.RFC3339)
}

func maxInt(a, b int) int { return int(math.Max(float64(a), float64(b))) }
