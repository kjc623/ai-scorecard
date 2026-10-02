# §9.4 latency, measured (windows/amd64)

Corpus `corpus.json`, 20 cases, 200 iterations per case. p95 is nearest-rank over every stage observation.

| stage | §9.4 native | measured native p95 | §9.4 wasm | measured wasm p95 |
|---|---|---|---|---|
| normalise | 5 ms | 0.0030 ms | 10 ms | 0.0205 ms |
| rules | 20 ms | 0.1346 ms | 35 ms | 0.5775 ms |
| validators | 10 ms | 0.0007 ms | 15 ms | 0.0046 ms |
| model | 60 ms | 0.0056 ms | 90 ms | 0.0310 ms |
| **sum of stage p95** | 150 ms target | 0.1439 ms | 150 ms target | 0.6336 ms |
| **whole classification** | — | p50 0.0176 / p95 0.1445 / max 0.9890 ms | — | p50 0.0817 / p95 0.6226 / max 6.0570 ms |

js/wasm module: 6344880 bytes; compile+instantiate 12.244 ms; module run (whole corpus) 38.590 ms.

Every stage p95 is within its §9.4 share and every sum is within the interactive target.
