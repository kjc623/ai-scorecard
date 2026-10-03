# Documentation accuracy review — 2026-10-03

Every Markdown file in the repository (95 files, about 1.4 MB) was read and compared with the code,
schema, contract and Bicep as they exist on this date. Nothing was built, run or changed. Each
finding gives the document line, then what the code shows. Findings are CONFIRMED (the contradicting
code was read) unless marked LIKELY.

Nine of the highest-impact findings were re-checked by hand after the review; all nine held. They
are marked ✔.

Line numbers are as of the working tree on this date, which includes uncommitted edits to
`localdev/`.

## Contents

1. Themes that recur across the repository
2. Security and deployment claims the configuration contradicts
3. Design docs against the schema and services (docs/00–06, ADRs)
4. Component READMEs (endpoint, extension, contracts, ingestion, vault, query, database)
5. Azure, tooling, lab and verification records
6. Code and configuration problems noticed along the way (not documentation)
7. Checked and found accurate

---

## 1. Themes that recur across the repository

### 1.1 "This host is offline" is no longer true

The machine has outbound network today: the Go module proxy, npm and Docker pulls all work. It
still has no C compiler, so statements about cgo and `-race` remain true. The gates still set
`GOPROXY=off`, but that is now a choice, not a constraint.

| File | Lines |
|---|---|
| `endpoint/README.md` | 47, 71, 76–77 |
| `endpoint/capture-spool/README.md` | 14–15, 81 |
| `endpoint/capture-spool/EVIDENCE.md` | 14–17, 315, 330 |
| `endpoint/classifier-host/README.md` | 8, 31, 123, 130 |
| `endpoint/classifier-host/MEASUREMENTS.md` | 12, 166, 171 |
| `endpoint/classifier-host/TOOLCHAIN-DECISION.md` | title, 6–7, 13, 16, 18, 23, 57, 58, 63 |
| `endpoint/canon/README.md`, `canon/gen/README.md`, `canon/EVIDENCE.md` | 9–10; 4–5; 33 |
| `endpoint/capture-core/README.md` | 43 |
| `contracts/generated/README.md` | 40–41 |
| `ingestion/ingest-api/README.md` | 106, 142–143 |
| `ingestion/ingest-api/internal/README.md`, `sqlpg/README.md` | 16; 10 |
| `vault/README.md` | 31, 40 |
| `vault/content-vault/README.md` | 8, 130, 132, 136 |
| `vault/content-vault/internal/README.md`, `cmd/content-vault/README.md` | 16, 22; 27 |
| `query/query-api/README.md`, `src/pg/README.md` | 19; 5–7 |
| `database/evidence/README.md` | 45, 47, 52, 471 |
| `azure/README.md`, `azure/cost-model.md` | 9–10; 11–13, 188–189 |
| `localdev/README.md` | 102, 143–148, 152–154, 234–235, 240 |
| `docs/lab/LAB-COST.md` | 10–14 |
| `.integration/LEAD-VERIFICATION.md` | 82, 268, 415–416, 608 |

Related image-cache claims are also stale: "azurite and minio are both cached", "the only
PostgreSQL image cached is 17-alpine" and "no golang image is cached" (`localdev/README.md` 102,
143–154; `localdev/build.mjs` 4–6). The cache today holds alpine, node, postgres:17-alpine and
golang:1.27 images, and no azurite or minio.

### 1.2 "No browser is installed" contradicts the browser evidence

Edge is installed and `extension/tools/in-browser-check.evidence.txt` records it loading the
extension and round-tripping through a registered native host.

- `endpoint/capture-core/cmd/capture-core/README.md` 110–111
- `endpoint/integration/README.md` 60–62
- `endpoint/canon/EVIDENCE.md` 289–290
- `docs/lab/LAB-COST.md` 130, 144, 439, 612
- `.integration/LEAD-VERIFICATION.md` 281–282, 586–587, 594
- `.integration/REPORT.md` 441

### 1.3 The device spool is not SQLite

`endpoint/capture-spool` is an append-only encrypted segment log. Its own README says so; these
documents still state SQLite as fact, without the deviation:

- `docs/00-architecture.md` 449, 452
- `docs/01-collectors.md` 1145–1166, 1186, 1209–1210 (also state names `sent`/`rejected_terminal`
  where `endpoint/protocol/spool.go` 24–37 has `delivered`/`rejected`/`dropped`, and
  `mono_offset_ms` where the code has `monotonic_offset_ms`)
- `docs/03-data-platform.md` 49
- `docs/05-platform-delivery.md` 462, 676
- `docs/06-security-and-threat-model.md` 268, 468
- ADR 0002 line 24 and its title; `docs/adr/README.md` 15

### 1.4 TypeScript, Fastify and React are named; the code is plain JavaScript

`query-api` is plain JS on `node:http` with zero dependencies; the dashboard and extension are
plain JS with no framework. `contracts/generated/typescript/envelope.ts` is the only `.ts` file and
nothing imports it.

- `docs/00-architecture.md` 339, 436, 445–446, 725–726
- `docs/01-collectors.md` 97
- `docs/02-ingest-and-transport.md` 207
- `docs/04-dashboard-and-query.md` 3, 61
- ADR 0003 lines 22, 49; ADR 0010 lines 69–70
- `endpoint/README.md` 28

Also `docs/01-collectors.md` 148–149, 613, 815 say the extension makes its inline decision in a
WASM classifier. The extension has no wasm; the predicate is JavaScript
(`extension/src/predicate.js`, `enforce.js`).

### 1.5 PostgreSQL 16 in the design, 17 in the lab and in all verification

`docs/00` 448, `docs/03` 44 and 48, ADR 0002 line 22 and `database/schema.sql` line 2 say 16. The
lab and every recorded verification run used 17. Azure still pins 16
(`azure/modules/postgres.bicep` 21). The READMEs disclose this; the design docs do not.

### 1.6 Old paths from the restructure

The Markdown is almost clean. The two remaining instances:

- `.integration/LEAD-VERIFICATION.md` 52: `db\tools\run-invariants.ps1`
- `database/evidence/README.md` 486 (`db\evidence\*.log`, inside a quotation); 395–396 omit the
  `endpoint/` prefix

Old paths survive widely in code comments and strings; see section 6.

---

## 2. Security and deployment claims the configuration contradicts

1. ✔ **"Only content-vault can unwrap."** `docs/05` 351–353, `docs/06` 352 and 1423 say ingest-api
   holds no unwrap right. `azure/main.bicep` 242–246 assigns ingest-api `cryptoServiceEncryption`,
   which `azure/modules/keyvault.bicep` 47 maps to Key Vault Crypto Service Encryption User. That
   built-in role includes wrap and unwrap. The CI check (`policy-scan.yml` 96–103) inspects only
   `unwrapPrincipalIds` and does not catch it.
2. ✔ **"TLS 1.3 on every hop" and mTLS between services.** `docs/06` 415 and 176–178.
   `azure/main.bicep` 469 passes `SAC_CONTENT_VAULT_URL` as `http://…`;
   `container-apps-env.bicep` 47–50 sets mTLS `enabled: false`; the vault serves plain HTTP
   (`cmd/content-vault/main.go` 224) and trusts identity headers.
3. **"Container Apps environment variables reference Key Vault."** `docs/05` 376–377. Every app
   passes `keyVaultEnv: []` (`main.bicep` 386, 413, 445, 472). ADR 0019 records the consequence;
   docs/05 and docs/06 do not.
4. **Transport binding by `public_key_thumbprint`.** `docs/06` 318. ingest-api never reads that
   column; it derives `credential_id` from the certificate DER
   (`ingestion/ingest-api/internal/auth/auth.go` 138–141).
5. **System-assigned vs user-assigned identity.** `docs/06` 344 says system-assigned; `docs/05` 316
   and the Bicep are user-assigned.
6. ✔ **KEK check constraint misquoted.** `docs/06` 901 and 1463 quote
   `CHECK (ceiling_mode IN ('m0','m1') OR kek_id IS NOT NULL)`. `database/schema.sql` 271–272 is
   `CHECK (ceiling_mode <> 'm3' OR kek_id IS NOT NULL)`; an M2 tenant needs no KEK.
7. **Assumption A11 described as unresolved.** `docs/06` 611–613, 1473. `schema.sql` 286–289 adds
   the constraint and names A11 as what it closes.
8. **`sac_ingest` privileges overstated.** `docs/06` 352. The schema grants SELECT only on device
   tables (`schema.sql` 1926) and no access to `ops.collector_state`. `docs/05` 323–324 describes
   narrow aggregator and reconciler roles; the schema has one shared `sac_ops` role with DELETE on
   the ingest tables (2001–2008).
9. **control-api "sign on the policy-bundle signing key".** `docs/05` 319–320. Its only Key Vault
   assignment is `secretsUser`.
10. **Staged traffic shifting.** `docs/05` 291–293 describes 0→10→50→100% with rollback.
    `container-app.bicep` 89 sets single-revision mode at 100%. No revision pipeline exists.
11. **Migration before traffic (LIKELY).** `docs/05` 186–187. `infra.yml` 120–134 deploys first and
    runs the migration job afterwards.
12. **PostgreSQL private endpoint.** `docs/05` 73, 80–81 and `azure/modules/README.md` 35–36 say
    private endpoint. `postgres.bicep` 103–107 uses delegated-subnet injection.
    `docs/lab/LAB-COST.md` 429 states it correctly.
13. **Hourly resource-graph drift assertions.** `docs/05` 201–210 presents them as existing.
    `azure/README.md` 129–131 says they are not implemented; `drift.yml` only fails the job.
14. **Environment ladder.** `docs/05` 128, 132–134 vs `main.bicep` 209, 403, 462 and `waf.bicep`
    71–73: staging gets 10 replicas, the 365-day archive and the rate-limit rule.
15. **Module descriptions overstate contents.** `docs/05` 74, 144–172: no peerings, no DNS
    resolver beyond an empty subnet, no schemas or roles in `postgres.bicep`, no SLO definitions or
    workbooks, budgets at resource-group scope only. The tree omits `params/lab.bicepparam`,
    `azure/tools/` and the `deployEdge`/`deployDashboard`/`deployExports` switches.
16. **Smaller items in docs/05.** "GPv3" storage (66, 67, 156, 157; the kind is `StorageV2`);
    ingest-api "HTTP/1.1 + HTTP/2" (59; transport is `http2`); "two jobs" (227, 871; there are
    three).
17. **"D6 is permanent."** `docs/05` 1214 and 1056 contradict ADR 0014, `docs/06` 14–16 and
    `docs/05` 90.
18. **Spool key wrapping.** `docs/06` 469 says DPAPI at machine scope; `docs/01` 1172 and 1334 say
    service-account scope; `capture-spool/keys.go` 18–23 ships neither.

---

## 3. Design docs against the schema and services

### docs/02-ingest-and-transport.md — describes a different merge model from the schema

docs/00, docs/03 and the ADRs mostly match `database/schema.sql`; docs/02 does not.

1. **Weak key.** 417 defines a `sac-l1` hash including tier and ladder material. `schema.sql`
   927–938 is a `|`-joined tenant, device, tool, kind, bucket and size.
2. ✔ **Route fidelity ranks are inverted and reordered.** 299–307 has higher-wins ranks (90, 80,
   70, 60, 50, 40, 10). `schema.sql` 2049–2056 has lower-wins ranks (10–70), with `cli.shim`
   second, not fifth. `docs/03` 149 agrees with the schema.
3. **Fidelity formula and tie-break.** 316 and 336 vs `schema.sql` 1790 and 1773–1776 (plain rank,
   first-seen wins).
4. **Columns that "must appear in schema.sql" and do not.** 401, 914–915: `dedup_weak_key`,
   `dedup_tier`, `prompt_digest`, `fidelity`, `route`, `route_rank`, `batch_id`,
   `same_route_observation_count`, `possible_undercount`, `upgraded_at`, `upgrade_count`,
   `superseded_by`. Line 917 names `route`, `route_rank`, `tier_rank`; the columns are `source`,
   `fidelity_rank`, `yields_content`.
5. **Merge confidence.** 432–437 uses high/medium/low; the schema allows high and low (991).
6. **Submission key and write path.** 590, 598–640 show the service issuing
   `INSERT … ON CONFLICT`. The key is `(tenant_id, submission_id)` and the service only calls
   `ingest.record_event()`.
7. **Health channel.** 564, 737, 752–756, 897: keyed per device with `reported_state`, `liveness`
   and skew columns. The schema keys on `(tenant_id, device_id, collector)` with `state` only
   (383–393); liveness values and the 45-minute threshold differ from the view (1296–1301).
8. **`ops.grant`.** 154–163, 918 vs `schema.sql` 533–548: column is `decision`, values differ, no
   key columns.
9. **`ops.content_object.state`.** 165, 919 give four values; the schema has two (622–629), and
   `docs/00` 833–836 agrees with the schema.
10. **`ingest.rejected`.** 693–695, 916 name the wrong columns; 695 says the digest is retained if
    well-formed, but the constraint forbids it (1067–1068). TTL 30 days (702) vs seeded 14 (2072).
11. **Enrolment uniqueness.** 503 and ADR 0005 line 22 cite a unique constraint on a hardware
    identity hash. `ops.device` has no such column (335–350).
12. **Who mints the object key.** 575, 804–825 have control-api return the plaintext key.
    `docs/00` 521, ADR 0006 and the vault code say the vault mints it.
13. **Retrieval endpoint (LIKELY).** 852–857 and `docs/00` 718 put `POST /v1/content/retrieval` on
    query-api. It is served by content-vault.

### docs/00, docs/03 and the ADRs

14. ✔ **The usage ledger is written by nothing.** `docs/03` 539, 565 and ADR 0015 lines 54, 89 say
    ingest-api increments it. No Go or JS file references `usage_daily`, and `record_event` does
    not touch it.
15. **"No extensions are required."** `docs/03` 56–61, ADR 0002 line 50. `schema.sql` 62–63
    creates `pg_trgm` and `btree_gin`. `docs/03` 458 and ADR 0014 say they are needed.
16. **`content_state = local_only` is never set (LIKELY).** `docs/00` 833–835, `docs/03` 347–348.
    Nothing writes it, and ADR 0017 decides the server does not learn this.
17. **Key destruction.** `docs/03` 222, 238 describe deleting `wrapped_dek` by a `sac_vault`
    reconciler. The column is NOT NULL; shredding overwrites the key in content-vault.
18. **PgBouncer (LIKELY).** `docs/03` 70. Nothing in `azure/`, `localdev/` or the services uses it.
19. **Envelope fields.** ADR 0004 line 59 names `collector_version`, which does not exist.
    `docs/00` 745 lists fields as required for `prompt` that the contract requires only at M1+.
20. **ADR 0016 wasm size (LIKELY).** Lines 23, 77 say 2.5 MB; `reports/wasm-size.txt` records
    6,345,103 bytes.
21. **Counts.** `docs/00` 762 ("Two tables", lists three; the schema has four), 892 (Q1–Q13;
    Q14–Q17 follow), 360 ("nine providers"); `docs/03` 213, 231, 375–387 (says 47 assertions,
    lists T1–T27).
22. **References that do not resolve.** `docs/adr/README.md` 62 (`archive/` does not exist);
    `docs/00` 130; `docs/03` 60, 532; ADR 0017 line 12; ADR 0013 line 15.
23. **ADR index.** `docs/adr/README.md` 6 says all are `proposed` while its table marks 0008
    superseded; ADR 0008 carries two Status lines; three index titles differ from the file titles
    (0006, 0010, 0018).

### docs/01-collectors.md

24. **Collector names.** 263–264 says names come from `ref.collector`. The device emits route
    names and `capture-extension`; `ref.collector` (`schema.sql` 2060–2066) holds different
    values, with a foreign key from `ops.collector_state`.
25. **"Names frozen by the contract."** 12–14. The schema freezes only the seven routes.
26. **Tamper and detail vocabulary.** 1211, 1484–1487 name seven causes that are not in the closed
    `protocol.Detail` set (`endpoint/protocol/envelope.go` 140–213).
27. **WASM latency "unmeasured".** 819–820, 1577. `classifier-host/README.md` 49–54 and
    `reports/latency-wasm.json` record measurements.
28. **Parser child "separate executable".** 100. It is a subcommand of the one binary.
29. **Smaller items.** 927 (bundle field is `classifier.release_id`); 276 (`detection_basis` has
    four values, not two); 730 and 1554 ("six signals", table has seven); 1262, 829–830 (§6.1 and
    §9.3 do not exist).

### docs/04-dashboard-and-query.md

30. **Tenant binding.** 650 shows `SET LOCAL app.tenant_id = $1`. The service uses
    `set_config(..., false)` with a mandatory reset on release, because the utility statement
    takes no bind parameter.
31. **"`ingest.submission` has no `kind` column."** 595. `schema.sql` 971 has it; line 629 of the
    same doc says it landed.
32. **`mart.agg_class_period` key.** 203, 489 omit `classifier_version` (`schema.sql` 1229); 294
    and 628 include it.
33. **Measures and dimensions.** 160–163 list three measures that do not exist as measures and
    omit four that do; 152–158 omit about eight dimensions; `starts_with` scope (166–167, 201)
    differs from `DSL.md` 143–146.
34. **Endpoints.** 108–109 list `GET /v1/submissions/{id}` and `GET /v1/findings/{id}`. The only
    routes are `/healthz`, `/readyz` and `POST /v1/query`.
35. **Concurrency.** 1189 says per-tenant 8 and per-user 4. The gate is process-wide with no
    tenant or user key.
36. **Statement timeouts (LIKELY).** 1158–1165 give 3 s and 5 s per class; only the 10 s session
    default is applied. See also section 4, query finding 1.
37. **Secondary indexes.** 449 says the schema has none; it has five. None of the nine indexes at
    455–465 exists.
38. **Other divergences.** 206, 340–343, 778 (Q7 path and cursor); 645 (audit-before-read);
    1249–1253 (no retention-run ledger exists); 200, 841, 1199 (top-N and bucket handling);
    216, 863, 1481 (`content_search` vs `content.search`).

### docs/risks/

39. **R1's "stale tampered row" defect is reported as open and has been fixed.**
    `R1-loopback-inference-capture.md` 173–208, 258, 322–326.
    `endpoint/capture-core/proxy/loopback/machine.go` 211–215 clears it and cites the harness.
    The doc's line citations are stale too.
40. **Q2 line citations.** `Q2-organisational-dimension.md` 33 cites `envelope.go:426` in a
    406-line file; 18, 23 and 62 are off by 14–20 lines.
41. **R1 harness.** `R1-harness/README.md` 9 offers `go test ./...`; the directory has no tests.
42. **`contracts/fixtures/`.** `docs/05` 244 cites it; it does not exist.

---

## 4. Component READMEs

### endpoint/

1. ✔ **`capture-core run` is documented and rejected.** `cmd/capture-core/README.md` 28;
   `main.go` 146–148 returns "unexpected argument".
2. **Process enumeration is wired.** `capture-core/README.md` 66–68 says none is.
   `enumerator.go` 34–42 wires a `tasklist` enumerator under `--proc-detect`.
3. **`--proc-detect` does not report degraded when tasklist works.** `detect/README.md` 57–58 vs
   `detect/provider.go` 243–283. Lines 43–44 and 54–57 of the same README also disagree.
4. **No stale-lock detection.** `capture-spool/README.md` 31–33 vs `lock.go` 11–14 and
   `EVIDENCE.md` 92.
5. **The self test does not assert startup order.** `cmd/capture-core/README.md` 138 vs
   `selftest.go` 131.
6. **"Six acks, `confidence: medium`" (LIKELY for the count).** 112–114 vs `selftest.go`
   374–381.
7. **The `policy` package does contain a signer.** `policy/README.md` 55–56 vs `verify.go` 252.
8. **`Open` does not always fail on a corrupt frame.** `capture-spool/README.md` 57; the agent
   opens with `CorruptQuarantine`.
9. **`--dry-run` does not print and exit.** `cmd/capture-core/README.md` 88 vs `run.go` 30–35.
10. **Test counts.** `capture-spool/EVIDENCE.md` 49 (37), `README.md` 62–63 (33, "now 34"); the
    tree has 34. Lines 133 and 269 miscount the killed-process tests (five).
11. **Native budget "105 ms".** `classifier-host/README.md` 51 and `MEASUREMENTS.md` 68;
    `classify/pipeline.go` 43–44 sums to 95 ms.
12. **Measured figures differ from `reports/`.** Six figures in `classifier-host/README.md` and
    `MEASUREMENTS.md` (wasm size, instantiate, module run, three latency values) no longer match
    the committed report files that the same docs call authoritative. The report files have
    uncommitted changes.
13. **`TOOLCHAIN-DECISION.md` predates ADR 0016.** 22–23, 43–45 say the docs choose Rust and an
    ADR must be raised; both happened.
14. **Minor.** GOCACHE prefix differs between READMEs; `selftest.go` 107–112 points at a README
    command no README contains; `cmd/capture-core/README.md` 142–146 on stand-ins and ordering.

### extension/ and contracts/

15. **README "decision 8" is stale.** `extension/README.md` 363–368 says the protocol has no
    member for "cannot enforce". `endpoint/protocol/envelope.go` 196 has
    `enforcement_unavailable` and the extension uses it.
16. **Generated-contract ambiguity note.** `contracts/generated/README.md` 61, 100–105 say
    `model_detection` forbids only `window_start`. The schema (326–329) forbids all four.
17. **"All three run the same suite."** `extension/README.md` 20, 101. `run-tests.mjs` omits
    `test/native-host.test.mjs` (7 tests).
18. **"A bundle can override" weights and threshold.** 334–335. Not wired; `SIGNAL_WEIGHT` is
    frozen.
19. **"Nothing else calls chrome.*".** 48–49. `content/content-boot.js` does, and the grep test
    does not look at it.
20. **`chrome.*` surface and storage.** 107–120 and `PERMISSIONS.md` 24. `chrome-adapter.js`
    189–199 and 222 call `chrome.storage.session` and `chrome.permissions.contains`.
21. **PERMISSIONS.md.** 8–9 (the test does not read PERMISSIONS.md); 8, 20 (`scripting` has no
    call site); 55–57 (`web_accessible_resources` includes an entry point; the test asserts
    inclusion, not equality); 20, 65 (the drop listener is installed after an async import).
22. **Does the extension produce envelopes?** `contracts/README.md` 45 and `PERMISSIONS.md` 39–40
    say yes; `extension/README.md` 89–90 and the code say no.
23. **Minor.** `extension/README.md` 152 (`tools/accept.mjs` is the repo-root one), 65–66.

### ingestion/ and vault/

24. **Driver availability contradiction.** `vault/README.md` 40 and
    `vault/content-vault/README.md` 8, 136 say no PostgreSQL driver can be obtained.
    `ingestion/ingest-api/go.mod` 10 requires pgx v5.11.0 with a working tagged build.
    `localdev/README.md` 231–243 ("F4") is stale for ingest-api for the same reason.
25. **Wrong ADR cited.** `vault/content-vault/README.md` 8 cites ADR 0016 for the driver gap; that
    ADR is about the classifier host. Line 131 cites `TOOLCHAIN-DECISION.md §3` for the wrong
    section.
26. **Key interface width.** `vault/README.md` 28 says five methods; the other two docs and the
    code say six.
27. **Non-POST is 405, not 400.** `ingestion/ingest-api/README.md` 20 vs `server.go` 76–78.
28. **No `--blob-ciphertext-endpoint` flag in ingest-api.** `cmd/ingest-api/README.md` 44. "Every
    setting is settable by a flag and by an environment variable" (`ingest-api/README.md` 91,
    `localdev/README.md` 158) is false in both directions.
29. **Named test file does not exist.** `ingest-api/README.md` 53 (`sql_integration_test.go`).
30. **Invariant test does not test what the README says.** `vaultinvariants/README.md` 21 vs
    `invariants_test.go` 344–364.
31. **NFC "gap tracked at `endpoint/canon/`".** `ingest-api/README.md` 142–144. `endpoint/canon/`
    is a finished implementation, not a tracker.
32. **Minor.** Package count (`content-vault/README.md` 120: six, not five); finalise route (94);
    `SAC_APPINSIGHTS` is not validated by the vault (`cmd/content-vault/README.md` 61–62); "Known
    state" heading (`ingestion/README.md` 41); two vs three inert settings; `SQL` vs `SQLStore`;
    references to CI where no CI configuration is wired (LIKELY).

### query/ and database/

33. **Per-class statement timeouts and `SET LOCAL` are not applied.** `DSL.md` 345, 347–348.
    `executePlan` (`src/plan.js` 345–395) never reads `statement_timeout_ms`.
34. **Q2 is not cursor-paged.** `DSL.md` 198, 398–401 vs `templates.js` 162 and `plan.js`
    505–512.
35. **Not every cursor failure is `cursor_expired`.** `DSL.md` 320–321; `plan.js` 255–267 returns
    `unsupported_query_shape` on the server-side path.
36. **Prohibited keys.** `DSL.md` 68–72 says ten; `errors.js` 180–191 has eleven (`group_by`).
37. **Q9 bypasses the shared pipeline.** `DSL.md` 19–20 vs `plan.js` 101–105.
38. **Invariants runner port.** `database/evidence/README.md` 46, 519 say 55432; the code and
    `database/tools/README.md` say 55434.
39. **Stale hash.** `database/evidence/README.md` 20 for `check-schema.mjs`.
40. **Counts.** Evidence README 438–442 (42/43 vs 47); 232, 445–447 (37 tables; the schema has
    38, RLS count LIKELY 32); 87 vs 307 (40 vs 36 tests); `dashboard/README.md` 41 (seventeen
    scenarios; there are 12).
41. **Lab container list.** Evidence README 525–527 omits `query-api`.
42. **`node --test test/`.** `query-api/README.md` 90–91 says it does not work; `package.json`
    uses it and the dashboard README documents it.
43. **Error states are broader than documented.** `DSL.md` 277 and `query-api/README.md` 108:
    any unhandled error returns `audit_chain_broken`, and a pool refusal returns 500, not 429.
44. **Auto-coarsening never fires for templates (LIKELY).** `DSL.md` 343, 402–406.
45. **Minor.** "rather than bundled" (`query-api/README.md` 21–23); runner container is never
    removed (`database/tools/README.md` 36); evidence directory "untracked" (489); template path
    (`dashboard/README.md` 47); coverage window shape (`DSL.md` 223); template limit state (338).

---

## 5. Azure, tooling, lab and verification records

1. **Pipelines are GitHub Actions workflows that nothing runs.** `azure/pipelines/README.md` 3–4
   calls them vendor-neutral. They use `runs-on`, `actions/checkout` and `azure/login`, live
   outside `.github/workflows/`, and the repo has no `.github/` directory. Present-tense claims
   that checks run on pull requests and nightly are not true of the repo as laid out (LIKELY:
   external wiring could not be checked).
2. **`docs/lab/LAB-COST.md` present-tense claims.** 388–390, 611 (no way to omit Front Door; the
   `deploy*` switches exist); 618 (lab parameter file "not created"; it exists and differs from
   the sketch); 90, 603 (no Dockerfiles; three exist); 97–105, 604–605 (services read no
   environment; they do); 141 (Azurite in Part A; the lab has none).
3. **LAB-COST §6 is cited for what it does not say.** `docs/lab/README.md` 21–23,
   `localdev/README.md` 105, `localdev/docker-compose.yml` 28. §6 marks blob storage testable
   and never mentions a certificate authority.
4. **`.integration/LEAD-VERIFICATION.md` 577–621 contradicts itself.** "No capture-core binary",
   "Chromium is not installed", "`database/tools` has no suite", "the NFC normaliser does not
   exist" all conflict with earlier sections and the tree. The header still says round 2.
5. **`.integration/REPORT.md`.** "does not exist" for `query/dashboard`, `vault/content-vault`
   and `azure/` (130, 220, 227, 318, 369, 428, 444, 456); three "open" variances are fixed in
   `tools/`; line citations are stale.
6. **`azure/README.md` contradicts itself.** 25 says checks run with `node --test azure/tools/`;
   92–94 says that form fails. `cost-model.md` 6 repeats the first. 108 omits the lab parameter
   file; 3 says one composition per environment.
7. **`azure/COST-FINDING.md` 91–92.** Says every cost figure is marked "under review"; none is
   outside LAB-COST. Lines 86–93 contain corrupted characters.
8. **`.cockpit/schema/README.md` 19–24.** Links to `template.json` and `example.json`, which do
   not exist.
9. **`.cockpit/README.md`.** 210–213 (no `design-tooling` component); 147, 189 (install path
   under `Downloads`); 45–51 vs 275 vs 292 (two, four or three actions; the code has three);
   73–74 (manifest fallbacks incomplete and inconsistent with the schema README).
10. **`localdev/README.md`.** 195 ("both services", "both Dockerfiles"; three); 147 ("two
    commands"; three); 246–249 (probe scheme is now a parameter, default unchanged, LIKELY);
    80–82 (path should be `localdev/tools/…`).
11. **Root `README.md` 75.** Says the lab README covers CPU architecture; it does not.
12. **`azure/modules/README.md` 40–42 (LIKELY).** Describes comments the module does not contain.

---

## 6. Code and configuration problems noticed along the way

These are not documentation, and none was tested. They are listed because the review found them.

1. ✔ **`.gitignore` still uses old paths.** Line 35 `!device/classifier-host/wasm/*.wasm` and
   line 47 `db/evidence/*.log`. Neither matches `endpoint/` or `database/evidence/` now, so the
   wasm exception and the evidence-log ignore have no effect.
2. ✔ **Dashboard parity tests skip.** `query/dashboard/test/parity.test.mjs` 19–21 imports from
   `services/query-api/…`, which does not exist, and each test skips when the import is missing.
3. ✔ **ingest-api holds a Key Vault role that can unwrap** (section 2, item 1).
4. **PostgreSQL subnet (LIKELY).** `azure/main.bicep` 297 passes the Container Apps subnet as the
   PostgreSQL delegated subnet. That subnet is already delegated to `Microsoft.App/environments`,
   and a subnet takes one delegation, so this probably cannot deploy.
5. **ingest-api may refuse to start in Azure (LIKELY).** It validates `SAC_APPINSIGHTS` as a URL
   (`main.go` 122); `main.bicep` 384 passes a connection string.
6. **content-vault `build` stage (LIKELY).** The Dockerfile does not copy `endpoint/protocol/`,
   which `go.mod` replaces `device/protocol` with.
7. **`schema-sql` output is stale.** `cmd/content-vault/main.go` 286, 292 says the retrieval-grant
   table does not exist; `schema.sql` 577 has it.
8. **Old paths in code comments and strings.** `ingestion/ingest-api/cmd/ingest-api/config.go`,
   `probes.go`, `infra_agreement_test.go`; `vault/content-vault/cmd/content-vault/config.go`,
   `internal/store/sql.go`, `tools/live-schema-check.ps1`; `query/query-api/src/registry.js`;
   `database/tools/check-schema.mjs`; `extension/src/messages.js`, `contract.test.mjs`;
   `azure/main.bicep` 624–625, `azure/inventory.json`, `azure/tools/index.mjs`;
   `tools/update-cockpit-tasks.mjs` 52; `endpoint/integration/native_seam_test.go` 25.
9. **Already known.** `localdev/tools/check-config-agreement.mjs` fails on `SAC_MAX_CONNECTIONS`.

---

## 7. Checked and found accurate

- Root README: eight gates, seventeen components with fifteen built, every declared path.
- `localdev/README.md` and `QUICKSTART.md` against today's lab: five containers, the `scorecard`
  network, default ports and `LAB_*` overrides, 18 smoke checks, the `pi` harness and gateway.
- Azure counts: 38 infra tests, 18 inventory rows, 17 modules, 7 identities, 18 alerts.
- Schema: role names, RLS enable and force on tenant tables, the audit hash chain, suspension
  columns, content-search constraints, 47 assertions, four schemas, seven roles.
- `query/query-api/DSL.md` §2: all 12 sources and their dimensions and measures, operators, the
  ten templates, and the numeric caps.
- Endpoint READMEs: every named test exists; flags and defaults, message types, startup and
  shutdown order, classifier validators and limits.
- Extension: permissions list, pinned key and id, golden frames, constants.
- ADR 0018 and ADR 0019 against the contract and Bicep; all ADR and document links other than
  those listed above.
