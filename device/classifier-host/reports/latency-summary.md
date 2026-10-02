# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0192 ms |
| rules | 20 ms | 0.1344 ms | 35 ms | 0.5542 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0055 ms | 90 ms | 0.0292 ms |
| **sum of stage p95** | 150 ms target | 0.1436 ms | 150 ms target | 0.6070 ms |
| **whole classification** | — | p50 0.0180 / p95 0.1441 / max 0.9535 ms | — | p50 0.0796 / p95 0.5993 / max 6.0810 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.320 ms; module run (whole corpus) 39.862 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
