# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0195 ms |
| rules | 20 ms | 0.1334 ms | 35 ms | 0.5670 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0057 ms | 90 ms | 0.0289 ms |
| **sum of stage p95** | 150 ms target | 0.1428 ms | 150 ms target | 0.6200 ms |
| **whole classification** | — | p50 0.0180 / p95 0.1430 / max 0.8931 ms | — | p50 0.0801 / p95 0.6139 / max 7.8093 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.802 ms; module run (whole corpus) 38.517 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
