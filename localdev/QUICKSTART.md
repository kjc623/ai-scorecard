# Quickstart — the coding-agent harnesses

The harnesses are containers on the lab's network with the repository at `/workspace`, the host's
Docker socket and the Docker Desktop MCP gateway. Run everything from `C:\architecture` in
PowerShell. The lab itself is described in `README.md`.

## 1. The lab

Docker Desktop restarts the lab's containers with Docker. If it is not up (`docker ps` shows no
`sac-lab-*` containers), start it:

```powershell
node localdev/run.mjs
```

## 2. The MCP gateway (its own terminal)

```powershell
node localdev/harness/mcp-gateway.mjs
```

It is ready when it prints `Start streaming server on port 8811`, which takes about 30 seconds.
Closing the window stops it. Without it the harnesses still work; they report the `docker` MCP
server as unreachable.

## 3. A harness

```powershell
docker compose -f localdev/harness.compose.yaml run --rm pi
docker compose -f localdev/harness.compose.yaml build opencode     # once
docker compose -f localdev/harness.compose.yaml run --rm opencode
docker compose -f localdev/harness.compose.yaml run --rm opencode bash
```

Pi's `/mcp` shows whether the gateway is connected. Provider keys (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY`, `DEEPSEEK_API_KEY`) are passed through from
the PowerShell session, e.g. `$env:DEEPSEEK_API_KEY = "…"` before `run`. Logins and configuration
persist in the `pi-home` and `opencode-home` volumes.

Inside a harness the lab's services answer by name on port 8080 (`control-api`, `ingest-api`,
`query-api`, `content-vault`, `dashboard`), the edge at `https://edge:8443`, and `psql` reaches the
lab's database as its superuser (the `PG*` variables are set).

## When something goes wrong

| Symptom | Cause and fix |
|---|---|
| `network sac-lab declared as external, but could not be found` | The lab is not up: step 1. |
| Pi's `docker` MCP server is not connected | The gateway is not running, or was started after a token change: restart step 2, then the harness. |
| The gateway fails on port 8811 | An old gateway is still running: close its window, or set `$env:MCP_GATEWAY_PORT` (and `MCP_GATEWAY_UPSTREAM` and Pi's `mcp.json` to match). |
| `prisma-postgres` tools are missing | Run `docker mcp oauth authorize prisma-postgres`, then restart step 2. |
| A change under `localdev/harness/` has no effect | Rebuild: `docker compose -f localdev/harness.compose.yaml build pi` (or `opencode`). |

`localdev/.env` holds the gateway token and any `LAB_*` overrides; it is not in git.
