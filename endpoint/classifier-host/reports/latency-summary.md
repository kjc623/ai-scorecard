# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0027 ms | 10 ms | 0.0192 ms |
| rules | 20 ms | 0.1354 ms | 35 ms | 0.5548 ms |
| validators | 10 ms | 0.0006 ms | 15 ms | 0.0044 ms |
| model | 60 ms | 0.0053 ms | 90 ms | 0.0282 ms |
| **sum of stage p95** | 150 ms target | 0.1440 ms | 150 ms target | 0.6065 ms |
| **whole classification** | — | p50 0.0170 / p95 0.1441 / max 0.6677 ms | — | p50 0.0786 / p95 0.6003 / max 5.9615 ms |

js/wasm module: 6345103 bytes; compile+instantiate 12.541 ms; module run (whole corpus) 37.599 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
