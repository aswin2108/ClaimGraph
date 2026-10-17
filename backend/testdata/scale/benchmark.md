# ClaimGraph scale benchmark

Graph `claimgraph_scale` as seeded (measured on a copy of its pristine snapshot, so claims submitted while testing the app are not included): 20000 claims, 15670 claimants, 122455 nodes / 175527 relationships, 12 planted rings. Bulk load 8.5 s. Busiest repair shop SHOP-0000 serves 1494 claims. Synthetic data; results describe this book only.

## Latency (ms, wall clock from Go, FalkorDB in local Docker)

| sample | operation | n | p50 | p95 | max | errors |
|---|---|---|---|---|---|---|
| planted-ring claims | Analyze (traversal + score + prior cases) | 156 | 210 | 321 | 659 | 0 |
| planted-ring claims | Counterfactual (exclude repair_shop) | 156 | 427 | 587 | 888 | 0 |
| planted-ring claims | FindRelatedClaims hops=1 | 156 | 30 | 39 | 92 | 0 |
| planted-ring claims | FindRelatedClaims hops=2 | 156 | 66 | 83 | 132 | 0 |
| planted-ring claims | FindRelatedClaims hops=3 | 156 | 187 | 299 | 673 | 0 |
| planted-ring claims | GetClaimRing (cached WCC/MSF + live facts) | 156 | 8 | 11 | 38 | 0 |
| busiest-shop claims | Analyze (traversal + score + prior cases) | 50 | 161 | 278 | 430 | 0 |
| busiest-shop claims | Counterfactual (exclude repair_shop) | 50 | 310 | 497 | 661 | 0 |
| busiest-shop claims | FindRelatedClaims hops=1 | 50 | 30 | 36 | 87 | 0 |
| busiest-shop claims | FindRelatedClaims hops=2 | 50 | 60 | 80 | 98 | 0 |
| busiest-shop claims | FindRelatedClaims hops=3 | 50 | 142 | 251 | 424 | 0 |
| busiest-shop claims | GetClaimRing (cached WCC/MSF + live facts) | 50 | 7 | 10 | 11 | 0 |
| random claims | Analyze (traversal + score + prior cases) | 200 | 151 | 306 | 1102 | 0 |
| random claims | Counterfactual (exclude repair_shop) | 200 | 304 | 548 | 1049 | 0 |
| random claims | FindRelatedClaims hops=1 | 200 | 29 | 37 | 94 | 0 |
| random claims | FindRelatedClaims hops=2 | 200 | 61 | 88 | 217 | 0 |
| random claims | FindRelatedClaims hops=3 | 200 | 135 | 298 | 1244 | 0 |
| random claims | GetClaimRing (cached WCC/MSF + live facts) | 200 | 7 | 10 | 45 | 0 |
| book-wide | GetStrongestLink (algo.SPpaths) across a ring | 12 | 6 | 9 | 9 | 0 |
| book-wide | ListRings (algo.WCC over the whole book) | 5 | 429 | 435 | 516 | 0 |
| book-wide | GetClaimRing first call (cold: algo.WCC + algo.MSF over the whole book) | 1 | 1061 | 1061 | 1061 | 0 |

Results cut by a limit at hops=3 (flagged `truncated`): planted-ring claims 0/156; busiest-shop claims 0/50; random claims 0/200; 

Claims showing only 5 example paths for some multi-hop connection (presentation cap, no evidence lost): planted-ring claims 71/156; busiest-shop claims 7/50; random claims 8/200; 

Claims that use shared infrastructure (a shop, carrier IP, kiosk or vendor account used by more than 20 claims; counted once, not as connections): planted-ring claims 154/156; busiest-shop claims 50/50; random claims 200/200; 

## Ring recovery (algo.WCC over strong evidence)

| ring | shape | planted claims | WCC claims | exact | confirmed case | rank in ring list |
|---|---|---|---|---|---|---|
| 0 | chain | 10 | 10 | true | true | 7 |
| 1 | mule | 14 | 14 | true | true | 3 |
| 2 | device_farm | 6 | 6 | true | false | 12 |
| 3 | mixed | 11 | 11 | true | true | 6 |
| 4 | chain | 15 | 15 | true | false | 8 |
| 5 | mule | 12 | 12 | true | true | 5 |
| 6 | device_farm | 13 | 13 | true | false | 11 |
| 7 | mixed | 15 | 15 | true | true | 1 |
| 8 | chain | 14 | 14 | true | false | 9 |
| 9 | mule | 16 | 16 | true | true | 2 |
| 10 | device_farm | 16 | 16 | true | false | 10 |
| 11 | mixed | 14 | 14 | true | true | 4 |

Exact recovery: 12/12 planted rings. Book-wide WCC found 2998 components with >= 3 claims (households and repeat claimants form legitimate ones). Planted rings in the top 12 of the ring list: 12; in the top 50: 12.

Top of the ring list:

| rank | ring | size | span days | confirmed | planted? |
|---|---|---|---|---|---|
| 1 | RING-CLM-118007 | 15 | 37 | 1 | true |
| 2 | RING-CLM-106090 | 16 | 40 | 1 | true |
| 3 | RING-CLM-114553 | 14 | 18 | 1 | true |
| 4 | RING-CLM-107009 | 14 | 33 | 1 | true |
| 5 | RING-CLM-109598 | 12 | 30 | 1 | true |
| 6 | RING-CLM-116472 | 11 | 39 | 1 | true |
| 7 | RING-CLM-119155 | 10 | 29 | 1 | true |
| 8 | RING-CLM-104062 | 15 | 35 | 0 | true |
| 9 | RING-CLM-110475 | 14 | 67 | 0 | true |
| 10 | RING-CLM-117107 | 16 | 55 | 0 | true |
| 11 | RING-CLM-115319 | 13 | 29 | 0 | true |
| 12 | RING-CLM-106093 | 6 | 19 | 0 | true |
| 13 | RING-CLM-100918 | 10 | 660 | 0 | false |
| 14 | RING-CLM-112681 | 4 | 66 | 0 | false |
| 15 | RING-CLM-116381 | 4 | 82 | 0 | false |

## Investigation-priority bands (deterministic score, hops=3)

| sample | Low | Review | High Priority |
|---|---|---|---|
| planted-ring claims | 0 | 0 | 156 |
| busiest-shop claims | 31 | 18 | 1 |
| random claims | 145 | 51 | 4 |

The score uses direct (1-transition) evidence only; ring membership beyond that is shown by the ring tools and never changes the score.

## What drives the score (share of claims where each factor fired)

| factor | points | planted-ring claims | busiest-shop claims | random claims |
|---|---|---|---|---|
| shared_claimant_payout | 25 | 53% | 24% | 18% |
| shared_non_public_device | 20 | 50% | 0% | 0% |
| shared_contact | 12 | 28% | 6% | 6% |
| shared_address | 10 | 28% | 68% | 71% |
| shared_repair_shop | 8 | 100% | 100% | 100% |
| shared_public_ip | 3 | 38% | 62% | 52% |
| connected_claims_breadth | 15 | 100% | 16% | 9% |
| temporal_proximity | 10 | 97% | 6% | 3% |
| claim_burst | 10 | 96% | 0% | 0% |
| new_policy_cluster | 10 | 100% | 0% | 0% |
| confirmed_fraud_history | 15 | 48% | 0% | 0% |
| multi_signal_reinforcement | 10 | 52% | 0% | 0% |
| household-explained (bonus withheld) | - | 1% | 68% | 70% |
| average score | - | 92 | 26 | 23 |
