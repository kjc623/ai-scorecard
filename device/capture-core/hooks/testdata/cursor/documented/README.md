# Cursor hook input, as documented

These files are **written from the documentation, not captured** from an installed Cursor. The
Cursor hooks page (`https://cursor.com/docs/agent/hooks`) could not be read from the build machine,
so they follow what third-party hook packages on npm, read 2026-10-08, give as Cursor's stdin JSON
for `beforeSubmitPrompt` and `beforeMCPExecution`: the common fields (`conversation_id`,
`generation_id`, `model`, `hook_event_name`, `cursor_version`, `workspace_roots`, `user_email`,
`transcript_path`), `prompt` and `attachments`, and `tool_name`, `tool_input` and the server's
`command` or `url`, with Windows paths. The packages disagree on `tool_input`: the newer ones
read it as a string holding the JSON (`before-mcp-execution.json`), older typings as an object
(`before-mcp-execution-object.json`). The version, model and ids are fixture values.

The device phase replaces this folder with input captured from the version installed on the
reference VM, in a folder named after that version.
