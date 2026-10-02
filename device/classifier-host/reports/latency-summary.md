# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0029 ms | 10 ms | 0.0197 ms |
| rules | 20 ms | 0.1342 ms | 35 ms | 0.5635 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0055 ms | 90 ms | 0.0287 ms |
| **sum of stage p95** | 150 ms target | 0.1432 ms | 150 ms target | 0.6164 ms |
| **whole classification** | — | p50 0.0177 / p95 0.1429 / max 0.7688 ms | — | p50 0.0812 / p95 0.6080 / max 6.8411 ms |

js/wasm module: 6345054 bytes; compile+instantiate 12.362 ms; module run (whole corpus) 39.673 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
