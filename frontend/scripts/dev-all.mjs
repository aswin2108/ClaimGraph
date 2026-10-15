// One command for local development: the Go API (127.0.0.1:8080) and the Vite dev server (5173).
//
//   npm run dev:all
//
// FalkorDB must already be running (docker). Ctrl+C stops both.
import { spawn, spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const frontend = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const backend = resolve(frontend, "..", "backend");

const children = [
  spawn("go", ["run", "./cmd/api"], { cwd: backend, stdio: "inherit" }),
  spawn(process.execPath, [join(frontend, "node_modules/vite/bin/vite.js")], { cwd: frontend, stdio: "inherit" }),
];

let stopping = false;
function stop(code = 0) {
  if (stopping) return;
  stopping = true;
  for (const c of children) {
    try {
      if (process.platform === "win32") spawnSync("taskkill", ["/PID", String(c.pid), "/T", "/F"]);
      else c.kill("SIGTERM");
    } catch {}
  }
  process.exit(code);
}

// If either side dies (for example FalkorDB is not running, so the API exits), stop the other and say so.
for (const c of children) c.on("exit", (code) => !stopping && (console.error(`\nA dev process exited (code ${code}). Stopping.`), stop(code ?? 1)));
process.on("SIGINT", () => stop(0));
process.on("SIGTERM", () => stop(0));
