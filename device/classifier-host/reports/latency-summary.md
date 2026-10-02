# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0032 ms | 10 ms | 0.0200 ms |
| rules | 20 ms | 0.1344 ms | 35 ms | 0.5670 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0061 ms | 90 ms | 0.0300 ms |
| **sum of stage p95** | 150 ms target | 0.1444 ms | 150 ms target | 0.6216 ms |
| **whole classification** | — | p50 0.0180 / p95 0.1440 / max 0.5258 ms | — | p50 0.0822 / p95 0.6131 / max 6.9414 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.187 ms; module run (whole corpus) 38.844 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
