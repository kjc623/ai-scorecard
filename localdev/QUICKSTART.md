# Quickstart — back up and running after a reboot

A reboot stops the lab containers and the MCP gateway. The images, the database volume, your Pi
login and the gateway token all survive, so getting back is three commands.

Run everything from `C:\architecture` in PowerShell.

## 1. Make sure Docker Desktop is running

Start Docker Desktop and wait until it says the engine is running. Check with:

```powershell
docker ps
```

An empty table is fine. An error means Docker is not ready yet.

## 2. Start the lab

```powershell
node localdev/run.mjs
```

This starts PostgreSQL, `ingest-api`, `content-vault` and `query-api`, then runs the smoke test.
It should end with `lab: all checks passed` (18 checks). The lab stays running afterwards.

## 3. Start the MCP gateway (its own terminal)

Open a second PowerShell window and leave this running:

```powershell
node localdev/harness/mcp-gateway.mjs
```

It is ready when it prints `Start streaming server on port 8811`, which takes about 30 seconds.
Closing this window stops the gateway. Skip this step if you do not need MCP tools; Pi still
works, it just reports the `docker` MCP server as unreachable.

## 4. Start Pi

Back in the first window:

```powershell
docker compose -f localdev/harness.compose.yaml run --rm pi
```

Pi opens in `/workspace`, which is this repository. Inside Pi, `/mcp` shows whether the gateway is
connected.

## Start OpenCode in Fedora

The Fedora harness joins the same `scorecard` network and has the same workspace, Docker socket,
lab environment variables and MCP gateway forwarding. Build it once, then start OpenCode with:

```powershell
docker compose -f localdev/harness.compose.yaml build opencode
docker compose -f localdev/harness.compose.yaml run --rm opencode
```

OpenCode's Docker MCP entry is installed on first start. Its configuration and provider login persist
in the `opencode-home` volume. To open a Fedora shell instead, run:

```powershell
docker compose -f localdev/harness.compose.yaml run --rm opencode bash
```

To use DeepSeek, set its API key in the same PowerShell session before starting OpenCode. Compose
passes this host variable into the container; keep the key out of the Compose file:

```powershell
$env:DEEPSEEK_API_KEY = "your-api-key"
docker compose -f localdev/harness.compose.yaml run --rm opencode
```

## What the harness can reach

| Thing | Address from inside the harness |
|---|---|
| `ingest-api` | `http://ingest-api:8080` (`$LAB_INGEST_URL`) |
| `content-vault` | `http://content-vault:8080` (`$LAB_VAULT_URL`) |
| `query-api` | `http://query-api:8080` (`$LAB_QUERY_URL`) |
| PostgreSQL | plain `psql` works; the `PG*` variables are set |
| Docker | `docker ps`, `docker logs`, `docker compose ... exec` — the host's engine, all containers |
| MCP gateway | `http://localhost:8811/mcp`, configured for Pi and OpenCode |

From the host the services are on `localhost:8080`, `8081` and `8082`, and PostgreSQL on `5432`.

## Quick checks

```powershell
# Is the lab up?
docker ps --format "{{.Names}}  {{.Status}}"

# Does Pi see the MCP gateway? Expect "docker: connected, N tools".
docker compose -f localdev/harness.compose.yaml run --rm pi pi mcp list

# Does the lab pass its smoke test from inside the harness?
docker compose -f localdev/harness.compose.yaml run --rm pi node localdev/run.mjs --no-up
```

## The device-auth lab, the dashboard and an installed agent

The lab above is the default one. The device-auth lab is a second, separate stack: the one a real
endpoint agent enrols against, with the content vault and the dashboard. After a reboot, bring it
back with:

```powershell
docker compose -f localdev/authlab.compose.yaml up -d
```

Use that command, not `node localdev/run.mjs --auth`, to restart it. `run.mjs --auth` is for the first
start or a wiped lab: it regenerates the lab's development CA every time, which orphans an agent
already installed on this machine until its MSI is rebuilt (`node installer/lab-msi.mjs`) and
reinstalled.

| Thing | Address |
|---|---|
| Dashboard (events, prompt-text search, content retrieval) | <http://127.0.0.1:8787/explore.html?transport=live> |
| Edge, where the agent sends | `https://127.0.0.1:8443` |
| PostgreSQL | `localhost:55435` |
| Table browser (optional) | <http://127.0.0.1:8089>, after `docker compose -f localdev/dbview.compose.yaml up -d` |

The installed agent is a Windows service and starts by itself at boot (`sc.exe query ShadowAICapture`).
It holds what it captures until the lab is back; delivery after a lab outage was not tested.

## Shutting down

```powershell
docker compose -f localdev/docker-compose.yml stop    # stop the lab, keep the database
node localdev/run.mjs --down                          # or: remove the lab AND its database volume
docker compose -f localdev/authlab.compose.yaml stop  # stop the device-auth lab, keep its database
```

Stop the gateway with Ctrl+C in its window. Exit Pi normally; its container is removed on exit and
its login stays in the `pi-home` volume.

## When something goes wrong

| Symptom | Cause and fix |
|---|---|
| `network scorecard declared as external, but could not be found` | The lab is not up. Run step 2 first. |
| `compose up failed` or an image is missing in step 2 | The lab images are gone. Run `node localdev/build.mjs`, then step 2 again. |
| `port is already allocated` in step 2 | Something else holds 5432, 8080, 8081 or 8082. Move the lab's port, for example `$env:LAB_PG_PORT = "5433"`, and rerun. |
| Pi's `docker` MCP server is not connected | The gateway is not running, or was started after a token change. Restart step 3, then restart Pi. |
| Gateway fails on port 8811 | An old gateway is still running. Close its window, or pick another port with `$env:MCP_GATEWAY_PORT` (and update `MCP_GATEWAY_UPSTREAM` and Pi's `mcp.json` to match). |
| `prisma-postgres` tools are missing | That server is not authorised. Run `docker mcp oauth authorize prisma-postgres`, then restart step 3. |
| Pi asks you to log in again | The `pi-home` volume was removed. Log in once; it persists from then on. |
| Changed `localdev/harness/pi/` and nothing happened | Rebuild: `docker compose -f localdev/harness.compose.yaml build pi`. |

## Where things live

| File | What it is |
|---|---|
| `localdev/docker-compose.yml` | The lab: services, ports, the `scorecard` network |
| `localdev/authlab.compose.yaml` | The device-auth lab: its own PostgreSQL, the SQL-mode services, the edge, the content vault, and the dashboard |
| `localdev/dbview.compose.yaml` | The optional read-only table browser over the device-auth lab's database |
| `localdev/harness.compose.yaml` | The harness containers that join the lab's network |
| `localdev/harness/pi/` | The Pi image: Dockerfile, entrypoint, MCP config |
| `localdev/harness/fedora/` | The Fedora image: Dockerfile, entrypoint, OpenCode MCP config |
| `localdev/harness/mcp-gateway.mjs` | Starts the Docker Desktop MCP gateway for the harnesses |
| `localdev/.env` | The gateway token and any `LAB_*_PORT` overrides. Not in git; do not delete it while the gateway is running |
| `localdev/README.md` | How the lab works, in detail |
