# APISIX proxy for aisa

[Apache APISIX](https://apisix.apache.org/) (3.18+) as the proxy of aisa: the first and the reference one.

The proxy operates APISIX in **standalone mode** (without etcd) using a two-hop route architecture and a [consul-template](https://github.com/hashicorp/consul-template) sidecar.

---

## Architecture & Contracts

The proxy implements the three contracts of [`PROXY_CONTRACT.md`](../../docs/concepts/PROXY_CONTRACT.md):

```
Client ──▶ [Port 9080: client route]
                 │
                 ├── (1) forward-auth ──▶ aisa /v1/decide
                 │
                 └── (2-hop) ──▶ [Port 9081: internal listener]
                                       │
                                       ├── model-<name> ──────────▶ Backend (e.g. Ollama, OpenAI)
                                       ├── fail-open-<name> ──────▶ Fail-open Backend
                                       ├── aisa-unreachable (503)
                                       └── no-backend (503)
                                       │
                                       └── (2) http-logger ───────▶ aisa /v1/usage
```

### 1. Decision API (`POST /v1/decide`)

* The client-facing route (`server_port: 9080`) executes a `serverless-pre-function`:
  * **Strips client-supplied `X-Aisa-*` headers** (Rule 1) so clients cannot spoof consumer identities or models.
  * Extracts the model from the JSON request body and sets `X-Aisa-Requested-Model` (Rule 2).
  * Injects `X-Aisa-Request-Id: $apisix_request_id` to correlate decision and usage events.
* `forward-auth` queries aisa `/v1/decide` with the client's `Authorization` and `X-Aisa-Requested-Model`.
* On decision allow, forward-auth sets `X-Aisa-Consumer` and `X-Aisa-Model`, and forwards to the internal listener (`127.0.0.1:9081`).
* Denials (400, 401, 413, 429, 503) pass directly to the client with `Retry-After` when set (Rule 4).

### 2. Usage Events (`POST /v1/usage`)

* Both the client route (for denied/unroutable requests) and internal model routes attach `http-logger` sending to aisa `/v1/usage`.
* **Every request is reported once.** A request that forward-auth lets through (aisa allowed it, or could not be reached) is reported by the internal route that serves it; the client route reports only a request that ends there because aisa denied it. A `serverless-post-function` sets the variable `aisa_forwarded` when forward-auth lets a request through, and the client route's `http-logger` filters on it. Its `_meta.priority` of 1000 runs it between forward-auth (2002) and `http-logger` (410), because APISIX evaluates and keeps the filter when `http-logger`'s access handler runs. `upstream_addr` cannot be used: it is still empty then. [`runtime_test.sh`](./runtime_test.sh) checks each case against a running APISIX.
* **Explicit format only ([#5](https://github.com/hlan-net/aisa/issues/5)):** The proxy explicitly lists only the Contract 2 fields (`request_id`, `consumer`, `model`, `prompt_tokens`, `completion_tokens`, `status`, `latency_ms`, `ttft_ms`, `stream`). Client request headers (`Authorization`) are never included in the log sink payload.
* The internal route strips `Authorization` before proxying to local backends, so plaintext consumer keys never reach backends without provider keys.

### 3. Config Rendering & Guarded Promotion ([#18](https://github.com/hlan-net/aisa/issues/18))

* `consul-template` renders `apisix.yaml.ctmpl` from the Consul catalog (`service "aisa-backend"`) and Vault provider secrets (`secret/data/aisa/providers/*`).
* **Guarded Promotion (`guard.sh`):**
  1. `consul-template` renders into a staging file (`/rendered/apisix.yaml.staged`).
  2. The guard script validates that:
     * The file is non-empty and terminates with the `#END` marker (verifying complete write).
     * The `routes:` block is present and contains required routes (`id: client`, `id: no-backend`).
     * Route IDs are unique.
  3. If validation succeeds, `guard.sh` keeps a copy of the current file and atomically moves (`mv -f`) the staged file to `/rendered/apisix.yaml`.
  4. **APISIX validates the routes against its own schemas** when it loads the file, and leaves out a route that fails, with the error in its log only. So the guard then asks APISIX's Control API (`127.0.0.1:9090`, reachable only inside the pod) for every route of the new file, and waits up to `GUARD_VERIFY_TIMEOUT` (10 s) until each is loaded from the new file: the Control API reports the modification time of the file a route came from as its `modifiedIndex`. If a route is missing, the guard puts the previous file back and fails, naming the routes. Until then, for at most that time, APISIX serves the new file without them.
  5. If validation fails, `guard.sh` aborts and APISIX continues serving the last known good configuration without dropping routes or returning 404 on the client endpoint.
  * Not checked against APISIX: the first render, by the init container before APISIX starts, and a render while the Control API does not answer (APISIX restarting); then only the checks of step 2 apply, with a warning in the log.
* **Metadata validation:** Backends with missing `models` or unsupported `provider` are skipped with descriptive comments in the rendered YAML.

---

## Fail-Open and Fail-Closed Routing ([#4](https://github.com/hlan-net/aisa/issues/4))

When aisa is unreachable (connection error or timeout):

1. `forward-auth` has `allow_degradation: true`, permitting the request to continue to the internal listener.
2. Because forward-auth degraded, `X-Aisa-Model` and `X-Aisa-Consumer` are **not** set; only `X-Aisa-Requested-Model` exists.
3. For models with `fail_policy = "open"` backends (e.g. local Ollama), a dedicated fail-open route matches `X-Aisa-Requested-Model` and proxies only to open instances. Usage is logged with an empty consumer, which aisa accounts under `unknown`.
4. For models without fail-open backends (paid cloud providers), no fail-open route exists; the request falls through to `aisa-unreachable`, which answers `503 Service Unavailable` with the error type `aisa_unreachable`.
5. Client-supplied `X-Aisa-Model` headers are stripped in the pre-step, preventing unauthenticated access to paid models during outages.
6. The usage event of such a request is sent by the internal route only. The client route reports a request only when it ends there (aisa denied it), so a degraded request is not reported twice under the same `request_id`.

---

## Upstream Timeouts & Streaming ([#14](https://github.com/hlan-net/aisa/issues/14))

* Backends may specify `timeout` in seconds in their Consul service metadata.
* `apisix.yaml.ctmpl` renders `timeout` (in milliseconds) on `ai-proxy-multi`:
  * Default: `300` s (300 000 ms) when omitted.
  * Maximum: `600` s (600 000 ms), the schema maximum of APISIX's `ai-proxy-multi` plugin.
* The internal listener hop sets `timeout: {connect: 5, send: 600, read: 600}` so slow generation is not cut off by nginx's default 60 s read timeout.
* **10-minute ceiling:** APISIX's `ai-proxy-multi` enforces a maximum timeout of 600 s. Clients requesting generations that may exceed 10 minutes must stream (`stream: true`); in streaming mode, the timeout bounds the wait between tokens rather than the total duration of the stream.

---

## Deployment

The proxy is deployed with plain manifests, not with the official APISIX chart: the chart has no place for the sidecar that renders the config (spike S5).

```bash
kubectl apply -k proxies/apisix        # into the namespace aisa
```

`kustomization.yaml` generates the ConfigMaps from the files in this directory (`config.yaml`, `apisix.yaml.ctmpl`, `consul-template.hcl`, `guard.sh`, `vault-agent.hcl`), so a cluster runs what the tests run, and a changed file rolls the pod. [`manifests/proxy.yaml`](./manifests/proxy.yaml) holds the service account, the Deployment and the Service `apisix-gateway`.

One pod, three containers:

| Container | Role |
|---|---|
| `vault-agent` | Logs in to Vault with the pod's service account (Kubernetes auth, role `aisa-render`) and keeps two tokens in a directory in memory: the Vault token and a Consul ACL token from `consul/creds/aisa-render`. Starts first, as a sidecar init container (Kubernetes 1.29+). |
| `consul-template` | Renders `apisix.yaml` with those tokens. An init container renders once before APISIX starts, because APISIX does not start without the file. |
| `apisix` | Reads the rendered file from a second directory in memory. It has no token and no access to Vault or Consul. |

Settings for an environment go into an overlay, not into these files:

* **Addresses**: the ConfigMap `apisix-env` (`CONSUL_HTTP_ADDR`, `VAULT_ADDR`, `AISA_DECIDE_URI`, `AISA_USAGE_URI`).
* **Vault names**: the auth mount, the role and the Consul secrets mount are in [`vault-agent.hcl`](./vault-agent.hcl); they must match the Terraform module.
* **A Consul without ACLs**: remove the `template` block from `vault-agent.hcl`, the `-consul-token-file` arguments and the second half of the startup probe.

### Tests

Run against the dev stack (`docker compose -f dev/compose.yaml up -d --build --wait`); CI runs all three:

* [`guard_test.sh`](./guard_test.sh): what `guard.sh` promotes and what it rejects, with a fake Control API.
* [`render_test.sh`](./render_test.sh): renders the template with consul-template against the dev Consul and Vault and checks the routes, also for model names that need an escaped or shortened route ID.
* [`runtime_test.sh`](./runtime_test.sh): runs APISIX on the rendered config with the stub aisa, and checks that a request aisa allows, denies, or cannot be asked about is reported to the usage sink exactly once, by the right route. It also runs `guard.sh` next to that APISIX: a file with a route APISIX's schema rejects is put back, and a valid one is kept.

Not verified yet: the login with Vault's Kubernetes auth and the Consul token from Vault need a cluster and were not part of spike S5. In particular, whether consul-template picks up a Consul token that the Vault Agent replaces when its lease ends has not been tested; until it has, give the Consul role a long lease or restart the pod within it.

### Metrics

The rendered config enables APISIX's `prometheus` plugin for every route with a global rule; without it the metrics port answers but reports no requests. The Service exposes the port as `metrics` (9091, path `/apisix/prometheus/metrics`). For the Prometheus Operator, apply [`manifests/servicemonitor.yaml`](./manifests/servicemonitor.yaml) as well. These metrics (`apisix_llm_*`, `apisix_http_status`) are the cross-check of aisa's ledger, never its source.

### Resource footprint ([#16](https://github.com/hlan-net/aisa/issues/16))

Measured on arm64 (spikes S7 on a Raspberry Pi 5 and S5 on a Raspberry Pi 4); the confirmation on a Raspberry Pi 4 with the real aisa is still open.

* **One worker:** `nginx_config.worker_processes: 1` in `config.yaml`. APISIX otherwise starts a worker per core of the node.
* **APISIX:** about 85 MiB idle and 115 MiB under 100 concurrent streams; requests `100m` / `96Mi`, memory limit `256Mi`.
* **consul-template:** requests `10m` / `32Mi`, memory limit `64Mi`.
* **Vault Agent:** requests `10m` / `64Mi`, memory limit `128Mi`. These are a starting point and not measured.
* **Memory volumes:** `8Mi` for the rendered config, `1Mi` for the tokens.
