# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0031 ms | 10 ms | 0.0210 ms |
| rules | 20 ms | 0.1341 ms | 35 ms | 0.5722 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0049 ms |
| model | 60 ms | 0.0059 ms | 90 ms | 0.0312 ms |
| **sum of stage p95** | 150 ms target | 0.1438 ms | 150 ms target | 0.6292 ms |
| **whole classification** | — | p50 0.0176 / p95 0.1437 / max 0.9443 ms | — | p50 0.0858 / p95 0.6208 / max 7.3272 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.450 ms; module run (whole corpus) 40.788 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
