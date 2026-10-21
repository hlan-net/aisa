# Spike S11: a backend that is gone

Question ([#52](https://github.com/hlan-net/aisa/issues/52)): APISIX's `ai-proxy-multi` has one
`timeout` for connecting and for reading, so a request to a backend that has just gone away waits
as long as a slow model may take. Which of the plugin's options shorten that wait, and for which
kinds of "gone"?

- `fallback_strategy: [http_5xx]`: after a 5xx, which a connection error or a timeout becomes
  inside the plugin (500 and 504), the request is sent to another instance.
- `retry_on_failure_within_ms`: that retry only when the failure came within the time, so a slow
  failure is not followed by a second wait.
- `checks.active` on each instance: active health checks, so the picker skips an instance that
  is down.

## How

`apisix.yaml` has a route per combination of a kind of "gone" and an option. Every route has two
instances of the same model: `ok` (the dev stack's `mock-local`) and `gone`, which is one of:

| Kind | Endpoint | What a client sees |
|---|---|---|
| `unroutable` | `192.0.2.1:8080` (TEST-NET-1): no host answers the SYN | the connect hangs until the timeout |
| `refused` | `127.0.0.1:9` inside the gateway: nothing listens | connection refused at once |
| `hung` | `mock-cloud`, paused by `run.sh`: the kernel completes the handshake, the process never reads | the request hangs until the timeout |

`timeout` is 8 s on every route, not the template's 300 s, so that a run takes minutes. The
health checks probe `/healthz` every second with a 1 s timeout and mark an instance unhealthy
after two failures. `run.sh` sends the requests one after another and prints the status and the
seconds of each; `compose.override.yaml` gives APISIX these routes and frees the stub aisa's host
port, which the spike does not use.

```
./proxies/apisix/spikes/s11-gone-backend/run.sh            # 10 requests per route, both phases
./proxies/apisix/spikes/s11-gone-backend/run.sh 4 5xx      # 4 requests, the 5xx phase only
```

## Result, 2026-10-21

APISIX 3.19.0 (`apache/apisix:3.19.0-debian`, 4 nginx workers), the dev stack on arm64, 10
requests per route. The picker is per worker, so the requests do not alternate strictly
between the two instances.

```
unroutable/plain             5/10  ok   4 slow (>=2 s)  max   8.0 s  total   32.3 s   200:0s 504:8s 504:8s 200:0s 200:0s 504:8s 500:0s 504:8s 200:0s 200:0s
unroutable/retry            10/10  ok   8 slow (>=2 s)  max   8.1 s  total   60.7 s   200:8s 200:8s 200:4s 200:8s 200:0s 200:8s 200:8s 200:8s 200:8s 200:0s
unroutable/retry-fast        3/10  ok   7 slow (>=2 s)  max   8.0 s  total   56.2 s   504:8s 200:0s 504:8s 200:0s 504:8s 504:8s 200:0s 504:8s 504:8s 504:8s
unroutable/checks            6/10  ok   4 slow (>=2 s)  max   8.0 s  total   32.3 s   200:0s 504:8s 504:8s 504:8s 504:8s 200:0s 200:0s 200:0s 200:0s 200:0s
unroutable/single-checks     0/10  ok   9 slow (>=2 s)  max   8.0 s  total   72.0 s   500:0s 504:8s 504:8s 504:8s 504:8s 504:8s 504:8s 504:8s 504:8s 504:8s
refused/plain                5/10  ok   0 slow (>=2 s)  max   0.1 s  total    0.3 s   500:0s 500:0s 200:0s 200:0s 200:0s 200:0s 500:0s 500:0s 200:0s 500:0s
refused/retry               10/10  ok   0 slow (>=2 s)  max   0.1 s  total    0.5 s   200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s
refused/retry-fast          10/10  ok   0 slow (>=2 s)  max   0.1 s  total    0.5 s   200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s 200:0s
refused/checks               7/10  ok   0 slow (>=2 s)  max   0.1 s  total    0.4 s   200:0s 200:0s 500:0s 200:0s 500:0s 200:0s 200:0s 200:0s 200:0s 500:0s
hung/plain                   5/10  ok   5 slow (>=2 s)  max   8.0 s  total   40.3 s   200:0s 200:0s 504:8s 504:8s 200:0s 504:8s 200:0s 200:0s 504:8s 504:8s
hung/retry                  10/10  ok   7 slow (>=2 s)  max   8.1 s  total   56.5 s   200:0s 200:8s 200:8s 200:0s 200:0s 200:8s 200:8s 200:8s 200:8s 200:8s
hung/retry-fast              4/10  ok   6 slow (>=2 s)  max   8.0 s  total   48.2 s   504:8s 200:0s 504:8s 504:8s 200:0s 200:0s 504:8s 504:8s 200:0s 504:8s
hung/checks                  7/10  ok   3 slow (>=2 s)  max   8.0 s  total   24.4 s   504:8s 200:0s 504:8s 200:0s 200:0s 504:8s 200:0s 200:0s 200:0s 200:0s
```

Read with the gateway's log:

1. **One timeout covers the connect.** A request that picks the unroutable instance waits the
   whole `timeout` and gets 504 (`connect: timeout` in the log), the same as one that picks the
   hung instance. With the template's 300 s, that is 5 minutes per such request. Twice a request
   got 500 at once instead (`connect: No route to host`): the network of the spike answered the
   SYN with an ICMP error those times, which a host that is really gone does not.
2. **A retry (`fallback_strategy: [http_5xx]`) turns a failure into an answer, after the wait.**
   Every request succeeded, but those that picked the gone instance first took the timeout plus
   the answer (8.1 s here, 300 s in the template). Refused connections, which fail at once, were
   retried for free: 10 of 10 in 0.05 s.
3. **`retry_on_failure_within_ms` keeps the slow case from doubling, at the price of the retry:**
   a failure after the timeout is answered 504 as it is, only fast failures move to the other
   instance.
4. **Active checks stop new requests after a few seconds, and only with two or more
   instances.** The checker starts with the first request to the route and marked the gone
   instance unhealthy 2 s later (`unhealthy TIMEOUT increment (2/2)`, or `TCP` for refused); the
   next three or four requests still went to it, one per worker, then none did. With one
   instance the checks are not consulted at all: `unroutable/single-checks` is 0 of 10, the same
   as no checks. The plugin also routes across every instance when all are unhealthy.
5. **Nothing here shortens a request that is already waiting on a gone backend**, and nothing
   helps the common case of a single backend. Only Consul's health check, which takes the
   backend out of the rendered config (S9: 3 s with a check every 2 s, more than half a minute
   with the 30 s of `CONSUL.md`'s example), stops new requests there; the ones in flight wait
   the timeout. A separate connect timeout in the plugin is the fix, and it is upstream.

The `5xx` phase, a backend that answers 503, is in the spike's outcome in
[`SPIKES.md`](../../../../docs/process/SPIKES.md#s11-a-backend-that-is-gone).
