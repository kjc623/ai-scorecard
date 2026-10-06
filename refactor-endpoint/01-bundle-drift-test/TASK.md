# 01. Make the policy drift test run

## Problem

The policy bundle is defined twice: once on the server in
`services/control-api/internal/policyserve/bundle.go`, and once on the device in
`device/capture-core/policy/bundle.go`. The device decoder refuses unknown fields, so a field the
server sends that the device doesn't know makes every device reject every bundle.

`TestServedBundleVerifiesWithTheDevicesVerifier`
(`services/control-api/internal/policyserve/service_test.go`, around line 398) exists to catch
that drift. It looks for the device modules under `<repo>/endpoint/`, but they moved to
`<repo>/device/`, so the test always reaches `t.Skipf` and checks nothing. Tasks 07, 08, 11 and 14
add bundle sections and depend on this test.

## Goal

The drift test runs, and fails when the two bundle definitions disagree.

## Scope

- In `service_test.go`, resolve `<repo>/device` instead of `<repo>/endpoint`, and rename the local
  variable to match. The `replace` lines for `capture-core`, `protocol` and `capture-spool` must
  point under `device/`.
- Turn the missing-directory skip into a failure (`t.Fatalf`). The directory is part of this
  repository, so its absence is a defect, not an environment gap. Keep the `-short` skip and the
  no-Go-toolchain skip.
- Check whether the throwaway module also needs a `replace` for
  `github.com/shadow-ai-capture/contracts` (`contracts/generated/go`). Add it only if the build
  fails without it.
- Change nothing else.

## Done when

- `cd services/control-api && go test ./internal/policyserve/ -run TestServedBundleVerifiesWithTheDevicesVerifier -v`
  shows the test running and passing (not `SKIP`).
- Adding a throwaway field to `policyserve.Bundle`, populated in `compose`, makes the test fail.
  Revert it afterwards, and quote the failure output in the report.
- `node tools/accept.mjs` passes.
