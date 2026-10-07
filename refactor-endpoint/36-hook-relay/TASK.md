# 36. Hook relay

## Problem

Vendor hooks (Claude Code `UserPromptSubmit`, Cursor `beforeSubmitPrompt`) run a command before
the prompt is sent, and block it if the command says so. The command runs as the user, but the
policy cache, the classifier and the spool live in the LocalSystem service: the state directory
is not readable by users. Nothing answers a hook today.

## Goal

The hook command is `capture-core.exe --hook <tool> <event>`. It relays the hook's input to the
service over the native endpoint, which decides allow, warn or block from the bundle in force
(`DESIGN.md` §9). The command prints the tool's decision and records the prompt on route
`tool.hook`. It never makes a network call, and it fails open.

## Scope

- **Protocol** (`device/protocol/native.go`): two new message types, added to `check-vocab` as
  device-only types.
  - `hook_evaluate` (hook → service):
    `{tool, event, session_id, prompt_text, tool_name?, cwd?, client_version?}`, with
    `prompt_text` at most 256 KiB. A larger prompt is sent as its length only, with
    `over_cap: true`.
  - `hook_decision` (service → hook): `{action: allow|warn|block, message, link, rule_id}`.
- **Hook mode** (`cmd/capture-core`, `--hook <tool> <event>`):
  1. Read stdin (at most 1 MiB) and pick the adapter for `<tool>`.
  2. The adapter turns the tool's JSON into `hook_evaluate`.
  3. Dial the native endpoint through `capture-core/localipc` (task 10), with the existing
     server-ownership check, and send.
  4. Wait for `hook_decision`, then print the adapter's rendering of it and exit 0.
  - The whole invocation has a hard 400 ms deadline from process start. On a timeout, a dial
    failure, a refusal, malformed input or a panic, it prints the adapter's allow output and
    exits 0.
  - It writes nothing to stderr unless the adapter's format requires it, and never logs prompt
    text.
- **Adapter interface** (`capture-core/hooks/adapter.go`):

  ```go
  type Adapter interface {
      Parse(event string, stdin []byte) (protocol.HookEvaluate, error)
      Render(event string, d protocol.HookDecision) (stdout []byte, exitCode int)
      Allow(event string) (stdout []byte, exitCode int)
  }
  ```

  The registry is keyed by tool key (`claude_code`, `cursor`, ...). This task ships only a
  `test` adapter used by the tests. The real adapters are tasks 37 and 38.
- **Service side** (`capture-core/hooks`, a `core.Provider`):
  - Collector `hook_relay`, route `tool.hook`.
  - It is `Toggled`, enabled when `endpoint.hooks.enabled`.
  - It registers a `hook_evaluate` handler on the native endpoint. While disabled, the handler
    answers `allow` at once.
  - For each request:
    1. Map the tool key to `app:<key>` and resolve the mode with the same `ScopeQuery` it passes
       to `Process`.
    2. At `m1`+, classify through `classifierlink` with `BudgetMS: 30`.
    3. Call `enforce.Evaluate` (task 12).
    4. Answer `hook_decision`.
    5. Then hand the observation to `core.Pipeline.Process`, with `Decision` from
       `enforce.RecordedAction(d, canEnforce=true)`, the peer's person (`peerPerson`), and the
       prompt text as the content reader.
  - The answer is written before the record is spooled. A spool failure counts `dropped`, never
    changes the answer.
  - At `m0` the text is not classified, and label rules don't match (§9).
  - A disabled tool (`endpoint.tools.<key>.hooks` false) is answered `allow`, and nothing is
    recorded.
- `ref.collector` row `hook_relay`, in `services/database/schema.sql` and in the next numbered
  migration in `services/database/migrations/`. Add `protocol.Collector` and the route constant if
  task 05 hasn't already.
- **Benchmark** (`capture-core/hooks/bench_windows_test.go`, build tag-free, skipped unless
  `SAC_HOOK_BENCH=1`). It runs on the reference VM, whose resources are the budget's baseline:
  1. Build `capture-core.exe`.
  2. Start a service in-process with a test bundle and a real classifier-host release.
  3. Run `capture-core.exe --hook test prompt` 1,000 times with a 4 KB prompt containing no
     secret, plus 100 runs with an AWS-key-shaped string.
  4. Report the p50, p95 and p99 of the whole process's wall time. The test fails when p99 is
     50 ms or more.

  The hook processes run as the invoking user, as real hooks do. The test needs no elevation, and
  its in-process service uses a temporary pipe name and state directory, not the installed
  service's.

## Done when

- `cd device/capture-core && go test -race ./hooks/ ./cmd/capture-core/` passes, including:
  - a fail-open test for each failure (no service, a slow service past 400 ms, malformed stdin,
    a refused frame);
  - a test that a `block` rule on `credential` blocks the AWS-key-shaped prompt and records
    `blocked`.
- On the reference VM (test binaries only, so no merge is needed for this check; the product
  install on the VM is untouched):
  1. On the PC, build the benchmark's binaries: `go test -c -o hooks.test.exe ./hooks/`,
     `capture-core.exe`, and `classifier-host.exe` with a test classifier release.
  2. Copy them into one folder in the VM with `invm.ps1 -CopyTo`.
  3. Run the benchmark as the console user:
     `invm.ps1 -AsUser console -Command '$env:SAC_HOOK_BENCH=1; <folder>\hooks.test.exe -test.run HookBench -test.v'`.
  4. It reports a p99 under 50 ms. Paste the numbers into the report and into `DECISIONS.md`,
     with the VM's vCPU count and memory.
- `node tools/accept.mjs` passes.
