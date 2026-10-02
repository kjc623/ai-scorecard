# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0031 ms | 10 ms | 0.0223 ms |
| rules | 20 ms | 0.1360 ms | 35 ms | 0.5622 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0054 ms |
| model | 60 ms | 0.0058 ms | 90 ms | 0.0320 ms |
| **sum of stage p95** | 150 ms target | 0.1456 ms | 150 ms target | 0.6218 ms |
| **whole classification** | — | p50 0.0181 / p95 0.1459 / max 0.6610 ms | — | p50 0.0865 / p95 0.6083 / max 7.7489 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.059 ms; module run (whole corpus) 39.391 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
