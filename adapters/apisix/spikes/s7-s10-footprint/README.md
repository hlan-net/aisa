# Spikes S7 + S10: footprint and added latency

The questions and outcomes are recorded in [`docs/process/SPIKES.md`](../../../../docs/process/SPIKES.md#s7--s10-footprint-and-added-latency). This directory holds what they are based on, so they can be re-run on other hardware or against a newer APISIX.

```bash
./adapters/apisix/spikes/s7-s10-footprint/run.sh                      # one nginx worker per core
APISIX_WORKERS=1 ./adapters/apisix/spikes/s7-s10-footprint/run.sh     # one worker
docker compose -f dev/compose.yaml -f adapters/apisix/spikes/s7-s10-footprint/compose.override.yaml down -v
```

`STREAMS`, `DURATION`, `TOKENS` and `REQUESTS` change the load; see the top of `run.sh`.

| File | Contents |
|---|---|
| [`config.yaml`](config.yaml) | The S8 config with the number of nginx workers from `APISIX_WORKERS` |
| [`apisix.yaml`](apisix.yaml) | `/full/`, the adapter shape chosen in S8 with the usage event of S6; `/noauth/`, the same without `forward-auth`; `/direct/`, one hop and no decision |
| [`compose.override.yaml`](compose.override.yaml) | Adds `mock-stream` (200 ms to the first token, then 50 tokens a second), `mock-fast` (answers at once) and a consul-template that watches the backends in Consul |
| [`backends.ctmpl`](backends.ctmpl) | The template it renders. Rendering the real `apisix.yaml` is spike S9 |
| [`run.sh`](run.sh) | Runs parallel streams through `/full/` and measures CPU and memory of APISIX, the stub aisa and consul-template; then compares the latency of the three routes. Exits non-zero when a request fails or a stream has no usage event |

## How it measures

- **Memory** is the proportional set size (PSS) of all processes of a container, the highest of the samples taken every 2 s. Memory that the nginx workers share is counted once; the sum of their resident set sizes was 200 MB where the PSS was 126 MB. A helper container in the host's PID namespace reads it from `/proc`, because Docker on Raspberry Pi OS has no memory accounting by default (`cgroup_disable=memory`).
- **CPU** is the container's CPU time from its cgroup, as average cores over a row, or as milliseconds per request in the latency part.
- **The load generator runs on the same machine**: one `curl` per stream. It competes with the gateway for the four cores, so the latencies under load are upper bounds.
- **aisa is the stub.** It looks up a key in a map and keeps what it receives in memory. Its numbers are a floor for aisa, not an estimate.

## Output of the recorded runs (2026-09-28, APISIX 3.18.0, arm64)

Raspberry Pi 5 (4 cores, 16 GB), Docker. Not the Raspberry Pi 4 that S7 names: see the outcome in `SPIKES.md` and [#16](https://github.com/hlan-net/aisa/issues/16).

```
== S7: footprint under parallel streams
   aarch64, 4 cores, APISIX_WORKERS=auto (4 worker processes)
   each stream: 256 tokens at 50 tokens/s through /full/; 30 s per row; memory is peak PSS
                                usage   TTFT ms  apisix       stub-aisa    consul-tpl  
streams     requests  failed   events   p50/p95  cores / MB   cores / MB   cores / MB  
idle               -       -        -         -  0.01 / 119    0.01 / 8      0.00 / 15   
1                  6       0        6   215/295  0.02 / 121    0.01 / 8      0.00 / 15   
10                60       0       60   222/238  0.14 / 143    0.01 / 9      0.00 / 15   
50               263       0      263   249/311  0.39 / 169    0.02 / 13     0.00 / 15   
100              500       0      500   324/442  0.61 / 178    0.03 / 13     0.00 / 15   
idle after         -       -        -         -  0.01 / 178    0.01 / 14     0.00 / 15   

== S10: latency of a non-streamed request to a backend that answers at once (ms)
   600 requests per row over kept-alive connections, in 3 rounds; CPU is per request
clients  variant          p50     p95     p99    mean  failed  apisix ms  stub ms   
1        direct          2.81    7.35    9.88    3.38       0  1.63       0.82      
1        noauth          3.31    7.93   10.43    3.78       0  2.30       0.89      
1        full            4.63   10.07   14.03    5.34       0  2.76       1.37      
1        p50 added by the internal hop: 0.51 ms, by forward-auth: 1.32 ms
10       direct         14.43   42.40   52.91   17.38       0  1.67       0.71      
10       noauth         15.28   40.19   54.14   17.71       0  1.98       0.68      
10       full           17.36   36.11   54.52   19.42       0  2.29       0.84      
10       p50 added by the internal hop: 0.85 ms, by forward-auth: 2.09 ms

all requests succeeded, and every stream has its usage event
```

With one worker:

```
== S7: footprint under parallel streams
   aarch64, 4 cores, APISIX_WORKERS=1 (1 worker processes)
   each stream: 256 tokens at 50 tokens/s through /full/; 30 s per row; memory is peak PSS
                                usage   TTFT ms  apisix       stub-aisa    consul-tpl  
streams     requests  failed   events   p50/p95  cores / MB   cores / MB   cores / MB  
idle               -       -        -         -  0.01 / 61     0.01 / 7      0.00 / 15   
1                  6       0        6   214/221  0.02 / 62     0.01 / 8      0.00 / 15   
10                60       0       60   216/230  0.06 / 72     0.01 / 9      0.00 / 15   
50               300       0      300   224/247  0.25 / 79     0.02 / 12     0.00 / 15   
100              598       0      598   282/424  0.48 / 85     0.03 / 13     0.00 / 15   
idle after         -       -        -         -  0.01 / 85     0.01 / 14     0.00 / 15   

== S10: latency of a non-streamed request to a backend that answers at once (ms)
   600 requests per row over kept-alive connections, in 3 rounds; CPU is per request
clients  variant          p50     p95     p99    mean  failed  apisix ms  stub ms   
1        direct          1.38    3.29    4.80    1.61       0  1.24       0.62      
1        noauth          1.61    3.85    5.78    2.03       0  1.47       0.64      
1        full            1.87    5.64    8.34    2.55       0  1.79       0.90      
1        p50 added by the internal hop: 0.23 ms, by forward-auth: 0.26 ms
10       direct         12.08   25.55   32.54   13.02       0  1.10       0.56      
10       noauth         14.86   33.93   42.86   16.86       0  1.47       0.58      
10       full           15.43   30.88   42.09   17.24       0  1.49       0.81      
10       p50 added by the internal hop: 2.77 ms, by forward-auth: 0.57 ms

all requests succeeded, and every stream has its usage event
```
