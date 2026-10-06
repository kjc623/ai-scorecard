// Entry point for the directory form `node --test contracts/tools/`. Node treats a positional test
// path as a file or a glob rather than a directory, so the directory form resolves this file;
// requiring the suite here runs exactly the tests `node contracts/tools/verify.mjs` runs.

require("./verify.mjs");
