/**
 * run-tests.mjs — the entry point `node --test apps/capture-extension` resolves to.
 *
 * It exists because of a Node-version detail, verified rather than assumed: on Node 22.23.1 a
 * positional argument to `--test` is treated as a *file*, not a directory to search, so
 * `node --test apps/capture-extension` would try to load the directory as a module and fail with
 * `Cannot find module`. `package.json`'s `main` field is how Node resolves that directory, so this
 * file is what the documented command loads, and it imports the real suite.
 *
 * It contains no tests of its own. Three invocations all work and all run the same 175 tests:
 *
 *   node --test apps/capture-extension                            (from the repo root — this file)
 *   node --test 'apps/capture-extension/**'*.test.mjs             (from the repo root — the glob)
 *   cd apps/capture-extension && node --test                      (discovery from the package)
 *
 * Chromium ignores `package.json` entirely, so nothing here affects the extension as loaded by a
 * browser.
 */

import './contract.test.mjs';
import './test/codec.test.mjs';
import './test/predicate.test.mjs';
import './test/mode-policy.test.mjs';
import './test/enforce.test.mjs';
import './test/queue-health.test.mjs';
import './test/native.test.mjs';
import './test/attachments.test.mjs';
import './test/content-script.test.mjs';
import './test/content-roundtrip.test.mjs';
import './test/worker.test.mjs';
