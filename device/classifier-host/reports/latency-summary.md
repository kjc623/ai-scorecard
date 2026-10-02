# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0028 ms | 10 ms | 0.0189 ms |
| rules | 20 ms | 0.1303 ms | 35 ms | 0.5496 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0054 ms | 90 ms | 0.0289 ms |
| **sum of stage p95** | 150 ms target | 0.1391 ms | 150 ms target | 0.6021 ms |
| **whole classification** | — | p50 0.0170 / p95 0.1395 / max 1.0157 ms | — | p50 0.0794 / p95 0.5975 / max 5.9814 ms |

js/wasm module: 6345054 bytes; compile+instantiate 12.237 ms; module run (whole corpus) 38.699 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
