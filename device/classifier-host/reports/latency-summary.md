# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0033 ms | 10 ms | 0.0241 ms |
| rules | 20 ms | 0.1364 ms | 35 ms | 0.5847 ms |
| validators | 10 ms | 0.0008 ms | 15 ms | 0.0074 ms |
| model | 60 ms | 0.0064 ms | 90 ms | 0.0392 ms |
| **sum of stage p95** | 150 ms target | 0.1469 ms | 150 ms target | 0.6554 ms |
| **whole classification** | — | p50 0.0183 / p95 0.1463 / max 0.5733 ms | — | p50 0.0878 / p95 0.6331 / max 7.5469 ms |

js/wasm module: 6344880 bytes; compile+instantiate 11.945 ms; module run (whole corpus) 39.699 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
