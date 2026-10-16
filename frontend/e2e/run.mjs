// Real-browser end-to-end test: Chrome -> Vite dev server -> Go API -> FalkorDB.
//
//   npm run test:e2e
//
// Safety: uses a scratch graph ("claimgraph_e2e", deleted afterwards), a separate API port, and starts the API
// with AGENT_ENABLED=false, so nothing here can call the LLM or touch your real data.
// Needs: Go, Docker FalkorDB on localhost:6379, and Chrome (set CHROME_PATH if it is not in the default place).
import { spawn, spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import puppeteer from "puppeteer-core";

const here = dirname(fileURLToPath(import.meta.url));
const frontend = resolve(here, "..");
const backend = resolve(frontend, "..", "backend");
const API = "http://127.0.0.1:8099";
const WEB = "http://127.0.0.1:5199";
const GRAPH = "claimgraph_e2e";
const CHROME =
  process.env.CHROME_PATH ??
  ["C:/Program Files/Google/Chrome/Application/chrome.exe", "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe", "/usr/bin/google-chrome", "/usr/bin/chromium"].find(existsSync);

const procs = [];
const results = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function check(name, ok, detail = "") {
  results.push({ name, ok });
  console.log(`${ok ? "  PASS" : "  FAIL"}  ${name}${ok || !detail ? "" : `\n        ${detail}`}`);
}
const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);

async function waitUp(url, what, ms = 60000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {}
    await sleep(300);
  }
  throw new Error(`${what} did not start`);
}

const api = async (method, path, body) => {
  const r = await fetch(API + path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  const j = await r.json();
  if (!r.ok) throw new Error(`${method} ${path} -> ${r.status} ${JSON.stringify(j)}`);
  return j;
};

function fnol(id, filed, mods = {}) {
  return {
    claim_id: id, claimant_id: `DRV-${id}`, claimant_name: `Driver ${id}`, policy_id: `POL-${id}`,
    policy_start: "2026-01-01", policy_end: "2026-12-31", policy_status: "active",
    vin: `1HGSYN${id.replace(/\D/g, "").padStart(11, "0")}`, incident_date: filed, filing_date: filed, amount: 4000,
    payout_account_id: `PAY-${id}`, payout_type: "claimant", repair_shop_id: `SHOP-${id}`, ...mods,
  };
}

const falkorContainer = () =>
  spawnSync("docker", ["ps", "-q", "--filter", "ancestor=falkordb/falkordb"]).stdout.toString().trim().split("\n")[0];

function cleanup() {
  for (const p of procs) {
    try {
      if (process.platform === "win32") spawnSync("taskkill", ["/PID", String(p.pid), "/T", "/F"]);
      else p.kill("SIGKILL");
    } catch {}
  }
  // drop the scratch graph and its demo snapshot
  for (const g of [GRAPH, `${GRAPH}__pristine`]) spawnSync("docker", ["exec", falkorContainer(), "redis-cli", "GRAPH.DELETE", g]);
}

async function main() {
  if (!CHROME) throw new Error("Chrome not found; set CHROME_PATH");

  // --- start the stack -------------------------------------------------------------------------
  const exe = join(tmpdir(), process.platform === "win32" ? "claimgraph-e2e-api.exe" : "claimgraph-e2e-api");
  const build = spawnSync("go", ["build", "-o", exe, "./cmd/api"], { cwd: backend, encoding: "utf8" });
  if (build.status !== 0) throw new Error("go build failed: " + build.stderr);
  procs.push(spawn(exe, [], { cwd: backend, stdio: "ignore", env: { ...process.env, API_ADDR: "127.0.0.1:8099", GRAPH_NAME: GRAPH, AGENT_ENABLED: "false", CORS_ORIGIN: WEB } }));
  await waitUp(API + "/healthz", "API");
  procs.push(spawn(process.execPath, [join(frontend, "node_modules/vite/bin/vite.js"), "--host", "127.0.0.1"], { cwd: frontend, stdio: "ignore", env: { ...process.env, PORT: "5199", API_URL: API } }));
  await waitUp(WEB, "Vite");

  // --- seed a small ring through the public FNOL endpoint (this also exercises ingestion) -------------
  const ring = { payout_account_id: "PAY-01", device_id: "DEV-01", repair_shop_id: "SHOP-01" };
  await api("POST", "/api/fnol", fnol("CLM-E01", "2026-06-01", ring));
  await api("POST", "/api/fnol", fnol("CLM-E02", "2026-09-05", ring));
  await api("POST", "/api/fnol", fnol("CLM-E03", "2026-09-07", ring));
  await api("POST", "/api/fnol", fnol("CLM-E04", "2026-03-01", { repair_shop_id: "SHOP-01" }));
  await api("POST", "/api/fnol", fnol("CLM-E05", "2026-02-01"));
  const T = "CLM-E03"; // newest filing date, so first in the queue
  // The demo snapshot that "Reset demo" restores (cmd/seedscale saves one for the real demo graph).
  const snap = spawnSync("docker", ["exec", falkorContainer(), "redis-cli", "GRAPH.COPY", GRAPH, `${GRAPH}__pristine`], { encoding: "utf8" });
  if (!snap.stdout.includes("OK")) throw new Error("could not save the demo snapshot: " + snap.stdout + snap.stderr);
  const baseline = await api("GET", `/api/claims/${T}/investigation?hops=3`);

  // --- browser --------------------------------------------------------------------------------
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1600, height: 1100 });
  const reqs = [];
  const problems = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/")) reqs.push({ method: r.method(), path: u.pathname, query: u.search, body: r.postData() });
  });
  page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
  page.on("console", (m) => {
    if (m.type() === "error") problems.push("console: " + m.text());
  });
  const mark = () => reqs.length;
  const since = (i) => reqs.slice(i);
  const posts = (rs, part) => rs.filter((r) => r.method === "POST" && r.path.includes(part));

  const text = (sel) => page.$eval(sel, (e) => e.textContent ?? "");
  const waitFor = async (fn, what, ms = 10000, ...args) => {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      try {
        if (await page.evaluate(fn, ...args)) return true;
      } catch {}
      await sleep(100);
    }
    return false;
  };
  const btn = (label) =>
    page.evaluateHandle((l) => [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === l && !b.disabled) ?? null, label);
  async function click(label) {
    const h = await btn(label);
    const el = h.asElement();
    if (!el) throw new Error(`no enabled button "${label}"`);
    await el.click();
  }
  const score = () => page.$eval(".g-score", (e) => Number(e.textContent));
  const cyInfo = () =>
    page.evaluate(() => {
      const cy = window.__cy;
      return { nodes: cy.nodes().length, edges: cy.edges().length, hidden: cy.elements(".hidden").length, ids: cy.nodes().map((n) => n.id()).sort() };
    });

  await page.goto(WEB, { waitUntil: "networkidle0" });
  await waitFor(() => document.querySelector(".g-score") && window.__cy, "first investigation");

  console.log("\nInitial load");
  check("no agent (LLM) request on page load", posts(reqs, "/agent").length === 0);
  check("queue lists all 5 seeded claims", (await page.$$eval(".q-item", (e) => e.length)) === 5);
  check("first claim is selected and its score equals the API's", (await score()) === baseline.score, `ui=${await score()} api=${baseline.score}`);
  check("band badge matches the API", (await text(".band")) === baseline.band);
  const graphApi = await api("GET", `/api/claims/${T}/graph?hops=3`);
  check("every node in the API graph is drawn by Cytoscape", eq((await cyInfo()).ids, graphApi.nodes.map((n) => n.id).sort()));
  check("evidence panel lists every connected claim", (await page.$$eval(".ev-item", (e) => e.length)) === baseline.connected_claims);
  check("the API ran the traversal inside FalkorDB", baseline.investigation.traversal_stats?.engine === "falkordb_in_database");
  check("the UI shows the FalkorDB timing line", (await text(".stats-line")).includes("Traversal ran inside FalkorDB") && /\d+ ms/.test(await text(".stats-line")));
  const planApi = baseline.recommended_actions?.actions ?? [];
  check("recommended verifications are shown without the agent, one per API action",
    planApi.length > 0 && (await page.$$eval('[aria-label="Recommended verifications"] > li', (e) => e.length)) === planApi.length && posts(reqs, "/agent").length === 0,
    `api=${planApi.length}`);
  check("every recommended action needs human approval", planApi.every((a) => a.requires_human_approval));

  console.log("\nReplay traversal (client side only)");
  let m = mark();
  await click("▶ Replay traversal");
  await sleep(300);
  const during = await cyInfo();
  check("replay starts by hiding later hops", during.hidden > 0, JSON.stringify(during));
  await waitFor(() => window.__cy.elements(".hidden").length === 0, "replay done", 8000);
  check("replay ends with the whole graph visible", (await cyInfo()).hidden === 0);
  check("replay sends no network requests", since(m).length === 0);
  // A highlighted evidence path must not survive a replay (it used to stay drawn and grow over itself).
  await page.evaluate(() => document.querySelector(".ev-head").click());
  await sleep(300);
  check("clicking a connected claim highlights its path", (await page.evaluate(() => window.__cy.elements(".onpath").length)) > 0);
  await click("▶ Replay traversal");
  await sleep(300);
  const replayed = await page.evaluate(() => ({ onpath: window.__cy.elements(".onpath").length, hidden: window.__cy.elements(".hidden").length }));
  check("replay clears the highlighted path and starts from the target", replayed.onpath === 0 && replayed.hidden > 0, JSON.stringify(replayed));
  await waitFor(() => window.__cy.elements(".hidden").length === 0, "replay done", 8000);

  console.log("\nHops selector");
  m = mark();
  await page.select(".select select", "1");
  await waitFor(() => document.querySelector(".select select").value === "1" && window.__cy, "hops");
  await sleep(800);
  const r1 = since(m);
  check("changing hops refetches investigation and graph with hops=1", r1.some((r) => r.path.endsWith("/investigation") && r.query.includes("hops=1")) && r1.some((r) => r.path.endsWith("/graph") && r.query.includes("hops=1")));
  check("changing hops never calls the agent", posts(r1, "/agent").length === 0);
  await page.select(".select select", "3");
  await sleep(800);

  console.log("\nCounterfactual toggle");
  const cfApi = await api("POST", `/api/claims/${T}/counterfactual`, { exclude: "repair_shop", hops: 3 });
  m = mark();
  await click("Repair shop");
  await waitFor(() => document.querySelector(".cf-result"), "cf result");
  await sleep(500);
  const r2 = since(m);
  check("exactly one counterfactual POST with the right body", posts(r2, "/counterfactual").length === 1 && eq(JSON.parse(posts(r2, "/counterfactual")[0].body), { exclude: "repair_shop", hops: 3 }));
  check("score shown equals the API's counterfactual score", (await score()) === cfApi.counterfactual.score, `ui=${await score()} api=${cfApi.counterfactual.score}`);
  check("score really changed", cfApi.counterfactual.score !== baseline.score);
  const withoutShop = await cyInfo();
  check("the repair-shop node is removed from the drawn graph", !withoutShop.ids.some((i) => i.startsWith("entity:RepairShop:")) && withoutShop.nodes < graphApi.nodes.length);
  check("the explanation is shown", (await text(".cf-result")).includes(cfApi.explanation.slice(0, 20)));
  m = mark();
  await click("Reset");
  await sleep(900);
  check("reset restores the original score without a new counterfactual POST", (await score()) === baseline.score && posts(since(m), "/counterfactual").length === 0);
  check("reset restores the full graph", eq((await cyInfo()).ids, graphApi.nodes.map((n) => n.id).sort()));
  check("disabled signal chips do nothing", (await btn("Phone / email")).asElement() === null);

  console.log("\nClaim ring (algo.WCC + algo.MSF in FalkorDB; strongest link via algo.SPpaths)");
  const ringApi = await api("GET", `/api/claims/${T}/ring`);
  check("the API finds the seeded ring", ringApi.size === 3 && ringApi.backbone.length > 0, `size=${ringApi.size}`);
  m = mark();
  await click(`Ring · ${ringApi.size} claims`);
  await waitFor(() => document.querySelector(".ring-stats") && window.__ringCy, "ring view");
  await sleep(300);
  check("opening the ring view sends no request (fetched once with the claim)", since(m).length === 0, JSON.stringify(since(m)));
  const ringIds = await page.evaluate(() => window.__ringCy.nodes().map((n) => n.id()).sort());
  const backboneIds = new Set(ringApi.backbone.flatMap((l) => [`${l.from.type}:${l.from.id}`, `${l.to.type}:${l.to.id}`]));
  check("Cytoscape draws only algo.MSF backbone nodes", ringIds.length > 0 && ringIds.every((i) => backboneIds.has(i)), `ui=${ringIds}`);
  check("every ring member claim is drawn", ringApi.members.every((x) => ringIds.includes(`Claim:${x.claim_id}`)), `ui=${ringIds}`);
  check("every ring member is listed", (await page.$$eval(".member", (e) => e.length)) === ringApi.members.length);
  const other = ringApi.members.map((x) => x.claim_id).find((c) => c !== T);
  const linkApi = await api("GET", `/api/claims/${T}/link/${other}`);
  m = mark();
  await page.evaluate((o) => document.querySelector(`.member[aria-label^="${o},"]`).click(), other);
  await waitFor(() => document.querySelector(".ring-link-head"), "strongest link");
  await sleep(300);
  const lr = since(m);
  check("clicking a member sends exactly one strongest-link request", lr.length === 1 && lr[0].path === `/api/claims/${T}/link/${other}`, JSON.stringify(lr));
  check("the strongest link shown matches the API", linkApi.found && (await text(".ring-link")).includes(`evidence cost ${linkApi.total_evidence_cost}`));
  const lit = await page.evaluate(() => window.__ringCy.elements(".onpath").length);
  check("the chain is highlighted on the graph", lit === linkApi.links.length * 2 + 1, `lit=${lit} links=${linkApi.links.length}`);
  check("the ring view never calls the agent", posts(since(m), "/agent").length === 0);

  console.log("\nRing queue (book-wide algo.WCC in review order)");
  const listApi = await api("GET", "/api/rings?min_size=3&limit=20");
  m = mark();
  await click("Rings");
  await waitFor(() => document.querySelectorAll('[aria-label="Ring queue"] .q-item').length > 0, "ring list");
  const shownRings = await page.$$eval('[aria-label="Ring queue"] .q-item strong', (e) => e.map((x) => x.textContent));
  check("the ring list matches the API, in the same order", eq(shownRings, listApi.rings.map((r) => r.ring_id)), `ui=${shownRings}`);
  check("opening the tab sends exactly one /api/rings request", since(m).filter((r) => r.path === "/api/rings").length === 1);
  check("the ring of the selected claim is marked", await page.$eval('[aria-label="Ring queue"] .q-item.on strong', (e) => e.textContent) === ringApi.ring_id);
  m = mark();
  await page.evaluate(() => document.querySelector('[aria-label="Ring queue"] .q-item').click());
  const opened = listApi.rings[0].ring_id.replace(/^RING-/, "");
  await waitFor((o) => document.querySelector(".claim-strip")?.textContent.includes(o), "ring opened", 10000, opened);
  await sleep(800);
  check("clicking a ring opens its member claim", (await text(".claim-strip")).includes(opened));
  check("...in the ring view", !!(await page.$(".ring-stats")));
  check("opening a ring never calls the agent", posts(since(m), "/agent").length === 0);
  await page.evaluate(() => [...document.querySelectorAll('[role="tab"]')].find((b) => b.textContent.startsWith("Claims")).click());
  await page.evaluate((t) => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes(t)).click(), T);
  await waitFor((t) => document.querySelector(".claim-strip")?.textContent.includes(t), "reselect target", 10000, T);
  await sleep(500);
  await click("Traversal ≤ 3 hops");
  await waitFor(() => document.querySelector(".select select") && window.__cy && !window.__cy.destroyed(), "back to traversal");
  await sleep(500);

  console.log("\nAgent button (LLM disabled in this run: deterministic explanation, zero spend)");
  m = mark();
  await click("Run investigation agent");
  await waitFor(() => document.querySelector(".agent-out"), "agent result", 15000);
  const r3 = since(m);
  check("exactly one agent POST", posts(r3, "/agent").length === 1 && eq(JSON.parse(posts(r3, "/agent")[0].body), { hops: 3 }));
  check("result is labelled Deterministic with the honest reason", (await text(".card:has(.agent-out)")).includes("Deterministic") && (await text(".card:has(.agent-out)")).toLowerCase().includes("language model is turned off"));
  check("a next step is shown", (await text(".next")).length > 20);
  const explanationText = await text(".agent-out");
  const known = new Set([T, ...baseline.investigation.related_claims.map((c) => c.claim_id)]);
  const mentioned = [...new Set(explanationText.match(/CLM-[A-Z0-9]+/g) ?? [])];
  check("no claim id appears that the graph did not return", mentioned.every((c) => known.has(c)), mentioned.join(","));
  m = mark();
  await click("Run again");
  await sleep(1200);
  check("'Run again' sends exactly one more POST", posts(since(m), "/agent").length === 1);

  console.log("\nDecision under the report, closing the investigation, and the memory loop");
  const reason = 'textarea[aria-label="Decision reason"]';
  const statusOf = async (method, path, body) =>
    (await fetch(API + path, { method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined })).status;
  // 1. "Need more information" on a different claim: recorded, investigation stays open
  let q = mark();
  await page.evaluate(() => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes("CLM-E05")).click());
  await waitFor(() => document.querySelector(".claim-strip")?.textContent.includes("CLM-E05"), "select E05");
  await sleep(500);
  check("no decision buttons before a report exists", !(await btn("Need more information")).asElement() && !(await page.$(reason)));
  await click("Run investigation agent");
  await waitFor((r) => document.querySelector(r), "decision box", 15000, reason);
  await page.type(reason, "e2e note");
  await click("Need more information");
  await waitFor(() => document.querySelector(".note.ok"), "needs info ok");
  const dp = posts(since(q), "/disposition");
  check("one disposition POST with the typed notes", dp.length === 1 && eq(JSON.parse(dp[0].body), { disposition: "needs_info", notes: "e2e note" }));
  check("the queue now shows the recorded outcome", (await page.$$eval(".q-item", (e) => e.some((b) => b.textContent.includes("CLM-E05") && b.textContent.includes("Needs More Information")))));
  check("the reason was cleared after saving", (await page.$eval(reason, (e) => e.value)) === "");
  check("the investigation stays open", !!(await btn("Run again")).asElement() && !(await page.$('[aria-label="Investigation closed"]')));
  const stored = await api("GET", "/api/claims?limit=100");
  check("the API agrees it was stored", stored.claims.find((c) => c.claim_id === "CLM-E05")?.disposition === "needs_info");

  // 2. Reject and close the OLD ring claim E01: two clicks, the report is sealed, the claim is locked
  await page.evaluate(() => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes("CLM-E01")).click());
  await waitFor(() => document.querySelector(".claim-strip")?.textContent.includes("CLM-E01"), "select E01");
  await sleep(500);
  q = mark();
  await click("Run investigation agent");
  await waitFor((r) => document.querySelector(r), "decision box E01", 15000, reason);
  const runE01 = posts(since(q), "/agent").length;
  await page.type(reason, "e2e: payout holder is not the claimant");
  await click("Reject and close");
  await sleep(300);
  check("first click on Reject sends nothing", posts(since(q), "/close").length === 0 && runE01 === 1);
  await click("Confirm: reject and close");
  await waitFor(() => document.querySelector('[aria-label="Investigation closed"]'), "closed", 10000);
  const cp = posts(since(q), "/close");
  const cbody = cp.length === 1 ? JSON.parse(cp[0].body) : {};
  check("second click sends exactly one close POST with the report's run id", cp.length === 1 && cbody.outcome === "rejected" && /^[0-9a-f]{32}$/.test(cbody.run_id ?? ""));
  check("the panel shows the decision as the investigator's", (await text('[aria-label="Investigation closed"]')).includes("Rejected by investigator"));
  check("the agent and decision buttons are gone", !(await btn("Run investigation agent")).asElement() && !(await btn("Need more information")).asElement());
  check("the queue and the top bar show Rejected", (await page.$$eval(".q-item", (e) => e.some((b) => b.textContent.includes("CLM-E01") && b.textContent.includes("Rejected")))) && (await text(".claim-strip")).includes("Rejected"));
  const sealedE01 = await api("GET", "/api/claims/CLM-E01/sealed-report");
  check("the sealed report is the one that was read", sealedE01.run_id === cbody.run_id);
  check("the server refuses new runs and decisions on the closed claim (409)",
    (await statusOf("POST", "/api/claims/CLM-E01/agent", {})) === 409 && (await statusOf("POST", "/api/claims/CLM-E01/disposition", { disposition: "needs_info" })) === 409);
  q = mark();
  await click("View sealed report");
  await waitFor(() => document.querySelector('[aria-label="Sealed report"]'), "sealed report shown");
  check("viewing the sealed report reads it once and never runs the agent", posts(since(q), "/agent").length === 0 && since(q).filter((r) => r.path.endsWith("/sealed-report")).length === 1);

  // 3. the memory loop: the later related claim now scores the history bonus
  await page.evaluate((t) => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes(t)).click(), T);
  await waitFor(() => document.querySelector(".claim-strip")?.textContent.includes("CLM-E03"), "reselect target");
  await sleep(800);
  const after = await api("GET", `/api/claims/${T}/investigation?hops=3`);
  check("later related claim gains the confirmed-outcome bonus (+15)", after.score === Math.min(100, baseline.score + 15) && after.factors.some((f) => f.name === "confirmed_fraud_history"), `before=${baseline.score} after=${after.score}`);
  check("the UI shows the new score and the prior outcome", (await score()) === after.score && (await text(".evidence")).includes("Confirmed Fraud"));

  console.log("\nFNOL intake");
  m = mark();
  await click("Looks clean on its own");
  await click("Submit synthetic FNOL");
  await waitFor(() => document.querySelector(".card .note.ok")?.textContent.includes("Received"), "fnol ok", 15000);
  const fp = posts(since(m), "/api/fnol");
  check("exactly one FNOL POST", fp.length === 1);
  const sent = JSON.parse(fp[0].body);
  await sleep(1200);
  check("the new claim appears in the queue and is selected", (await text(".claim-strip")).includes(sent.claim_id));
  const newInv = await api("GET", `/api/claims/${sent.claim_id}/investigation?hops=3`);
  check("it is connected to the seeded ring by real graph queries", newInv.connected_claims >= 3 && newInv.factors.some((f) => f.name === "shared_claimant_payout"), `connected=${newInv.connected_claims}`);
  check("the UI score equals the API score for the new claim", (await score()) === newInv.score);
  check("the success message reports the real policy checks", (await text(".card .note.ok")).includes("policy checks passed"));
  check("it joins the seeded ring, and the intake shows the ring alert", !!newInv.ring_alert && (await text(".ring-alert")).includes(newInv.ring_alert.ring_id));
  m = mark();
  await click("Isolated claim");
  await click("Submit synthetic FNOL");
  await sleep(1500);
  await waitFor(() => document.querySelector(".g-score"), "isolated");
  check("an isolated claim shows no connections and no invented evidence", (await text(".evidence")).includes("No supporting connection") || (await text(".evidence")).includes("No connected") || (await page.$$eval(".ev-item", (e) => e.length)) === 0);
  check("an isolated claim scores 0", (await score()) === 0);
  check("an isolated claim states the explicit 'no supporting connection' outcome", (await text(".outcome")).includes("No supporting connection found in the graph"));
  check("an isolated claim shows no ring alert", !(await page.$(".ring-alert")));

  console.log("\nReset demo (restores the snapshot taken right after seeding)");
  check("the API offers a reset for this graph", (await api("GET", "/api/meta")).demo_reset_available === true);
  m = mark();
  await click("Reset demo");
  await sleep(300);
  check("the first click only arms the reset", posts(since(m), "/api/demo/reset").length === 0);
  await click("Click again to reset");
  await waitFor(() => document.querySelector(".banner.ok")?.textContent.includes("Demo reset"), "reset done", 15000);
  await sleep(1000);
  check("exactly one reset POST", posts(since(m), "/api/demo/reset").length === 1);
  const afterReset = await api("GET", "/api/claims?limit=100");
  check("every claim submitted during the run is gone", eq(afterReset.claims.map((c) => c.claim_id).sort(), ["CLM-E01", "CLM-E02", "CLM-E03", "CLM-E04", "CLM-E05"]), afterReset.claims.map((c) => c.claim_id).join(","));
  check("every outcome recorded during the run is gone", afterReset.claims.every((c) => !c.disposition && !c.investigation_outcome));
  check("the closed investigation is open again", !(await api("GET", "/api/claims/CLM-E01")).closure);
  check("the queue shows the restored claims", (await page.$$eval('[aria-label="Claim queue"] .q-item', (e) => e.length)) === 5);
  const restored = await api("GET", `/api/claims/${T}/investigation?hops=3`);
  check("the score is back to its starting value (no +15 from the outcome recorded earlier)", restored.score === baseline.score, `now=${restored.score} start=${baseline.score}`);
  check("the UI shows the restored score", (await score()) === baseline.score);
  check("a reset never calls the agent", posts(since(m), "/agent").length === 0);

  console.log("\nPage health");
  check("no console errors or uncaught exceptions", problems.length === 0, problems.join(" | "));
  // 4 clicks in this script: Run + Run again on the target, then one report each on E05 and E01 before deciding.
  check("the agent was only ever called by explicit button clicks", posts(reqs, "/agent").length === 4);

  await browser.close();
}

let failed = false;
try {
  await main();
} catch (e) {
  console.error("\nE2E aborted:", e.message);
  failed = true;
} finally {
  cleanup();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} checks passed${bad ? `, ${bad} FAILED` : ""}`);
process.exit(failed || bad ? 1 : 0);
