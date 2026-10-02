# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0029 ms | 10 ms | 0.0207 ms |
| rules | 20 ms | 0.1347 ms | 35 ms | 0.5545 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0049 ms |
| model | 60 ms | 0.0058 ms | 90 ms | 0.0310 ms |
| **sum of stage p95** | 150 ms target | 0.1441 ms | 150 ms target | 0.6111 ms |
| **whole classification** | — | p50 0.0176 / p95 0.1444 / max 0.5337 ms | — | p50 0.0845 / p95 0.6021 / max 6.5311 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.580 ms; module run (whole corpus) 39.649 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
