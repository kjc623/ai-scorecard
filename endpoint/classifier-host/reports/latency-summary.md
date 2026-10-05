# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0026 ms | 10 ms | 0.0192 ms |
| rules | 20 ms | 0.1347 ms | 35 ms | 0.5775 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0056 ms | 90 ms | 0.0289 ms |
| **sum of stage p95** | 150 ms target | 0.1435 ms | 150 ms target | 0.6300 ms |
| **whole classification** | — | p50 0.0172 / p95 0.1443 / max 0.7695 ms | — | p50 0.0806 / p95 0.6241 / max 6.1478 ms |

js/wasm module: 6360665 bytes; compile+instantiate 12.258 ms; module run (whole corpus) 36.562 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
