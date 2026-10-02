// contracts/tools/index.js
//
// Entry point for the directory form of the acceptance command, `node --test contracts/tools/`.
// Node 22 treats a positional test path as a glob pattern and a file path, not as a directory,
// so that command resolves `contracts/tools/` through the CommonJS loader; without this file it
// fails with "Cannot find module .../contracts/tools" and no test ever runs. Requiring the real
// suite here registers it synchronously (Node 22 supports require() of an ES module), so the
// directory form runs exactly the same tests as `node contracts/tools/verify.mjs`.

require("./verify.mjs");
