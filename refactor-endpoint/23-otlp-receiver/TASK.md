# 23. Local OTLP receiver

## Problem

Claude Code, Codex, Copilot and Claude Cowork can export their own telemetry, including prompts,
over OpenTelemetry (OTLP). There is nothing on the device to receive it. Pointing tools straight
at the cloud would put credentials in user-readable config, and would send prompt text off the
device before classification.

## Goal

capture-core runs an OTLP receiver on loopback, as `DESIGN.md` §8 describes. It accepts logs,
traces and metrics over OTLP/HTTP (protobuf and JSON) and OTLP/gRPC, rejects any request without
the per-device token, and hands logs and traces to a normalizer chosen by `service.name`. An
admin switches it on and off from the dashboard.

## Scope

- **Dependencies** (`DESIGN.md` §11), added to `device/capture-core/go.mod`:
  `go.opentelemetry.io/proto/otlp`, `google.golang.org/protobuf` and `google.golang.org/grpc`; and
  for tests only, `go.opentelemetry.io/otel/sdk` with the OTLP log, trace and metric exporters.
- **`device/capture-core/otlp`**, a `core.Provider`:
  - collector `otel_receiver` (new `protocol.Collector` constant and `ref.collector` row,
    component `capture_core`, modes `m0`–`m3`,
    in `schema.sql` and the next free numbered migration), route `tool.otel`;
  - `core.Toggled` on `endpoint.otel.enabled`.
  - **Listeners**:
    - HTTP on `endpoint.otel.http_listen` and gRPC on `endpoint.otel.grpc_listen` (§5 defaults
      `127.0.0.1:47318` and `127.0.0.1:47317`);
    - refuse to bind a non-loopback address even if a bundle got past validation;
    - an address change in `ApplyPolicy` rebinds both;
    - `Start` is transactional, through `core.ResourceStack`: if either port is held, both are
      released and the start fails with `port_held_by_other` (existing detail).
  - **HTTP** (`net/http`):
    - `POST /v1/logs`, `/v1/traces` and `/v1/metrics`;
    - `Content-Type` `application/x-protobuf` or `application/json`, decoded with
      `proto.Unmarshal` / `protojson.Unmarshal` into the `collector/{logs,traces,metrics}/v1`
      request types;
    - `gzip` `Content-Encoding` accepted;
    - bodies capped at 4 MiB (413 above that);
    - responses are the OTLP success messages in the request's encoding.
  - **gRPC**: the `LogsService`, `TraceService` and `MetricsService` `Export` methods. The token
    is checked from the `authorization` metadata in a unary interceptor.
  - **Token**:
    - 32 random bytes, hex, created once as `otlp.token` in the state directory (with the state
      directory's protection);
    - checked as `Authorization: Bearer <token>` with `subtle.ConstantTimeCompare`;
    - a missing or wrong token gets HTTP 401 / gRPC `Unauthenticated` before the body is read.
    - Expose `Token() string` for the config writers (tasks 27, 29, 31).
  - **Routing**:
    - Metrics are acknowledged and discarded; count `skipped_not_generative` per request.
    - Each log record or span is routed by its resource's `service.name` to a registered
      `Normalizer` interface (`Name() string`, `Accepts(serviceName string) bool`,
      `Logs(ctx, Sender, *logspb.ResourceLogs)`, `Spans(ctx, Sender, *tracepb.ResourceSpans)`).
      `Sender` is the connection's attribution (task 24 fills it; here it carries only the
      remote address).
    - With no normalizer registered, records are counted `observed` and dropped. Normalizers
      arrive in tasks 26, 28, 30 and 33.
  - **Privacy**: never log a request body, an attribute value or a decoding error that quotes the
    body. Log the endpoint, size and outcome only.
  - **Health**: `healthy` when both listeners are up; `degraded` with `port_held_by_other` if one
    is held; `absent` when disabled (task 06).
- Wire it into `buildProviders`.
- **Tests**:
  - The official Go SDK exporters (`otlploghttp`, `otlptracehttp`, `otlpmetrichttp`,
    `otlploggrpc`, `otlptracegrpc`, `otlpmetricgrpc`), configured with the receiver's address and
    `Authorization` header, each export one batch successfully.
  - A hand-built `protojson` body over HTTP JSON is accepted.
  - No token or a wrong token gets 401 / `Unauthenticated`, and the test's fake normalizer sees
    nothing.
  - A 5 MiB body gets 413.
  - A held port fails `Start` with both listeners released.

## Done when

- `cd device/capture-core && go test -race ./otlp/` passes.
- Ready to merge. After merge and deploy (`AGENTS.md`):
  1. On the PC, download the official `otelcol-contrib` Windows binary and `telemetrygen` from the
     same release, and record the version in `DECISIONS.md`.
     - These are test-only helpers. Copy them into `C:\ProgramData\SacTestbed\otel\` with
       `invm.ps1 -CopyTo`, never into the product's folders.
  2. Read the device token with
     `invm.ps1 -Command 'Get-Content C:\ProgramData\ShadowAICapture\state\otlp.token'`.
     Don't print it in the report.
  3. Write a collector config whose `otlphttp` exporter targets `http://127.0.0.1:47318` with the
     token in `headers`, and copy it in.
  4. Run the collector and `telemetrygen logs` as the console user with
     `invm.ps1 -AsUser console -Command`.
  5. Show the export succeeding, and a wrong token failing with 401, from the collector's output.
  6. Remove `C:\ProgramData\SacTestbed\otel\` afterwards.
- `node tools/accept.mjs` passes.
