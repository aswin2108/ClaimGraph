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

function cleanup() {
  for (const p of procs) {
    try {
      if (process.platform === "win32") spawnSync("taskkill", ["/PID", String(p.pid), "/T", "/F"]);
      else p.kill("SIGKILL");
    } catch {}
  }
  // drop the scratch graph
  spawnSync("docker", ["exec", spawnSync("docker", ["ps", "-q", "--filter", "ancestor=falkordb/falkordb"]).stdout.toString().trim().split("\n")[0], "redis-cli", "GRAPH.DELETE", GRAPH]);
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
  const waitFor = async (fn, what, ms = 10000) => {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      try {
        if (await page.evaluate(fn)) return true;
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

  console.log("\nReplay traversal (client side only)");
  let m = mark();
  await click("▶ Replay traversal");
  await sleep(300);
  const during = await cyInfo();
  check("replay starts by hiding later hops", during.hidden > 0, JSON.stringify(during));
  await waitFor(() => window.__cy.elements(".hidden").length === 0, "replay done", 8000);
  check("replay ends with the whole graph visible", (await cyInfo()).hidden === 0);
  check("replay sends no network requests", since(m).length === 0);

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

  console.log("\nDisposition buttons and the memory loop");
  // 1. a plain outcome on a different claim
  let q = mark();
  await page.evaluate(() => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes("CLM-E05")).click());
  await waitFor(() => document.querySelector(".claim-strip")?.textContent.includes("CLM-E05"), "select E05");
  await page.type("textarea", "e2e note");
  await click("Not Substantiated");
  await waitFor(() => document.querySelector(".note.ok"), "disposition ok");
  const dp = posts(since(q), "/disposition");
  check("one disposition POST with the typed notes", dp.length === 1 && eq(JSON.parse(dp[0].body), { disposition: "not_substantiated", notes: "e2e note" }));
  check("the queue now shows the recorded outcome", (await page.$$eval(".q-item", (e) => e.some((b) => b.textContent.includes("CLM-E05") && b.textContent.includes("Not Substantiated")))));
  check("notes were cleared after saving", (await page.$eval("textarea", (e) => e.value)) === "");
  const stored = await api("GET", "/api/claims?limit=100");
  check("the API agrees it was stored", stored.claims.find((c) => c.claim_id === "CLM-E05")?.disposition === "not_substantiated");

  // 2. confirmed fraud needs two clicks; record it on the OLD ring claim E01
  await page.evaluate(() => [...document.querySelectorAll(".q-item")].find((b) => b.textContent.includes("CLM-E01")).click());
  await waitFor(() => document.querySelector(".claim-strip")?.textContent.includes("CLM-E01"), "select E01");
  q = mark();
  await click("Confirmed Fraud");
  await sleep(300);
  check("first click on Confirmed Fraud sends nothing", posts(since(q), "/disposition").length === 0);
  await click("Click again to confirm");
  await waitFor(() => document.querySelector(".note.ok"), "confirmed recorded");
  check("second click sends exactly one POST", posts(since(q), "/disposition").length === 1 && JSON.parse(posts(since(q), "/disposition")[0].body).disposition === "confirmed_fraud");

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
  m = mark();
  await click("Isolated claim");
  await click("Submit synthetic FNOL");
  await sleep(1500);
  await waitFor(() => document.querySelector(".g-score"), "isolated");
  check("an isolated claim shows no connections and no invented evidence", (await text(".evidence")).includes("No supporting connection") || (await text(".evidence")).includes("No connected") || (await page.$$eval(".ev-item", (e) => e.length)) === 0);
  check("an isolated claim scores 0", (await score()) === 0);

  console.log("\nPage health");
  check("no console errors or uncaught exceptions", problems.length === 0, problems.join(" | "));
  check("the agent was only ever called by explicit button clicks", posts(reqs, "/agent").length === 2);

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
