# 14. App catalog

## Problem

The discovery collectors (tasks 16–21) match what they find on a device against a catalog of AI
apps:
- executable names and publishers;
- AppX and uninstall names;
- CLI binaries and npm packages;
- IDE extension ids;
- inference domains, listen ports and model stores.

The only catalog today is `ref.tool_catalogue` (`services/database/schema.sql`, around lines
127–133). It maps behaviour-derived fingerprints (`tls_…`, `tf1:…`, `proc_…`) to display names.
It has no app-level row and no signals a device can match, and nothing delivers it to devices.

## Goal

`ref.app` and `ref.app_signal` (`DESIGN.md` §4) hold 20 seed apps, each filled in from a verified
source. Each app has a matching `app:<app_key>` row in `ref.tool_catalogue`. The signed policy
bundle carries the catalog as `catalog` (§5), and the device decodes and validates it.

## Scope

- **Database** (`services/database/schema.sql`, and the same change, seed rows included, as the
  next free numbered migration):
  - Create `ref.app` and `ref.app_signal` exactly as in §4. Grant read to the roles that read
    `ref.tool_catalogue` today.
  - `ref.tool_catalogue`:
    - add the nullable `app_key` column referencing `ref.app`;
    - add `endpoint` to the `signal_kind` CHECK;
    - add one `('app:<app_key>', display_name, vendor, 'endpoint', '{}', app_key)` row per app.
  - Seed the 20 `app_key` values listed in §4. Verify every signal against the vendor's
    documentation or a real install, put the URL in `source_url`, and record in `DECISIONS.md`
    what was checked on which version. Expected signal kinds (add only what you verified; leave
    out a kind you couldn't verify, and say so):

    | app_key | category | expected signals |
    |---|---|---|
    | `claude_desktop` | `chat_assistant` | `windows_exe` `claude.exe`, `windows_uninstall_name`, `macos_bundle_id`, `publisher`, `inference_domain` `claude.ai` |
    | `chatgpt_desktop` | `chat_assistant` | `windows_appx` (package family), `windows_exe`, `macos_bundle_id`, `publisher`, `inference_domain` `chatgpt.com` |
    | `cursor` | `ide` | `windows_exe` `Cursor.exe`, `windows_uninstall_name`, `macos_bundle_id`, `publisher`, `inference_domain` |
    | `windsurf` | `ide` | `windows_exe`, `windows_uninstall_name`, `macos_bundle_id`, `publisher` |
    | `vscode` | `ide` | `windows_exe` `Code.exe`, `windows_uninstall_name`, `macos_bundle_id`, `publisher` |
    | `jetbrains` | `ide` | `publisher`, `windows_uninstall_name` (prefix) |
    | `claude_code` | `coding_agent` | `cli_binary` `claude`, `npm_package` `@anthropic-ai/claude-code`, `windows_exe` `claude.exe` (native installer), `inference_domain` `api.anthropic.com` |
    | `codex` | `coding_agent` | `cli_binary` `codex`, `npm_package` `@openai/codex`, `windows_exe` |
    | `gemini_cli` | `coding_agent` | `cli_binary` `gemini`, `npm_package` `@google/gemini-cli` |
    | `copilot_cli` | `coding_agent` | `cli_binary` `copilot`, `npm_package` `@github/copilot` |
    | `github_copilot` | `ide_assistant` | `ide_extension_id` `github.copilot`, `github.copilot-chat` |
    | `claude_code_vscode` | `ide_assistant` | `ide_extension_id` `anthropic.claude-code` |
    | `continue` | `ide_assistant` | `ide_extension_id` `continue.continue` |
    | `ollama` | `local_runtime` | `windows_exe` `ollama.exe`, `ollama app.exe`, `listen_port` `11434`, `model_store` `%USERPROFILE%\.ollama\models` |
    | `lm_studio` | `local_runtime` | `windows_exe` `LM Studio.exe`, `lms.exe`, `listen_port` `1234`, `model_store` |
    | `openai_api` | `inference_api` | `inference_domain` `api.openai.com` |
    | `anthropic_api` | `inference_api` | `inference_domain` `api.anthropic.com` |
    | `azure_ai_foundry` | `inference_api` | `inference_domain` `.openai.azure.com`, `.services.ai.azure.com`, `.models.ai.azure.com` |
    | `aws_bedrock` | `inference_api` | `inference_domain` `.bedrock-runtime.<region>.amazonaws.com` (leading-dot suffix form) |
    | `google_ai_api` | `inference_api` | `inference_domain` `generativelanguage.googleapis.com`, `.aiplatform.googleapis.com` |

  - `inference_domain` values follow the bundle's existing host-match rule (`policy/bundle.go`
    `hostMatches`): an exact host, or a leading dot for a suffix. `model_store` values may
    contain `%USERPROFILE%` or `~`, expanded per user by the collector.
  - `invariants.test.sql`: the CHECKs refuse a bad `app_key`, category, platform and kind, and
    every `ref.app` row has its `app:` row in `ref.tool_catalogue`.
- **control-api** (`internal/policyserve`):
  - `Bundle.Catalog []CatalogApp` with the JSON shape in §5:
    `{app_key, category, signals[{platform, kind, value}]}`.
  - Composed from both tables through a new statement on `store.PolicyInputs`, registered in
    `sql.go` `Statements`, and added to the `storetest` fake.
  - Sorted by `app_key`, then platform, kind and value, so the payload comparison in `current()`
    is stable.
- **Device** (`device/capture-core/policy/bundle.go`):
  - `CatalogApp`, `CatalogSignal` and `Bundle.Catalog`.
  - `Validate` refuses an unknown category, platform or kind, an empty value, and a duplicate
    `app_key`.
  - Add helpers on `*Bundle`, each returning the app key or keys:
    - `AppByExe(platform, base string)` (case-insensitive);
    - `AppByPublisher(platform, subject string)`;
    - `AppByDomain(host string)` (`hostMatches`);
    - `AppByExtensionID(id string)`;
    - `AppByCLI(name string)`;
    - `AppByNPM(pkg string)`;
    - `AppsByPort(port int)`;
    - `Category(appKey string)`.

    Collectors call these; no collector parses the catalog itself.
  - Task 12's evaluator resolves `match.categories` through `Category` from now on: remove the
    "never matches" placeholder and its comment, and add the test cases.
- **Dashboard**: the enforcement rule card's category picker (task 11) uses the categories present
  in the catalog. There is no new card.

## Done when

- `node services/database/tools/check-schema.mjs` and `node tools/accept.mjs --only database` pass.
- The drift test (task 01) passes with `catalog`.
- The migration proof (task 04) shows an empty diff.
- `cd device/capture-core && go test ./policy/ ./enforce/` passes, including the lookup helpers
  and a category rule that matches.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release: the bundle in force on the VM carries all
20 apps. `invm.ps1 -Command` decodes the cached bundle's payload from the agent's state directory
(`policy\`) and prints the `catalog` entry count and app keys. Quote them.
