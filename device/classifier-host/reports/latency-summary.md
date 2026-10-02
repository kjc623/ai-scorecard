# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0202 ms |
| rules | 20 ms | 0.1340 ms | 35 ms | 0.5663 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0056 ms | 90 ms | 0.0305 ms |
| **sum of stage p95** | 150 ms target | 0.1433 ms | 150 ms target | 0.6216 ms |
| **whole classification** | — | p50 0.0175 / p95 0.1438 / max 0.5508 ms | — | p50 0.0827 / p95 0.6121 / max 6.1317 ms |

js/wasm module: 6345054 bytes; compile+instantiate 11.930 ms; module run (whole corpus) 37.937 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
