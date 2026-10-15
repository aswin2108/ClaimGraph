import type { FNOL } from "./types";

// Synthetic FNOL scenarios for the intake panel. They reference identifiers that already
// exist in the seeded graph (PAY-01, DEV-01, SHOP-01, ...) so the connections are discovered
// by real graph queries, not hardcoded into the UI.

export interface Preset {
  key: string;
  title: string;
  blurb: string;
  build: (claimId: string) => FNOL;
}

const base = (claimId: string): FNOL => ({
  claim_id: claimId,
  claimant_id: `DRV-${claimId}`,
  claimant_name: `Synthetic Driver ${claimId.slice(-4)}`,
  policy_id: `POL-${claimId}`,
  policy_start: "2026-01-01",
  policy_end: "2026-12-31",
  policy_status: "active",
  vin: `1HGSYN${claimId.replace(/\D/g, "").padStart(11, "0").slice(-11)}`,
  incident_date: "2026-09-08",
  filing_date: "2026-09-09",
  incident_location: "Synthetic City",
  loss_type: "collision",
  amount: 4100,
  payout_account_id: `PAY-${claimId}`,
  payout_type: "claimant",
  repair_shop_id: `SHOP-${claimId}`,
});

export const PRESETS: Preset[] = [
  {
    key: "clean",
    title: "Looks clean on its own",
    blurb: "Passes every ordinary policy check. Reuses a payout account and device already seen on other claims.",
    build: (id) => ({ ...base(id), payout_account_id: "PAY-01", device_id: "DEV-01", repair_shop_id: "SHOP-01" }),
  },
  {
    key: "shop",
    title: "Busy repair shop only",
    blurb: "A control case: the only overlap is a popular repair shop.",
    build: (id) => ({ ...base(id), repair_shop_id: "SHOP-01", filing_date: "2026-03-02", incident_date: "2026-03-01" }),
  },
  {
    key: "vendor",
    title: "Vendor settlement account",
    blurb: "Paid directly to a repair vendor's account, which must not count as a shared claimant payout.",
    build: (id) => ({ ...base(id), payout_account_id: "PAY-VENDOR", payout_type: "vendor_direct_settlement", repair_shop_id: "SHOP-01" }),
  },
  {
    key: "isolated",
    title: "Isolated claim",
    blurb: "Shares nothing with any other claim. Expect no connections and no invented evidence.",
    build: (id) => base(id),
  },
];

export const newClaimId = () => `CLM-${String(Date.now()).slice(-6)}`;
