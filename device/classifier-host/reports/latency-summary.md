# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0200 ms |
| rules | 20 ms | 0.1357 ms | 35 ms | 0.5699 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0051 ms |
| model | 60 ms | 0.0056 ms | 90 ms | 0.0315 ms |
| **sum of stage p95** | 150 ms target | 0.1450 ms | 150 ms target | 0.6264 ms |
| **whole classification** | — | p50 0.0177 / p95 0.1454 / max 0.5883 ms | — | p50 0.0835 / p95 0.6154 / max 6.0692 ms |

js/wasm module: 6345054 bytes; compile+instantiate 11.863 ms; module run (whole corpus) 38.447 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
