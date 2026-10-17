# Build log

A dated record of how ClaimGraph was built during Graph Hacks (Oct 15–18, 2026; times in IST). Hacking opened Oct 15, 12:01 AM; work on this repository started that day at about 12:30 PM.

The idea is mine. I work in insurance, and finding fraud hidden among ordinary-looking claims had been on my mind for a while. I developed it into this design with AI help and studied past FalkorDB hackathon winners to shape it. Most code was written with Claude Code from my instructions. This log records the decisions I made, the problems I found by using the app, and how each result was checked.

## Oct 15: foundation

| Time | What happened |
|---|---|
| 12:30 | Reviewed the plan (Idea.txt) and the project rules file. **Decision:** personal auto insurance only. I proposed the data points: claim, claimant/driver, vehicle, policy, repair shop, tow provider, payout destination, contact/device. |
| 12:48 | Asked for a tiny Go program to prove the FalkorDB connection works before anything else, then a small synthetic graph queried end to end. Rule set: I commit everything myself. |
| 13:08–14:30 | FNOL validation and idempotent ingestion; the first investigation query (claims connected through shared entities, with the actual paths); the deterministic scoring engine; the disposition recorder; the counterfactual. For the last two I first checked that they were in the plan. |
| 14:34 | Asked for a review of all code so far and a test for every feature. |
| 15:00–15:13 | HTTP API; the bounded agent with its tool allowlist. I asked whether the agent needs memory and how it restarts after a failure. **Result:** the agent is stateless; its only memory is investigation outcomes stored in the graph. |
| 15:53 | **Decision:** use Gemini Flash as the model; key kept in a git-ignored file. |
| 16:40 | Asked for a review of every model call ("I don't want huge bills"). **Result:** per-run limits on turns, requests, tokens and time, plus per-hour and per-day budgets, with tests that count real requests. |
| 18:02–18:12 | The 30-scenario evaluation: T01–T20, then T21–T30, with fixed expected scores. |
| 18:19 | Frontend. I asked how to see the hops being performed. **Result:** a step-by-step traversal replay driven by the real hop data. |
| 19:03 | Asked for a frontend review and tests for every button that fires a request. |
| 19:18 | **Bug I found:** opening the UI gave 502 errors (the API wasn't started). Later the agent button failed with "API is not running" while the graph was loading fine. **Cause:** agent runs over 30 s were cut off by the server's write timeout. Fixed, with a regression test that runs a real HTTP server. |
| 19:45 | Docker Compose: clean start, persistence, test profile. |

Commits that night (Oct 16, 00:02–01:26): graph algorithms (WCC, MSF, SPpaths) and the MCP server; scoring changes for shared resources; LLM check and the ring alert on intake.

## Oct 16: product, scoring and evidence

| Time | What happened |
|---|---|
| 12:00 | Resumed against the Track 01 plan. Ring panel, ring list in the queue, recommended verifications, timing line. |
| 13:01 | **Decision:** I asked whether the demo should submit a claim live and watch it get flagged, instead of an autopilot button. I dropped autopilot and asked for a Reset button so every visitor can start fresh. |
| 13:10 | Asked whether the "ordinary checks passed" message was real or just text. **Result:** real policy checks in Go. **Decision:** the 20,000-claim book is the default graph. |
| 13:40 | Showcase claim preset, so anyone can repeat my demo. |
| 14:09 | **Testing by hand:** the queue card was too small and placed below the intake card; only 100 claims were visible; asked for the hop logic to be explained in the UI; asked for a growing traversal animation. All fixed (queue on top, paging and search, hop hint, grow animation). |
| 14:30 | **Bug I found:** after selecting a node, the highlighted edges stayed visible during a replay. Fixed. |
| 14:45 | **Question that found a scoring flaw:** for CLM-119984 the 3-hop traversal showed a dense graph, but the ring showed 3 nodes. **Cause:** context-only identifiers (vendor accounts, public devices, private IPs) were connecting claims. |
| 15:01 | **Rule I set:** investigate any scoring fix before implementing it, and the planted rings must stay flagged. Also asked to show the earlier decision's notes and date; checked whether a confirmed case really adds +15 to later linked claims; found the 20k claims were missing from the queue. |
| 15:40 | **Decision:** approved fixes A (context-only identifiers never connect claims) and C (categories from every direct link). Asked for a study of false High scores in households. Renamed "Not Substantiated" to "No Fraud Found". |
| 16:37 | **Decision:** implement the household rule only after the full study. **Result** on 1,157 sampled claims: ring claims 156/156 still High, random claims marked High 11.9% → 2.8%, no score went up. |
| 17:04 | **Bugs I found:** the "?" hint was cut off by the graph card. A claim that failed its policy checks showed "Low priority" with no explanation. **Result:** a separate "Coverage check failed" note that never changes the investigation score. |
| 17:30–18:52 | **Decision:** benchmark against PostgreSQL, full version. **Result:** 24,264 identical answers from both engines. FalkorDB wins whole-book ring detection (about 40×) and any-depth weighted chains (about 28×); tuned PostgreSQL wins short lookups. |
| 19:20 | **Decision:** report where the graph excels first, then PostgreSQL's wins, stated plainly. |
| 19:37 | Re-ran the older 20k benchmark on a clean copy of the book after the scoring changes. |
| 20:06 | README, with screenshots and a GIF captured from the running app. |
| 21:23 | Checked the project against the official rules and fixed the track name. |
| 21:37 | Wrote this log and the README section on AI use. |
| 22:30 | **Question I raised:** Track 01 lists "tool selection through MCP", but our in-app agent called its tools directly; only outside clients used MCP. **Decision:** the app's own agent now discovers its tools over MCP (`tools/list`) and calls them over MCP (`tools/call`), with the same guardrails underneath. Verified with tests and one live Gemini run (8 tools discovered, 8 calls over MCP). |
| 23:45 | **Decision (my design):** the investigator reads the agent's report and closes the investigation. Approve (no fraud found; the claim goes on to normal claims handling) or Reject (fraud confirmed), each with a required reason, or Need more information to keep it open. The three buttons appear only under a report, replacing the separate decision tile. Closing seals the exact report into the graph and locks the claim (no more agent runs or decisions; the server enforces it). Verified with API tests, 65 UI tests and 83 browser checks. |

## Oct 17

| Time | What happened |
|---|---|
| 00:40 | README review from a judge's point of view (my "I" voice, known limitations), refreshed screenshots, clean-clone test from GitHub (`docker compose up --build` from nothing: 78 High Priority, ring of 11, close and reset all work). |
| 01:50 | **Decision:** add "Show Cypher": the Evidence panel and Ring view show the exact queries sent to FalkorDB with their bound parameters. Tests prove every parameter is logged, no claim id is ever pasted into query text, and the model never receives Cypher. **Decision:** MIT license. |

## How results are checked

Every number in the README can be re-run from this repository:
- `go test ./...`, including the 30-scenario evaluation (`internal/eval`) with exact expected scores and bands;
- parity tests that compare the production traversal with an independent reference engine on every fixture and on random graphs;
- `npm test` (Vitest) and `npm run test:e2e` (real browser, real API, real FalkorDB);
- `cmd/bench` (the 20k-claim book) and `cmd/benchrel` (FalkorDB vs PostgreSQL).
