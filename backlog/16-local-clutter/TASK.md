# 16. Remove local state left over in the working copy

Needs: the owner's machine, and the owner's confirmation in the session before anything is deleted.

## Problem

The owner's working copy still has untracked folders from tooling and labs that no longer exist.
They are not in git (`.git/info/exclude` hides them), but they make the repository look cluttered on
disk:

| Folder | What it is |
|---|---|
| `.cockpit/` | Leftovers of a removed desktop-harness plugin (e.g. `node_modules`) |
| `.integration/` | Old acceptance evidence, and `observe/` screenshots of lab data that show names and prompts |
| `.research/` | An extracted third-party SDK, about 35 MB |
| `.testtmp/` | Test runners' scratch space and browser profiles |
| `.tools/` | Old Go build and module caches pointed inside the repository, tens of thousands of files |
| `bin/` | Stale host-built binaries |
| `installer/.lab/` (or `device/installer/.lab/` after task 15) | Staging folders of the old lab MSI build |

## Goal

The working copy holds only tracked files, git-ignored build output that the current tools create,
and the owner's own `.claude/` and `skills-lock.json`.

## Steps

1. For each folder: confirm `git ls-files <folder>` is empty and that nothing tracked references it
   (`git grep -n "<folder>"`). Report anything that does not hold.
2. List the folders with their size and file count, and ask the owner to confirm the deletion.
   Mention specifically that `.integration/observe/` holds screenshots of lab data.
3. Delete the confirmed folders.
4. Remove their lines from `.git/info/exclude`, keeping `.claude/`, `skills-lock.json` and
   `localdev/harness/`.

Out of scope: `localdev/.authlab/`, `localdev/.authlab-identity/` and `localdev/.msi/` (the lab's key
material and lab MSI — keep them), and any tracked file.

## Done when

- `git status --short --ignored` shows no entry for the deleted folders.
- `node tools/accept.mjs` still passes (it recreates any cache it needs elsewhere).
