# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0032 ms | 10 ms | 0.0212 ms |
| rules | 20 ms | 0.1354 ms | 35 ms | 0.5535 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0051 ms |
| model | 60 ms | 0.0059 ms | 90 ms | 0.0328 ms |
| **sum of stage p95** | 150 ms target | 0.1452 ms | 150 ms target | 0.6126 ms |
| **whole classification** | — | p50 0.0181 / p95 0.1450 / max 0.9426 ms | — | p50 0.0863 / p95 0.6001 / max 8.4972 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.863 ms; module run (whole corpus) 45.715 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
