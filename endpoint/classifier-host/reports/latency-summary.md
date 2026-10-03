# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0027 ms | 10 ms | 0.0192 ms |
| rules | 20 ms | 0.1342 ms | 35 ms | 0.5645 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0054 ms | 90 ms | 0.0289 ms |
| **sum of stage p95** | 150 ms target | 0.1429 ms | 150 ms target | 0.6170 ms |
| **whole classification** | — | p50 0.0170 / p95 0.1441 / max 0.7540 ms | — | p50 0.0788 / p95 0.6077 / max 5.8916 ms |

js/wasm module: 6345103 bytes; compile+instantiate 12.843 ms; module run (whole corpus) 37.035 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
