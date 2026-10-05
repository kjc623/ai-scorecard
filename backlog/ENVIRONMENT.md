# The environment

What the harness is, what it can reach, and the commands that work in it. `AGENTS.md` holds the
rules; this file holds the facts. Read it before you run anything. Task 00 runs every command here
and prunes what is no longer true or no longer needed.

## Where you are

You are in the OpenCode harness: Fedora, workspace at `/workspace`, with Go, Node, `psql`, `rg`, a
headless Chromium and the host's Docker engine through the mounted socket. `git status` is never
clean here: it also shows `.claude/`, `skills-lock.json` and a locally modified
`installer/profiles/lab-host.env`, none of which is yours.

The harness is on the network of **the device-auth lab** (`localdev/authlab.compose.yaml`, network
`scorecard-authlab`, containers named `sac-authlab-*`). This is the lab a real endpoint agent is
enrolled against and the one this backlog was observed on. Everything in it is reachable by
service name:

- Plain `psql` reaches its database: `postgres://postgres:sac-lab-only@postgres:5432/shadow?sslmode=disable` (the password is a compose literal, not a secret). That DSN is what `SAC_PG_DSN` should carry for a Go service or tagged test run from the harness; the control-api/ingest-api tagged tests default to `127.0.0.1:5432`, which is not the lab, so they skip here.
- `$LAB_QUERY_URL`, `$LAB_INGEST_URL` and `$LAB_VAULT_URL` are its `query-api`, `ingest-api` and
  `content-vault`. The device path is the edge, `https://edge:8443`, with the lab's own
  certificate.

## The two tenants

The database holds two tenants, and each has its own dashboard:

| | The owner's tenant | The sample tenant |
|---|---|---|
| Tenant id | `11111111-1111-1111-1111-111111111111` | `5a3c0de0-7e57-4a11-9000-0000000d3a01` |
| Holds | What the owner's Windows device sent: real prompts, stored content, one real person | What the device simulator sent: invented devices, people and events, no stored content |
| Dashboard | `http://dashboard:8787` (on the host, port 8787) | `http://dashboard-sample:8787` (on the host, port 8788) |
| You may | Read it. Observe its pages. | Fill it, reshape it and empty it as the task needs. |

Simulated data goes in the sample tenant and nowhere else:

- `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --devices 8 --events 240`
  enrols devices and sends events and health reports through the device path, into the sample
  tenant, which is its default. It was accepted in full from the harness, and the pages fill
  within one aggregator interval (30 s). Never pass it `--tenant` with the owner's tenant id.
- Use it freely to make a page show what you need to see: enough people to clear k-suppression, a
  degraded collector, a quiet device, an empty state. A row you need that the simulator cannot
  produce, you may write into the sample tenant with `psql`.
- Observe it with the full address:
  `node query/dashboard/tools/observe.mjs 'http://dashboard-sample:8787/index.html?transport=live#tools'`.

The owner's tenant is live, and the owner looks at it. Do not change it by any route: not with
`psql`, not with simulated traffic, and not through the product's own writes (a sanction decision,
a setting, a finding review, an erasure). Reading it writes audit rows, which is expected. A clause
that is about what the real device sent (its prompts, its tool fingerprints, its stored content) is
checked there, by reading. Everything else is checked in
the sample tenant. Say in the verdict table which tenant each clause was observed in.

A schema change is the one thing that touches both, because the tenants share a database. Apply
it, and say in the report exactly what you ran.

There is a second lab, the default lab (`localdev/docker-compose.yml`, network `scorecard`), with
the same service names. The harness is not on its network and no task here uses it. If a document
or a script says "the lab" and means that one, it does not mean yours.

## Lab commands

- Start the device-auth lab: `docker compose -f localdev/authlab.compose.yaml up -d`.
- **Never run `node localdev/run.mjs`, with any flag.** With `--auth` it regenerates the lab
  certificate authority, which orphans the agent installed on the owner's Windows machine. Without
  it, it brings up or smoke-tests the default lab, and from this harness its `--no-up` form would
  post test events into the device-auth lab. Some file headers still tell you to run it; they
  predate this harness.
- Rebuild the lab images after changing a service: `node localdev/build.mjs --auth`, then
  `docker compose -f localdev/authlab.compose.yaml up -d <service>`.
- Rebuild the dashboard image: `docker build -f query/dashboard/Dockerfile -t sac/dashboard:lab .`
  from the repository root, then `up -d dashboard dashboard-sample`. Both dashboards run that
  image.
- After editing `query/dashboard/src/` or its templates, run `node tools/build-index.mjs` in
  `query/dashboard/`; the generated `index.html` and `explore.html` are checked for drift by tests.
- The endpoint agent runs on the owner's Windows host, installed from an MSI
  (`installer/lab-msi.mjs`). You cannot reinstall it from the harness. If a task needs a new agent
  build on the device, build it, say exactly what the owner must run, and verify what you can with
  the device simulator, in the sample tenant. The simulator uses the same path a real agent uses.
  What it cannot show is that the installed agent does the same, which is why each task's "Done
  when" leaves the real device to the owner.

## Seeing the dashboard

- `node query/dashboard/tools/observe.mjs '<address>'` opens one dashboard address in the headless
  browser, waits for the page's reads to finish, and prints the text a person would read there. It
  saves that text, a full-page screenshot and the rendered DOM under `.integration/observe/`, which
  is not committed. For example:
  `node query/dashboard/tools/observe.mjs 'index.html?transport=live#devices' --expect Reporting`.
  A bare path like that one opens the owner's dashboard; give the full address for the sample
  tenant's. Quote the address. The header of the script documents `--expect`, `--absent` and `--click`.
- This is how a claim about what a page shows is checked. A POST to the dashboard's `/v1/query`
  forwarder shows that the data is there; it does not show that the page renders it.
- The script loads, waits, clicks and reads. If a clause needs more (typing into a field, a
  signed-in session, a download), extend the script rather than falling back to the forwarder, and
  say in the report what you added.
- Read the whole printed page, not only the line you were looking for. The header counts console
  errors, failed requests and requests to other hosts; a count that is not 0 goes in your report.
- The page you observe is the dashboard image, not your working tree. Rebuild and restart the
  dashboard before observing a change to it.
- If the script reports that no browser was found, the harness image predates it. Install it as
  root from inside the harness, using this container's name from `docker ps`:
  `docker exec -u 0 <harness container> dnf -y install chromium-headless liberation-sans-fonts liberation-mono-fonts`.

## Tests

- Node packages: `node --test` inside the package directory (`query/dashboard`, `query/query-api`).
  Do not pass a directory argument.
- Go modules: `go test ./...` inside each module you touched. The modules build offline on the host
  with `GOFLAGS=-mod=mod GOPROXY=off`; if a build here wants to download a module, stop and report it
  rather than adding or upgrading a dependency.
- Whole repository: `node tools/accept.mjs`.

Some suites fail in the harness before any work is done. `backlog/BASELINE.md` lists them, with the
cause of each and the commit it was measured at; task 00 writes it. Run the suites you will touch
before you change anything and compare with that file. Your report names only the differences: a
failure that is not in the baseline, or a baseline failure that now passes. Do not explain the
baseline again. If the file does not exist, do task 00 first.
