# Deploying the public demo

One Linux server runs the same Docker Compose stack as a local run, with two additions:
- **Caddy** for HTTPS (free certificate, renewed automatically);
- a **public nginx config** with per-visitor rate limits, plus a lower AI budget.

Only ports 80 and 443 are open to the internet. FalkorDB, its Browser, the API and the web container stay on
`127.0.0.1` of the server, exactly as in `docker-compose.yml`.

## What it needs

- Ubuntu 24.04, **2 GB RAM or more** (the live demo runs on a 2 GB / 1 vCPU DigitalOcean droplet with 2 GB swap).
- A hostname that points at the server. Without buying a domain, use `<ip-with-dashes>.sslip.io`
  (for example `203-0-113-7.sslip.io`); sslip.io resolves it to that IP.

## Steps

```bash
# 1. Docker (official repository) and a firewall that allows only SSH and the web ports
#    https://docs.docker.com/engine/install/ubuntu/
ufw allow OpenSSH && ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 443/udp && ufw --force enable

# 2. The code
git clone https://github.com/aswin2108/ClaimsAgent.git /opt/claimgraph && cd /opt/claimgraph

# 3. Settings (both files are git-ignored)
echo "SITE_ADDRESS=203-0-113-7.sslip.io" > .env          # your hostname
mkdir -p Secrets && nano Secrets/.env                     # optional: API_Gemini=<a separate, capped key>

# 4. Start (the first build takes several minutes on 1 vCPU)
docker compose -f docker-compose.yml -f docker-compose.deploy.yml up -d --build
```

Open `https://<your hostname>`. The first visit can take a few seconds while Caddy gets the certificate.

## Limits on the public site

| What | Limit |
|---|---|
| Any API call | 10 per second per visitor (bursts of 60) |
| Submitting, challenging, deciding, closing, resetting | 20 per minute per visitor |
| AI agent runs | 3 at once, then 1 per minute per visitor; at most 10 per hour and 300,000 tokens per day for the whole site |

When the AI budget is spent, the agent button returns the deterministic explanation without calling the model.
Change the budget with `AGENT_MAX_RUNS_PER_HOUR` / `AGENT_DAILY_TOKEN_BUDGET` in `.env`; turn the AI off with
`AGENT_ENABLED=false`.

## Shared demo

Every visitor sees the same graph. If someone has already closed the showcase claim, **Reset demo** (top right,
two clicks) restores the starting state. A nightly reset can be added with cron:

```bash
# 21:30 UTC = 03:00 IST
30 21 * * * curl -fsS -X POST -H 'Content-Type: application/json' -d '{"confirm":"reset"}' http://127.0.0.1:8080/api/demo/reset >/dev/null
```

## Updating

```bash
cd /opt/claimgraph && git pull
docker compose -f docker-compose.yml -f docker-compose.deploy.yml up -d --build
```

The graph lives in the `falkordb-data` volume and survives rebuilds; `docker compose ... down -v` wipes it and the
next start seeds it again.
