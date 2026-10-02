# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0028 ms | 10 ms | 0.0202 ms |
| rules | 20 ms | 0.1346 ms | 35 ms | 0.5473 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0049 ms |
| model | 60 ms | 0.0055 ms | 90 ms | 0.0302 ms |
| **sum of stage p95** | 150 ms target | 0.1435 ms | 150 ms target | 0.6026 ms |
| **whole classification** | — | p50 0.0174 / p95 0.1442 / max 0.8598 ms | — | p50 0.0817 / p95 0.5924 / max 6.5915 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.437 ms; module run (whole corpus) 39.494 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
