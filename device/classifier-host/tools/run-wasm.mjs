// run-wasm.mjs — the Node host for the js/wasm classifier module.
//
// ADR 0016 fixes the loader: one source, two targets, the wasm copy loaded through Go's own
// runtime shim. This driver is the *test host* of that decision — it loads
// $(go env GOROOT)\lib\wasm\wasm_exec.js, hands the module the same argv the native binary gets,
// and reports the two costs the ADR makes an acceptance criterion rather than a footnote:
//
//   - compile_and_instantiate_ms: what the extension pays once, before the first inline decision
//   - run_ms: what the module itself took for the arguments it was given
//
// The module is executed with the same subcommands as the native build (`classify`, `measure`,
// `release`), so the equivalence run and the latency measurement are the same code path on both
// targets. A browser host would use wasm_exec.js identically; only this timing shell differs.
//
// Usage: node tools/run-wasm.mjs <module.wasm> [args...]
// The metrics JSON is written to **stderr** so stdout stays exactly what the module produced.

import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { performance } from "node:perf_hooks";
import { TextEncoder, TextDecoder } from "node:util";

const require = createRequire(import.meta.url);

// The shim expects the environment a browser page would give it; in Node these come from the
// standard library instead. This mirrors what Go's own wasm_exec_node.js installs.
globalThis.require = require;
globalThis.fs = fs;
globalThis.path = path;
globalThis.TextEncoder = TextEncoder;
globalThis.TextDecoder = TextDecoder;
globalThis.performance ??= performance;
globalThis.crypto ??= require("node:crypto");

const here = path.dirname(fileURLToPath(import.meta.url));
const [wasmPath, ...goArgs] = process.argv.slice(2);
if (!wasmPath) {
  console.error("usage: node tools/run-wasm.mjs <module.wasm> [args...]");
  process.exit(2);
}

require(path.join(here, "wasm_exec.js"));

const go = new Go();
go.argv = [wasmPath, ...goArgs];
go.env = Object.assign({ TMPDIR: os.tmpdir() }, process.env);
go.exit = process.exit;

const metrics = { wasm_path: wasmPath, args: goArgs, module_bytes: 0, compile_and_instantiate_ms: 0, run_ms: 0, exit_code: 0 };
process.on("exit", (code) => {
  metrics.exit_code = code ?? 0;
  console.error(JSON.stringify(metrics));
});

const t0 = performance.now();
const bytes = fs.readFileSync(wasmPath);
metrics.module_bytes = bytes.length;
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
const t1 = performance.now();
metrics.compile_and_instantiate_ms = Number((t1 - t0).toFixed(3));

const done = go.run(instance);
const t2 = performance.now();
metrics.run_ms = Number((t2 - t1).toFixed(3));
await done;
