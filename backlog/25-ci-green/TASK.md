# 25. Make CI pass on GitHub Actions

Needs: the owner has pushed the branch (agents do not push unless asked).

## Problem

`.github/workflows/ci.yml` has never run. It runs `node tools/accept.mjs` on Ubuntu (skipping the
Windows-only installer gate) and `node tools/accept.mjs --only go,installer` on Windows. The gate
passes on the owner's Windows machine, but a hosted runner differs: a clean module cache, no
pre-built images, a different Chrome, Docker only on Linux, no WiX.

## Goal

Both CI jobs pass on a pull request, and every gate that can run on each runner does run (none
reported `SKIPPED` except where the runner genuinely cannot: the installer gate on Linux).

## Scope

- Read the failing runs' logs (`gh run list`, `gh run view --log-failed`) and fix the causes in the
  workflow, in `tools/accept.mjs`, or in the component that fails — whichever is wrong.
- If a gate needs a tool the runner lacks (e.g. WiX for the installer checks, or a Chrome for the
  browser check), install it in the workflow step, pinned to a version.
- Do not weaken a gate or skip a test to get green. If a test is environment-dependent, make it
  correct on both machines.
- Out of scope: `deploy.yml` (needs Azure; task 26).

## Done when

- The latest CI run on the branch is green for both jobs, and its logs show every expected gate ran.
- `node tools/accept.mjs` still passes on the owner's Windows machine.
