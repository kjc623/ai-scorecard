# Project manifests

Every project the cockpit is pointed at describes itself with a **manifest**: a
JSON file at the project root declaring its components, its tasks, and the agents
that will build them.

The cockpit looks for these, in order:

| Path | Use |
| --- | --- |
| `.cockpit/project.json` | The normal choice. Keeps one folder of cockpit material inside the project. |
| `project.cockpit.json` | For a project that does not want a `.cockpit/` folder. |
| `cockpit.project.json` | The same, with the words the other way round. |
| `cockpit.json` | The shortest form, and last so a tool can describe itself without displacing the project's manifest. |

Without one the cockpit still renders the real workspace tree and says the
manifest is missing — it never invents an architecture.

- **The schema** is [`manifest-v1.schema.json`](manifest-v1.schema.json).
- **[`../project.json`](../project.json)** is a complete, real manifest — the Shadow AI
  Capture package that this plugin was built alongside — and the one to copy from
  when starting a new `.cockpit/project.json`. It shows the shape at a realistic
  size: 17 components across 7 layers, 7 tasks with dependencies, a roster of 6
  agents with the files each is handed, and six invariants.
- **[`cockpit.json`](../cockpit.json)** is this plugin's own manifest, describing
  the cockpit itself. It is the working example of the "tool describes itself"
  case, and the file the panel shows when you open it against its own folder.

## The join that matters

`agents[].id` must match the DSH teammate name exactly. That is how the cockpit
knows whether a planned agent is actually running — a roster that cannot be
joined to reality would report "not created" for an agent that is working, or
quietly show a member nobody planned.

`agents[].readPaths` is what the agent is *given*; `agents[].writeScopes` is what
it *may modify*. The cockpit shows both, and warns when two agents claim
overlapping scopes — advisory, exactly as the team task board treats it.

## Running the checks on a manifest

```sh
node ../tools/inspect.mjs            # from the project root: prints what the panel will show
```

`inspect.mjs` resolves the manifest, validates it, derives the index and prints
it as text. It is the fastest way to see a manifest mistake without opening a
browser, and it reports every validation problem with its JSON pointer.
