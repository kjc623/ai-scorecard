# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0027 ms | 10 ms | 0.0184 ms |
| rules | 20 ms | 0.1324 ms | 35 ms | 0.5517 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0055 ms | 90 ms | 0.0279 ms |
| **sum of stage p95** | 150 ms target | 0.1412 ms | 150 ms target | 0.6024 ms |
| **whole classification** | — | p50 0.0173 / p95 0.1415 / max 0.6494 ms | — | p50 0.0788 / p95 0.5957 / max 6.0554 ms |

js/wasm module: 6345054 bytes; compile+instantiate 12.127 ms; module run (whole corpus) 37.876 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
