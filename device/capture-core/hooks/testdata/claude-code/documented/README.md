# Claude Code hook input, as documented

These files are **written from the documentation, not captured** from an installed Claude Code.
They follow the stdin JSON the Claude Code hooks reference (`https://code.claude.com/docs/en/hooks`,
read 2026-10-08, describing versions up to 2.1.295) gives for `UserPromptSubmit` and `PreToolUse`:
the common input fields, `prompt`, and `tool_name`, `tool_input`, `tool_use_id` and `mcp_server`,
with Windows paths.

The device phase replaces this folder with input captured from the version installed on the
reference VM, in a folder named after that version.
