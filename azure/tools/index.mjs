// index.mjs — `node --test infra/tools/` loads this file when the path names a directory, so the
// suite is reachable both as `node --test infra/tools/` and as `node --test infra/tools/check-infra.test.mjs`.
// It contains no tests of its own: importing the suite is the whole point, and a second copy of the
// tests here would be a second thing to keep true.
import './check-infra.test.mjs';
