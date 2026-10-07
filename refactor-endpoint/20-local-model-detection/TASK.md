# 20. Local model detection

Needs: on the reference VM, Ollama for Windows installed for the console user and running in their
session, with two models pulled
(for example `ollama pull llama3.2:1b` and `ollama pull qwen2.5:0.5b`). Optionally LM Studio
with one downloaded model.

## Problem

Models that run locally never touch the network, so connection monitoring can't see them. The
product needs to know:
- that a runtime such as Ollama or LM Studio is present and serving;
- which models are downloaded on the device.

## Goal

The inventory provider (task 16) detects local model runtimes by their running process and
listening port, lists the models in their on-disk stores, and emits one `local_model` discovery
record per runtime and user with the model names.

## Scope

- **`device/capture-core/inventory/models.go`** (parsing) plus `models_windows.go` (locations), a
  scanner added to the inventory provider's list:
  - **Runtime presence**: a catalog `local_runtime` app counts as present for a user when any of
    these holds:
    - a process matching its `windows_exe` runs as that user (`hostinfo.ProcessInfo`, task 09);
    - a TCP listener on one of its `listen_port` values belongs to such a process (from
      `GetExtendedTcpTable` listening rows, using the task 09 helpers; add
      `hostinfo.ListenersOn(port int) ([]uint32, error)` if task 09 didn't);
    - its model store exists in that user's profile.

    The basis is `port_listen` when a listener was seen, else `model_store`.
  - **Model names from disk**. Read files only; never call the runtime's HTTP API:
    - **Ollama**: expand `model_store` (`%USERPROFILE%\.ollama\models`, or `OLLAMA_MODELS` from
      the user's environment in `HKU\<SID>\Environment`). Read
      `manifests\registry.ollama.ai\<namespace>\<model>\<tag>` and derive `model:tag`, with the
      `library/` namespace shown without the prefix.
    - **LM Studio**: expand its `model_store`. Verify the current default location
      (`%USERPROFILE%\.lmstudio\models` or `%USERPROFILE%\.cache\lm-studio\models`) and record it
      in `DECISIONS.md`. List `<publisher>\<model>` directories that contain a `.gguf` or MLX
      weight file.
  - At most 64 names per record (the contract cap), sorted. A longer list is truncated and counts
    one `dropped`.
  - **Emit** `local_model` with `app_key`, the runtime version (PE version resource of the
    runtime's exe, as in task 17), `model_names`, and the user's person.
  - The §6 key has no model list in it, so a newly pulled model on the same day isn't
    re-emitted until the next day. That matches §6; note it in the report.
- **Tests**:
  - fixture Ollama and LM Studio stores (namespaced and `library` models, a tag-less manifest
    directory that is ignored, an empty store);
  - the presence rules with fake process and listener seams.

## Done when

- `cd device/capture-core && go test -race ./inventory/` passes.
- Ready to merge. After merge and deploy (`AGENTS.md`), with Ollama running and two models pulled:
  - A `discovery` / `local_model` record for `app:ollama` is emitted, with both model names and
    the console user's `user_ref`. Show it from the spool log (the agent's `envelope spooled` lines, task 05: `invm.ps1 -Command 'Select-String "envelope spooled" C:\ProgramData\ShadowAICapture\state\capture-core.log | Select -Last 50'`), with the batch acknowledged in `invm.ps1 -AgentState`.
  - Show `invm.ps1 -AsUser console -Command 'ollama list'` beside it.
- `node tools/accept.mjs` passes.
