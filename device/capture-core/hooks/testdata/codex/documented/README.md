# Codex hook input, as documented

These files are **written from the documentation, not captured** from an installed Codex CLI. The
Codex hooks page (`https://developers.openai.com/codex/hooks`) could not be read from the build
machine, so they follow the JSON Schema the `openai/codex` repository publishes for the hook's
stdin (`codex-rs/hooks/schema/generated/user-prompt-submit.command.input.schema.json`, `main` at
`99aa053`, read 2026-10-08): `session_id`, `turn_id`, `transcript_path` (a path or `null`), `cwd`,
`hook_event_name`, `model`, `permission_mode` and `prompt`, plus `agent_id` and `agent_type` when
a subagent submits the prompt, with Windows paths. The ids, model and paths are fixture values.

The device phase replaces this folder with input captured from the version installed on the
reference VM, in a folder named after that version.
