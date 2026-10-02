# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0200 ms |
| rules | 20 ms | 0.1337 ms | 35 ms | 0.5640 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0049 ms |
| model | 60 ms | 0.0055 ms | 90 ms | 0.0302 ms |
| **sum of stage p95** | 150 ms target | 0.1429 ms | 150 ms target | 0.6190 ms |
| **whole classification** | — | p50 0.0173 / p95 0.1432 / max 0.8986 ms | — | p50 0.0819 / p95 0.6103 / max 6.7971 ms |

js/wasm module: 6345054 bytes; compile+instantiate 12.615 ms; module run (whole corpus) 41.422 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
