# tools — the live-schema harness

One script, `live-schema-check.ps1`, and one purpose: produce the PostgreSQL evidence that `go test`
cannot produce on this host.

The Go integration test that runs the vault's SQL statements against a real server
(`internal/store/sql_integration_test.go`) skips here with an exact reason — the file sandbox denies a
child process access to the Docker named pipe
(`npipe:////./pipe/dockerDesktopLinuxEngine`) — and runs on a machine without that restriction. This
harness produces the same evidence from a shell that *can* reach the pipe, and it is not a paraphrase:
it runs `content-vault schema-sql` to emit the statement text the service itself issues, then executes
exactly that.

```powershell
pwsh -File tools/live-schema-check.ps1 [-Container shadowpg-invariants] [-Out evidence/live-schema.log]
```

Checks, each inside a transaction that ends in `ROLLBACK`, so nothing it does is left behind:

1. The server is the one the schema was applied to (`ops.content_object` exists).
2. Every statement the service issues PREPAREs and DEALLOCATEs against the live schema.
3. ADR 0014's mutually exclusive pair is refused by the database, and for the named constraint
   (`tenant_full_text_search_requires_vendor_readable_content`).
4. A `full_text` tier below M3 is refused, for the named constraint.
5. The content-object lifecycle — insert, guarded re-wrap, shred — executes and the row reads back as
   `shredded/erasure/<version>` with the wrapped key replaced.
6. `ops.retrieval_grant` put and claim, a second guarded claim matching zero rows, and the
   `retrieval_grant_single_use` trigger refusing an unguarded UPDATE of a redeemed grant.

It also needs `go` on the path, with `GOCACHE` under the repository and `GOPROXY=off`, because it builds
`./cmd/content-vault` to generate the SQL. The log lands in `evidence/live-schema.log` by default; note
that `*.log` is git-ignored, so a fresh clone carries the harness and the generated `_schema.sql`, not
the run's output. **The harness proves the SQL text; it does not prove the `database/sql` plumbing
around it** — that caveat is stated in [../README.md](../README.md#not-verified-and-why).
