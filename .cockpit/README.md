# Project Cockpit

A visual cockpit for a project being built with DeepSeek Harness agents: the
architecture as a map you can read at a glance, the live agent team, the shared
task board, and — the question this exists to answer — **exactly which files each
agent will be handed, and what its task is.**

It adds one icon to the sidebar. Clicking it opens a full-width panel beside
Conversation.

It is **read-only**: it reads your project and your team, and writes nothing.

---

## What it shows

Five tabs, all fed from one derived view model so no two tabs can disagree:

| Tab | What it answers |
| --- | --- |
| **Architecture** | Every component, grouped by layer, with its status, language, dependencies and the paths it owns. A status bar shows how much of the plan is done. Clicking a component opens its declared paths, its dependencies and its dependents. |
| **Agents** | The roster the project is planned around. Each agent shows its task, the files it is handed (read vs. write), and whether it exists in the live team yet — `not created`, `inactive`, `running`, or `failed`. Live teammates the manifest never declared still appear, because hiding them would misreport what is running. |
| **Tasks** | The written plan against the live team task board: matched by subject, planned-but-not-on-board, and on-board-but-not-planned. |
| **Files → Agents** | The join the whole tool is for. A table of which agent receives which file and why, the set of files each agent will be given, and the real workspace tree with each file labelled by its owner. Files belonging to no declared component or agent are listed separately. |
| **Verify** | Everything inconsistent, in one list: declared paths that are not on disk, agent write scopes that overlap, tasks in progress with no owner, a failed teammate. Plus the manifest path and the project's own verify/run commands. |

Two properties are worth calling out because they are deliberate:

- **Planned and present are never conflated.** A path the manifest declares but
  the workspace does not contain is reported as *absent*, not omitted. That
  distinction is the difference between an architecture document and a wish.
- **Absence is stated, not implied.** With no live team, the panel says "no live
  team in this session" rather than showing zero running agents, which would read
  as "nothing is happening".

## The agent also gets it

The plugin registers a model-facing tool, `project_cockpit`, with four views
(`overview`, `agents`, `files`, `tasks`). Your agents can orient themselves in
the project — and see the files they are about to be handed — without reading the
manifest or walking the tree.

## Planning actions — the one place it writes

The cockpit is read-only with respect to your files. The panel can, however, do
two things to the shared team task board, through the host's three actions
(`create_task`, `update_task`, `send_message`) and only those:

| Where | Action | What it does |
| --- | --- | --- |
| Tasks tab | **Add to team board** | Creates the planned task on the shared board, carrying its description and write scopes. A task whose subject is already on the board is shown with its **live revision** instead of being offered again — revision is what a later change must quote. |
| Agents tab | **Send task to member** | Messages that teammate its task statement, the files it is handed as context, its write scopes, and the paths declared for it that are not on disk yet. |

Both are attributed to the session the panel is showing, resolved to that
session's exact live Lead agent — never to "some agent", which would attribute
one project's work to another. Everything else is refused by name: no file
writes, no spawning, no interruption, no policy changes. That surface belongs to
the agent tools, where the model decides with context the cockpit does not have.

Every task change is a compare-and-set. A stale revision comes back as `409` with
the team service's own reason, shown beside the button that caused it rather than
in a page-wide banner — "the revision was stale" is an answer about *that* row.

The two halves of a plugin can be at different revisions: the client bundle is
served from the file and hot-reloads on a page refresh, while the host half needs
a restart. So the panel asks the host what it can do before offering an action,
and when the host is older it says *"Creating tasks needs the host half of the
cockpit to be restarted"* rather than rendering a button that would fail.

---

## The manifest

The cockpit reads `.cockpit/project.json` in the workspace root (it also looks
for `project.cockpit.json`, `cockpit.project.json` and `cockpit.json`, in that
order). Without a manifest it
still renders the real workspace tree, and says the manifest is missing.

The schema is [`.cockpit/schema/manifest-v1.schema.json`](schema/manifest-v1.schema.json).
The shape, in brief:

```jsonc
{
  "schemaVersion": 1,
  "project": {
    "name": "…", "summary": "…",
    "stage": "design | scaffold | building | hardening | shipped",
    "docsRoot": "docs",
    "testCommand": "…",   // shown in Verify
    "runCommand": "…"
  },
  "components": [{
    "id": "capture-core",          // lowercase kebab-case, referenced everywhere else
    "name": "Capture Core",
    "kind": "service",             // free-form: service, device-agent, ui, data, docs…
    "layer": "device",             // the row it sits in on the architecture map
    "language": "Go", "runtime": "…",
    "status": "proposed | scaffolded | in-progress | done | blocked | cut",
    "summary": "…",
    "dependsOn": ["classifier-host"],   // component ids; rendered as edges, cycles rejected
    "paths": ["src/capture"],           // files or directory prefixes this component owns
    "docs": ["docs/01-collectors.md"],
    "decisions": ["docs/adr/0001-….md"]
  }],
  "tasks": [{
    "id": "T3", "subject": "…", "description": "…",
    "status": "pending | in_progress | completed | blocked",
    "dependsOn": ["T1"], "componentIds": ["ingest-api"],
    "writeScopes": ["ingestion/ingest-api"], "owner": "ingestor"
  }],
  "agents": [{
    "id": "ingestor",              // must match the DSH teammate name exactly
    "role": "Backend engineer", "emoji": "📥", "purpose": "…",
    "taskId": "T3",
    "task": "The statement handed to this agent, verbatim.",
    "componentIds": ["ingest-api"],
    "readPaths": ["docs/02-ingest-and-transport.md"],  // context it is given
    "writeScopes": ["ingestion/ingest-api"]             // what it may modify
  }],
  "invariants": [{
    "id": "INV-1", "statement": "…",
    "componentIds": ["…"], "record": "docs/adr/0008-….md"
  }]
}
```

`agents[].id` is the join key: it must match the name you give `spawn_teammate`,
which is how the panel knows whether a planned agent is actually running.

### What validation catches

Validation is strict and reports **every** problem with a JSON pointer rather
than stopping at the first, because a silently misread manifest draws a
confident, wrong architecture. It rejects: an unknown status, a duplicate id, a
malformed id, a dependency on a component that does not exist, a dependency
cycle, a task referencing an unknown task or component, and an unknown
`taskId`. Two agents claiming overlapping write scopes is reported as an
**advisory collision** — the same rule the team task board uses, and equally
non-blocking.

---

## Install

The plugin lives in this workspace at `.cockpit/` and is installed into a DSH
profile as a local file dependency:

```sh
dsh plugin --profile desktop add file:C:/architecture/.cockpit
```

`add file:` **copies** the directory into the profile, so the install is a
snapshot. After changing anything here, sync it — with DSH closed:

```sh
node .cockpit/tools/sync-install.mjs --apply
```

Then add it to the profile's bundle stack — either through the plugin manager,
or by hand in `$DSH_HOME/profiles/<profile>/package.json`:

```json
"dsh": { "profile": { "bundles": [ "…", "dsh-project-cockpit" ] } }
```

**A newly installed plugin's browser half needs a host restart.** The host half
(the route and the tool) hot-loads immediately; the client module registry
caches its verdict for a loader entry until restart, so the sidebar icon appears
only after DSH restarts. Editing `lib/client.js` afterwards is hot-reloaded on a
page refresh, because the bundle revision is derived from the file's mtime,
ctime and size.

## Moving it, and using it on any project

The cockpit is not tied to one project. Nothing in it is anchored to its own
folder: the project is chosen per call, in this order —

1. a `root` set in the profile config, which pins one project;
2. **the calling session's working directory** — the folder you started the
   session in, which is what "the project" means;
3. the host process's directory, as an honest last resort.

The panel names its session in every request and the footer shows the directory
that was actually read *and which rule chose it*, so "which project am I looking
at" is never a guess. Two sessions in two directories get two different projects.

**To move the package to its own folder:**

```sh
# 1. move it
move C:\architecture\.cockpit  C:\Tools\dsh-project-cockpit

# 2. re-point the profile at the new address (DSH closed)
#    or simply:  dsh plugin --profile desktop add file:C:/Tools/dsh-project-cockpit
#    in $DSH_HOME/profiles/desktop/package.json the dependency becomes
#      "dsh-project-cockpit": "file:C:/Tools/dsh-project-cockpit"
#    the bundles list still names the package, so it does not change.

# 3. restore the dev-only SDK links the local test suite needs
cd C:\Tools\dsh-project-cockpit
node tools/link-sdk.mjs        # links from a DSH checkout, or extracts from app.asar
node test/all.mjs
```

Three things to know:

- **`node_modules/@deepseek-ai/` will break.** It holds junctions into the DSH
  source extraction used while building. `tools/link-sdk.mjs` recreates them, and
  when no checkout is available it **extracts the packages out of the installed
  harness's `app.asar`** — so a moved copy is self-sufficient. The plugin itself
  never reads this directory.
- **The manifest moves with the *project*, not with the tool.** `.cockpit/project.json`
  was written for the Shadow AI Capture package and describes that package only —
  no component in it claims `.cockpit` — so it stays behind at the project's root
  when the package moves out. Each project you use the cockpit on gets its own
  `.cockpit/project.json` at its root.
- **The suite skips rather than fails where its fixture is gone.** A dozen checks
  read the project this package was built beside; move the package and they report
  `skip … no fixture project beside this package`. The remaining checks cover the
  derivation, the bundle contract, the panel and the host plugin, and run
  anywhere.

`tools/relaunch.cmd` still works after a move — it resolves the workspace
relative to its own location.

## Configuration

Override in the profile's `cordis.patch.yml` (the user layer outranks bundle
layers):

```yaml
- id: project-cockpit
  name: 'dsh-project-cockpit'
  config:
    root: ''          # '' = each session's own working directory
    ttlMs: 2000       # how long a built index stays warm
    maxFiles: 4000    # bound on the workspace walk
    serveRoute: true  # serve /api/project-cockpit to the panel
    exposeTool: true  # register the project_cockpit tool
    allowActions: true  # false = strictly read-only, the POST route answers 403
```

Set `allowActions: false` for a cockpit that only ever reads. The GET half keeps
working; only the two planning actions are refused.

## Develop

```sh
cd .cockpit
node test/all.mjs            # 92 checks across five suites
node tools/inspect.mjs       # print the cockpit's own view, no browser needed
node tools/build-client.mjs  # rebuild lib/client.js from lib/client/*.js
node tools/build-client.mjs --check   # fail if the bundle is stale
node tools/sync-install.mjs           # report drift between this workspace and the install
node tools/sync-install.mjs --apply   # copy the workspace over the install (DSH closed)
```

`pnpm add file:` makes a **copy** of this directory, not a link, so a profile
install drifts the moment the workspace changes. `tools/sync-install.mjs` reports
which installed files differ and copies them when asked. It must run with DSH
closed: the running harness holds the installed files open, and the copy fails
with a sharing violation rather than a permission error.

To do both in one step — sync, then start DSH — double-click
`tools/relaunch.cmd`. It refuses to launch if the sync did not complete, so a
partial install can never be started into a failed boot.

The client half has no build toolchain: `tools/build-client.mjs` concatenates
`lib/client/*.js` into the single lazy-CJS file the shell loads, rewriting
imports to the shell's frozen module table and refusing anything not in it.

| Suite | What it proves |
| --- | --- |
| `test/run.mjs` | Manifest validation, cycle detection, overlap detection, path resolution, and a real scan of this workspace. |
| `test/bundle.mjs` | The built bundle registers with the shell correctly — the sidebar id equals the panel key, no default export (which would drop `inject`), no undeclared module requests. |
| `test/panel.mjs` | What the panel renders, from real index shapes: the project, the layers, the edges, the absent paths, the roster against a live team, the board action in each of its states, and the failure states. |
| `test/host.mjs` | The host half mounts, registers its route and tool, and every view — including the three planning actions and their refusals — answers with the right data. |
| `test/composition.mjs` | The **shell's own** client-module scanner accepts the package. It imports `ClientModuleRegistry` from the shipped `@deepseek-ai/dsh-client-modules`, feeds it this profile's loader rows, and asserts a composed row with a resolvable bundle, the right module id, and a valid revision. This is the check that distinguishes "my manifest looks right" from "the host composes it". |

`test/composition.mjs` reads the installed profile, so it is specific to a
desktop install. It fails loudly rather than skipping when the profile or the
development junctions are missing.

### Layout

```
.cockpit/
  package.json          dsh manifest: bundle patch + client declaration
  cordis.patch.yml      the loader row that inserts the plugin
  project.json          this workspace's manifest
  schema/               manifest-v1.schema.json
  lib/
    index.js            host half: cordis plugin, /api route, project_cockpit tool
    actions.js          the planning actions: create, transition, message
    store.js            manifest location, cached build, document outlines
    discover.js         workspace walk, declared-path resolution, index derivation
    manifest.js         validation and normalization
    client.js           GENERATED client bundle
    client/
      index.js          panel + sidebar glyph + slot registrations + action UI
      viewmodel.js      status breakdown, roster merge, task match, file tree
      styles.js         the stylesheet, themed through --dsw-* tokens
  test/                 five suites, run by test/all.mjs
  tools/                bundle builder, workspace inspector, install sync
```

> **If the harness ever fails to start after an update**, the install is
> incomplete: a partial copy leaves `lib/index.js` importing a file that is not
> there, and DSH's boot is all-or-nothing. Quit DSH, then run
> `node .cockpit/tools/sync-install.mjs --apply` (or `tools/relaunch.cmd`, which
> syncs and restarts) and start it again. As a last resort, remove
> `dsh-project-cockpit` from the profile's `dsh.profile.bundles` to boot without
> the plugin.

`node_modules/` holds development-only junctions to the SDK packages the host
half imports and `test/composition.mjs` scans, which a local Node process cannot
otherwise resolve. They are not part of the package and the plugin does not need
them when installed.

## Limits

- **Bash, formatters and other writers bypass it.** The index is a snapshot with
  a TTL; it is not a filesystem watcher.
- **The workspace walk is bounded** (depth, file count, entries per directory).
  When it truncates, the panel says so.
- **Tasks are matched to the live board by subject text**, the only join
  available — the two id spaces are independent. Unmatched rows are reported as
  unmatched rather than guessed at.
- **Write-scope overlaps are advisory.** They never block a claim, exactly as the
  team task board intends.
- **One manifest per workspace root.** A session's working directory selects the
  project.
- **The cockpit claims no identity of its own.** Its actions speak for the
  session's Lead, which is why a panel with no open session offers no actions
  instead of picking one.
- **A newly installed plugin needs a host restart** for its browser half; see
  Install above. Editing the client bundle afterwards only needs a page refresh.
