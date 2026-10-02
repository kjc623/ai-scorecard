# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0028 ms | 10 ms | 0.0184 ms |
| rules | 20 ms | 0.1302 ms | 35 ms | 0.5373 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0054 ms | 90 ms | 0.0279 ms |
| **sum of stage p95** | 150 ms target | 0.1390 ms | 150 ms target | 0.5880 ms |
| **whole classification** | — | p50 0.0173 / p95 0.1387 / max 0.6460 ms | — | p50 0.0783 / p95 0.5811 / max 5.6335 ms |

js/wasm module: 6345054 bytes; compile+instantiate 12.021 ms; module run (whole corpus) 36.967 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
