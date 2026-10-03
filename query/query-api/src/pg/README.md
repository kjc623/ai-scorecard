# `src/pg/` — the PostgreSQL wire client

A complete PostgreSQL client in plain Node, with no dependencies. It is the only database driver in
this repository that is neither `psql` nor a build-tagged Go driver, and that is a rule rather
than a preference: **there is no external dependency anywhere in this tree**, so `pg` is not used
and the service reads the database by speaking the protocol itself.

| File | Responsibility |
|---|---|
| `client.js` | The frozen seam: `createClient()`, and the connection state machine |
| `protocol.js` | Message framing — the startup packet, `Parse`/`Bind`/`Describe`/`Execute`/`Sync`, `DataRow`, `RowDescription`, `ErrorResponse` |
| `scram.js` | SASL/SCRAM-SHA-256, the authentication a modern PostgreSQL asks for |
| `types.js` | Decoding column values into JavaScript |
| `error.js` | A typed error carrying the SQLSTATE, so the caller can tell "the database refused this" from "this service is broken" |

## The interface the rest of the package depends on

```
createClient({ host, port, database, user, password, ssl, statementTimeoutMs, applicationName })
  await connect()          // rejects; the database never says maybe
  await query(text, params)  // -> { rows, rowCount, fields }
  await begin() / commit() / rollback()
  await close()
  ready                    // false from the first byte of a query
```

`params` are sent as BIND parameters. Values are never interpolated into SQL text — the whole point
of the read path is that a value cannot change what a statement means.

## Three decisions a caller can observe

**One connection, one statement at a time.** A second concurrent `query()` rejects with `SAC_BUSY`
rather than queueing. A queue would hide a "one client per request" bug behind mysterious response
ordering, and `ready` already answers the question a queue would paper over. This is why
`src/http/pool.js` exists: concurrency comes from having several connections, not from sharing one.

**The transport is required and is the caller's decision.** No option means no connection. An
unencrypted session must be asked for by name (`sslMode: 'disable'`), and `ssl: true` means
verify-full. There is no `rejectUnauthorized: false` path anywhere in this directory — not even
behind a flag — because a mode that encrypts without authenticating the server is the one failure
that cannot be noticed later.

**`close()` is terminal and never rejects.** A teardown that can fail is a teardown nobody calls.

## Verification

`test/pg-client.test.mjs` splits in two, and the split is the point.

**Without a server** it checks the parts that can be checked in isolation: message framing and
reassembly across chunk boundaries, the exact bytes of the startup and `SSLRequest` packets, the
`Parse`/`Bind` layout (a hostile value appears in `Bind` and **not** in `Parse`), `ErrorResponse` to
SQLSTATE, the RFC 7677 SCRAM vector byte for byte, replayed-nonce and tampered-verifier refusals,
every decoder, and the config refusals. A scripted protocol double over real sockets covers the
socket path, including MD5 and a `prefer` to `'N'` fallback.

**With a server** it runs against a real PostgreSQL: bound parameters carrying a quote and a
semicolon as *data*, uuid, an `int8` past the precision of a double, `numeric`, `bytea`, `json`,
arrays, timestamps, `BEGIN`/`INSERT`/`ROLLBACK` visibility, a duplicate key surfacing as `23505`, and
`statement_timeout` arriving as `57014`.

A test that cannot run **skips with its reason** — never a silent pass. There are two distinct
reasons and the skip says which: no container at all, or a container whose published port cannot be
reached.
