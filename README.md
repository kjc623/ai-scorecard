# Shadow AI Capture

Records what employees send to generative AI tools from company-managed devices, and makes it
queryable.

The customers are 500–5,000-employee companies that have *not* bought enterprise AI, so there is no
vendor-side API to pull from: if the system does not observe the usage on the device, the data does
not exist anywhere.

**The property the whole design turns on.** Content is interpreted where it is observed and **does not
leave the machine**: classification, a digest and dimensions cross the network, and content crosses
only on an explicit per-event grant from the backend. A tenant can trade that property away for
content search — which requires the vendor to be able to read the content — and the trade is made per
tenant and recorded in the database rather than assumed
([ADR 0014](docs/adr/0014-content-search-is-a-per-tenant-capability.md)). That is not a privacy
feature bolted onto a monitoring system: it is what keeps the breach surface small enough to sell, and
it constrains almost every other decision here.

## Where things are

| Directory | What lives there |
|---|---|
| [`extension/`](extension/) | The browser extension: Chromium MV3, wide observation, narrow emission, inline warn/block |
| [`endpoint/`](endpoint/) | The desktop agent: the capture core, its encrypted spool, the classifier host, the device protocol package, and the NFC canonicaliser |
| [`ingestion/`](ingestion/) | The API devices POST events to. The one write path |
| [`control/`](control/) | The control plane: device enrolment, the DPoP token endpoint and the per-event content grant (`control-api`) |
| [`vault/`](vault/) | The content vault — the only component that can unwrap content keys |
| [`query/`](query/) | The read path: the closed query DSL and the dashboard the ten questions are answered in |
| [`database/`](database/) | The PostgreSQL schema, its assertions, and the tools that check both |
| [`azure/`](azure/) | The deployment as code: Bicep, parameter files, pipelines |
| [`localdev/`](localdev/) | Runs the whole stack locally, in containers, for nothing |
| [`contracts/`](contracts/) | The wire contract, and the generated TypeScript and Go types |
| [`tools/`](tools/) | The acceptance gates — one command that decides whether the repository still works |
| [`docs/`](docs/) | The design record: the subsystem documents and the decision records |

Every one of those has its own README explaining what is inside and how it works.

## What is built, and what is not

Every component but one has code. `control-api`'s enrolment, the DPoP token endpoint
([ADR 0020](docs/adr/0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md)),
the content grant with its finaliser, policy delivery and the health channel are built; `aggregator`
(the scheduled job that keeps the dashboard's aggregates fresh) now has code too. The one with none is
`reconciler` (the second expiry mechanism and drift detector). The Azure transport is written but not deployed:
[`azure/modules/application-gateway.bicep`](azure/modules/application-gateway.bicep) is the device
ingress, and no subscription has run it. The device-to-cloud path is built and proven locally:
`capture-core --service` enrols (`x509` or `dpop`), spools, and drains batches to `POST /v1/events`,
and the opt-in auth lab (`node localdev/run.mjs --auth`) drives that through an Application Gateway
stand-in into the real schema. The development installer ([`installer/`](installer/)) builds the
Windows MSI, macOS PKG and Linux package that carry it; the MSI installs a Windows service the binary
hosts itself (`--service`). Still absent for a real deployment: the MDM-delivered per-tenant
enrolment profile, a signed policy bundle, the trust/proxy configuration, and a signed artefact. For
the local lab, `installer/lab-msi.mjs` stands in for the MDM and builds an MSI that carries all of
those, unsigned. The current pre-prod blockers and the ordered go-live sequence are tracked in
[`azure/RUNBOOK.md`](azure/RUNBOOK.md).

The content path is built and proven in that lab, on a Windows host: at M3 the agent holds a prompt
locally, asks `control-api` for a per-event grant, and uploads the sealed content; `content-vault`
records it; and an analyst finds it by its text and retrieves it on the dashboard's Explore page,
through `query-api` and the vault's case-reference and second-approver check. What stands in for the
real thing there: the storage account (a lab service and a signed URL instead of Blob storage and a
SAS), the session (a development principal instead of a signed-in analyst), and the second approval
(a name the requester types, which the vault only requires to be someone else).

The browser half is verified in a real browser: Edge loads the extension, its listener observes real
requests, M0 carries no content, a registered native host produces a genuinely connected channel, and
a real user-selected file is read, chunked and acknowledged by `capture-core`. Inline blocking is the
one path that is **not observable** here, because an unpacked extension cannot hold the permission it
needs — it requires a policy install, which requires elevation.

The authoritative per-component status is in [`.cockpit/project.json`](.cockpit/project.json), and
`docs/00-architecture.md` §7 lists what is still unknown with an owner and a way to close each item.

## How to check it

One command decides whether the repository still works:

```
node tools/accept.mjs
```

It runs eight gates — every package suite, the contract against its generated output, the cross-component
seam and vocabulary checks, the six architecture invariants, the endpoint binary as a process, the
browser half in a real Chromium, and the database's assertions against a real server — and exits
non-zero if any of them fails. A gate that cannot run is reported `SKIPPED` with its reason, and a
skipped gate is **not** a pass. [`tools/README.md`](tools/README.md) explains what each gate decides.

To run the whole system locally, including a real PostgreSQL with the schema applied:

```
node localdev/build.mjs     # build the images
node localdev/run.mjs       # up, smoke test, report
```

[`localdev/README.md`](localdev/README.md) covers the addresses and ports, the build constraints, and
the one credential the lab uses. It also covers the opt-in device-auth lab, which is the one a real
endpoint agent enrols against and the one that serves the dashboard on live data.

## The design record

The design came first and is still the place to start for *why*:

| If you want to know… | Read |
|---|---|
| The whole design, the alternatives and why this one won | [`docs/00-architecture.md`](docs/00-architecture.md) |
| How nine usage modes get captured on three operating systems | [`docs/01-collectors.md`](docs/01-collectors.md) |
| How data reaches the cloud, and the normative dedup specification | [`docs/02-ingest-and-transport.md`](docs/02-ingest-and-transport.md) |
| The data model, retention, erasure and recovery | [`docs/03-data-platform.md`](docs/03-data-platform.md) |
| How the ten questions become queries | [`docs/04-dashboard-and-query.md`](docs/04-dashboard-and-query.md) |
| How it deploys, updates and stays running | [`docs/05-platform-delivery.md`](docs/05-platform-delivery.md) |
| The trust model, the key hierarchy and residual risk | [`docs/06-security-and-threat-model.md`](docs/06-security-and-threat-model.md) |
| Why a specific choice was made | [`docs/adr/`](docs/adr/) — the index is its README |
| What was measured rather than assumed | [`docs/risks/`](docs/risks/) |

## Seven properties hold the design together

1. **Content stays put by default.** The only content-egress path is a per-event grant, so "upload
   everything" is structurally unreachable rather than merely forbidden
   ([ADR 0008](docs/adr/0008-no-server-side-content-search-in-any-key-mode.md)).
2. **One write path.** Collectors hold no database credential; `ingest.record_event()` owns
   idempotency, the dedup tie-break and retention
   ([ADR 0001](docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md)).
3. **One read path.** The browser never speaks SQL; a closed DSL compiles to parameterised queries
   ([ADR 0003](docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md)).
4. **Observations are immutable.** History cannot be rewritten, by anyone short of a superuser
   ([ADR 0004](docs/adr/0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md)).
5. **Isolation is structural.** Forced row-level security, tenant-leading keys, and per-tenant content
   keys as an independent second layer.
6. **Content search is a per-tenant capability.** `disabled`, `attachment_names` or `full_text`; the
   last requires vendor-readable content, and the mutually exclusive pair — customer-held keys with
   server-side content search — is a database check constraint rather than a policy note
   ([ADR 0014](docs/adr/0014-content-search-is-a-per-tenant-capability.md)).
7. **Coverage is honest.** Every collection path reports its own health; a path that is not working
   appears as `degraded` or `absent`, never as an absence of data.

## Traceability, and what this package is not

Every constraint in [`docs/00-architecture.md` §2](docs/00-architecture.md) cites the brief section it
comes from. Where a decision rests on something the brief does not say, it is labelled **ASSUMPTION**
and listed as an open question with an owner and a way to close it.

- **This is not a legal position.** The brief explicitly excludes legal, privacy-policy and
  jurisdictional review, and [`docs/06-security-and-threat-model.md`](docs/06-security-and-threat-model.md)
  §11 states only what the system can *attest*, not what the law requires.
- **All cost figures are estimates** at Azure East US list price, pay-as-you-go, as of 2 October 2026,
  and must be re-baselined before any commercial commitment.
- **Every ADR is `proposed`.** None has been reviewed by anyone outside this package, and none has a
  second author.
