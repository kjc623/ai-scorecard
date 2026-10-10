# Handoff: device phase, 2026-10-10

For the agent continuing the device phase (`AGENTS.md`, `refactor-endpoint/AGENTS.md`, tasks 58–60).
Read `TESTBED.md` and the last entries of `DECISIONS.md` with this. Everything here is fact as of
the date above; the PRs named near the end may have merged since.

## Where things stand

- **Pre-prod is up** (task 58 done): Fly.io org `personal`, apps `sac-preprod-{edge,ingest-api,
  control-api,content-vault,query-api,dashboard,jobs,migrate}`, Supabase project
  `hbrvtbiifkmgdcmozxcx` (database `shadow`, session pooler, SSL enforced), hostnames
  `devices.preprod.sundial.solutions` (edge, dedicated IPv4) and `console.preprod.sundial.solutions`
  (dashboard). Deploys run from a push to `main` (`.github/workflows/deploy.yml`): agent release,
  images, `migrate` as the migrate app's release command, then each app. Verify checks 1–6 passed.
- **The reference VM is enrolled and reporting** (task 59 all but ticked): Hyper-V `WIN11-TEST`,
  hostname `WinDev2407Eval`, Intune-enrolled by hand (no Entra P1; the owner declined a trial),
  agent installed through Intune (`deploy.mjs` published the Win32 app, id in `TESTBED.md`), device
  id `5b2bab66-9ed2-4f32-b952-27c5c45e4293`, mode m3, extension installed by Intune's force-install
  policy and connected to the agent (`capture_extension healthy`). Events reach PostgreSQL and the
  dashboard. Not yet done for task 59: the "replaces the first version through Intune" check (run
  `deploy.mjs` again after the next merge) and `-Screenshot`; `-AsUser second` is not run (one user).
- **Task 60 has not started its loop.** What has been done is fixing what the first real prompt
  exposed on the console (below). The first brief's "On the device" section is next.

## The owner's rules, learned the hard way

- Step-by-step instructions, one step at a time, every command labelled with the window it runs in
  (Windows PowerShell as administrator at `C:\architecture` for `invm.ps1`/`deploy.mjs`; Git Bash at
  `/c/architecture` for the `fly/scripts/*.sh`; the browser) and no placeholder without saying
  where its value comes from. Documentation is the agent's job, never the owner's.
- The owner merges PRs; never ask them to check out a branch. Base every PR on `main` and say so.
  A PR based on another PR's branch merged into that branch, not into `main` (#21), and had to be
  re-opened as #22.
- `invm.ps1` is called as `& .\tools\testbed\invm.ps1 -Command '...'` from PowerShell, never
  `powershell -File`.
- The product is now "Sundial" in anything customer-facing; the code is unchanged.
- No Microsoft trials. One Entra user (`kyle@sundial.solutions`), `admin@sundial.solutions` is the
  tenant admin.
- Decisions taken on the console: an admin has every capability (#20); one sanction decision and
  one mode override per tool, the product manages fingerprints (#21/#22); the rollup writes no
  zero rows for buckets with no data (#23); **no k-suppression anywhere** (#24, the owner withdrew
  the five-person rule); the extension's fingerprint is the destination host alone, `tf2:` (#25);
  one row per device on the Devices page (#26, fixed by #27).

## PRs of this session (merge order; all to `main`)

#18 CI/loopback/testbed (merged), #19 TESTBED docs (merged), #20 admin role (merged, deployed),
#22 per-tool sanction (merged, deployed; migration 0018), #23 rollup (merged, deployed), #24 no
suppression (merged, deployed), #25 `tf2` fingerprints (merged, deployed; migration 0019), #26 one
row per device (merged; its migrate step **failed**: 0020 cast a view column's type), #27 the fix
(open at handoff; migration 0020 proven over the previous view). After #27 deploys, pre-prod
carries migrations 0018–0020.

## What to do next, in order

1. Confirm run for #27 is green (Actions → deploy). If `migrate` fails again, the log is in the
   failed job; `fly logs -a sac-preprod-migrate` has the rest.
2. Owner: `node tools\testbed\deploy.mjs` (PowerShell as administrator, `C:\architecture`) to put
   the current release on the VM; restart Edge in the VM so it fetches the rebuilt extension from
   `https://console.preprod.sundial.solutions/v1/extension/updates.xml`; sign out and in on the
   dashboard; send one ChatGPT prompt. Expected: Overview without refusal banners, Devices one row,
   Tools shows "ChatGPT (web)" with a real number and no suppression. The five old
   "Unrecognised tool tf1:…" rows are history and age out of the 7-day window.
3. Still owed by the owner, asked for several times: a capture of one real ChatGPT request from the
   VM's Edge (F12 → Network → `backend-api` → the POST ending `/conversation` or `/f/conversation`,
   Payload → view source) and the agent log lines with `tool_fingerprint`
   (`Select-String` over `capture-core.log` under `%ProgramData%`). The capture confirms the host
   the extension sees; Gemini and Perplexity hosts in the catalogue are from documentation.
4. Owner: revoke the two spare deployment keys (Settings → Deployment, created 14:12 and 14:16 UTC
   on 2026-10-09 from two accidental `.intunewin` downloads); keep the one whose enrolment count is 1.
5. Finish task 59's Done-when (second `deploy.mjs` run replaces the version; `-Screenshot`), tick
   it in `README.md`, then start task 60 at the first brief with an "On the device" section.
6. Quiet the edge's `TLS handshake error … EOF` log lines from Fly's health checks (small, unassigned).

## How to verify a change before pushing

- `node tools/accept.mjs` (go, node, static; bicep/database/installer/browser skip in the agent
  container, say so). Run it from the repository root; it takes ~5 minutes.
- Database gate locally: `sh /home/user/db-gate.sh` existed only in the previous container. Rebuild
  it: PostgreSQL 16 on port 54329, superuser `postgres`, trust auth; the gate script is
  `services/database/tools/test-database.mjs` adapted to a local server (creates `azure_pg_admin`,
  `sac_admin`, database `sac`, runs `go run ./cmd/migrate` twice, then `invariants.test.sql`).
- **A migration must also be applied over the previous schema**, not only over a fresh database:
  recreate the old object, apply the file, run query-api's live tests
  (`SAC_TEST_PG_DSN=postgres://postgres@127.0.0.1:54329/sac?sslmode=disable node --test` in
  `services/query-api`; the superuser bypasses row-level security, which the live fixtures need,
  and can `SET ROLE`). The same DSN runs control-api's and jobs' live tests.
- In the agent's shell, never rely on the working directory across tool calls: use absolute paths.
  `node --test` takes files or no argument, never a directory.

## Values (no secrets; the files that hold them are named in `TESTBED.md`)

Tenant "Endpoint Test" `84beb829-b508-437c-9cd2-501f36e28b81` (ceiling m3); Entra tenant
`5ac3954b-f7e1-47af-bbab-2ca027d1948f`; Entra app "Sundial (pre-prod)" client
`c9ba435a-79b9-47cb-8f20-dada5160e0d6`; Intune publishing app `6013fcd8-b0fe-4201-9e0c-5bb4a78e7b57`,
certificate `300591A257424963C75D23327A40BED95F0156C5`; test device group
`6a3c290c-2ff1-4c44-bba9-ca6ed29caed6`; Intune app `3c023904-f50e-4612-81ea-d69d5604fb69`; extension
id `jnjjgjlbhfleknjpoiiopcaogphghodk`; edge IPv4 `169.155.51.159`; checkpoint `clean-enrolled`
(Intune policy only, no agent). Supabase CA on the PC at `C:\Users\kyle\prod-ca-2021.crt`; device
certificate by Posh-ACME (90 days).
