import type {
  AgentResult,
  ClaimDetail,
  ClaimSummary,
  Counterfactual,
  DispositionCase,
  FNOL,
  FNOLResult,
  GraphView,
  Meta,
  Report,
  Ring,
  StrongestLink,
} from "./types";

export class ApiError extends Error {
  status: number;
  problems: string[];
  constructor(status: number, message: string, problems: string[] = []) {
    super(message);
    this.status = status;
    this.problems = problems;
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, init);
  } catch {
    throw new ApiError(0, "Cannot reach the ClaimGraph API. Is it running on 127.0.0.1:8080?");
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    // The dev/preview proxy answers 502/503/504 (with a non-JSON body) when nothing is listening on the API port.
    if ([502, 503, 504].includes(res.status) && !body?.error) {
      throw new ApiError(res.status, "The ClaimGraph API is not running. Start it with `npm run dev:all`, or `go run ./cmd/api` in backend/.");
    }
    throw new ApiError(res.status, body?.error ?? `Request failed (${res.status})`, body?.problems ?? []);
  }
  return body as T;
}

const json = (method: string, data?: unknown): RequestInit => ({
  method,
  headers: { "Content-Type": "application/json" },
  body: data === undefined ? undefined : JSON.stringify(data),
});

const enc = encodeURIComponent;

export const api = {
  meta: () => call<Meta>("/api/meta"),
  claims: () => call<{ claims: ClaimSummary[] }>("/api/claims?limit=100").then((r) => r.claims),
  claim: (id: string) => call<ClaimDetail>(`/api/claims/${enc(id)}`),
  investigation: (id: string, hops: number) => call<Report>(`/api/claims/${enc(id)}/investigation?hops=${hops}`),
  graph: (id: string, hops: number, exclude?: string) =>
    call<GraphView>(`/api/claims/${enc(id)}/graph?hops=${hops}${exclude ? `&exclude=${enc(exclude)}` : ""}`),
  counterfactual: (id: string, exclude: string, hops: number) =>
    call<Counterfactual>(`/api/claims/${enc(id)}/counterfactual`, json("POST", { exclude, hops })),
  disposition: (id: string, disposition: string, notes: string) =>
    call<DispositionCase>(`/api/claims/${enc(id)}/disposition`, json("POST", { disposition, notes })),
  agent: (id: string, hops: number) => call<AgentResult>(`/api/claims/${enc(id)}/agent`, json("POST", { hops })),
  ring: (id: string) => call<Ring>(`/api/claims/${enc(id)}/ring`),
  link: (id: string, other: string) => call<StrongestLink>(`/api/claims/${enc(id)}/link/${enc(other)}`),
  submitFNOL: (f: FNOL) => call<FNOLResult>("/api/fnol", json("POST", f)),
};
