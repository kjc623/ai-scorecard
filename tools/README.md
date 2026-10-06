# tools/ — the acceptance gate

```
node tools/accept.mjs            # every gate that can run here
node tools/accept.mjs --list     # the gates and what a failure means
node tools/accept.mjs --only go  # one gate (or --skip browser)
```

It exits non-zero if any gate fails. A gate that cannot run on this host (no Docker, no `az`, not
Windows, no Chromium) is reported `SKIPPED` with its reason; a skipped gate is not a pass. CI runs it
on Linux and Windows (`.github/workflows/ci.yml`).

| Gate | Runs |
|---|---|
| `go` | `go vet` and `go test` in every Go module |
| `node` | `npm test` in every Node package |
| `static` | The checks below, the contract drift check, the schema check and the infrastructure checks, with their tests |
| `bicep` | `az bicep build` of the template and both parameter files, warnings included |
| `database` | `services/database/tools/test-database.mjs`: the schema applied as a non-superuser on PostgreSQL 16, then its invariants |
| `installer` | `device/installer/verify.mjs` (Windows) |
| `browser` | `device/extension/tools/in-browser-check.mjs` in a real Chrome or Edge |

The cross-component checks in this directory compare one component against another, which no
component's own tests can do:

| Check | Decides |
|---|---|
| `check-config.mjs` | Every environment variable `azure/main.bicep` sets for an app or job is read by that component |
| `check-invariants.mjs` | Structural properties: devices hold no database credential, the dashboard holds no SQL, query-api binds values, the spool is append-only, only content-vault touches stored content, collector health stays closed |
| `check-vocab.mjs` | The extension and `device/protocol` spell every shared vocabulary the same way |
