# Concept: Consul integration

Consul holds three things for aisa: **where the model backends are**, **what models cost** and **what each consumer may spend**. aisa reads them directly. Gateways get backends only through rendered config ([`ADAPTER_CONTRACT.md`](./ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration)).

Related: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`VAULT.md`](./VAULT.md), [`../features/quotas-and-budgets.md`](../features/quotas-and-budgets.md).

The rule is that **Consul holds everything that is not a secret**, and Vault the secrets ([`ARCHITECTURE.md`](./ARCHITECTURE.md#where-things-are-written-and-what-aisa-makes-of-them)). From the two, aisa creates the Kubernetes objects that consumers see; Consul is where the holder, or aisa's admin interface for the holder, writes.

## Backends: Consul catalog

Model backends are registered as Consul services. Many of them run outside Kubernetes (a workstation with a GPU, a Mac mini, a vLLM box), so the Terraform module registers them statically:

```hcl
resource "consul_service" "ollama_1" {
  name       = "aisa-backend"
  service_id = "ollama-1"   # unique in the catalog: the backend's name in usage events
  node = consul_node.gpu_box.name
  port = 11434
  tags = ["ollama", "local"]
  meta = {
    provider = "openai-compatible"
    models   = "qwen3,phi4-reasoning,qwen2.5-coder"
    priority = "1"
    fail_policy = "open"  # served without a decision while aisa is unreachable; "closed" by default
    # key    = "openai"   for a backend that needs an API key: the secret's name in Vault
    # scheme = "https"    http or https; https by default for a backend with a key, else http
    # timeout = "600"     seconds the gateway waits for the backend's next byte; 300 by default
  }
  check {
    name     = "ollama"
    check_id = "service:ollama-1"
    http     = "http://gpu-box.internal:11434/api/version"
    interval = "30s"
    timeout  = "5s"
  }
}
```

- All backends use one service name (`aisa-backend`), and **service meta** describes each one. The adapter template turns them into gateway routes (for APISIX, `ai-proxy-multi` instances grouped by model).
- Cloud providers are registered the same way (an external node such as `api.openai.com`), so every backend is discoverable in one place.
- Each backend has a **service ID that is unique in the whole catalog**, not only on its node: it is the backend's name in the rendered config and in usage events, where aisa looks up its provider and prices. The Terraform provider defaults the ID to the service name, which all backends share, so the module must set `service_id`.
- The meta `scheme` says how the gateway reaches a backend, `http` or `https`. It defaults to `https` for a backend with a key, so a provider key is not sent in the clear by accident, and to `http` for the others.
- The meta `fail_policy` says whether the backend may serve requests while aisa is unreachable: `open` backends serve them without a decision, `closed` backends (the default) do not. A request is answered with 503 only when its model has no `open` backend. It belongs to the backend and not to the consumer, because without aisa the gateway cannot tell who the consumer is. An `open` backend serves any credential in that time, including an unknown one, and its usage is charged to no quota or budget, so set it only where unmetered use costs nothing that matters, typically local models ([`ADAPTER_CONTRACT.md`](./ADAPTER_CONTRACT.md#adapter-rules-for-the-decision)).
- The meta `timeout` is how long, in seconds, the gateway waits for the backend's next byte: before the first token, between tokens, or for a whole non-streamed answer. It defaults to 300, and the adapter caps it at what its gateway accepts ([`ADAPTER_CONTRACT.md`](./ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration)). Slow local models need it raised.
- A backend that needs an API key names it with the meta `key`: the secret `secret/aisa/providers/<key>` in Vault ([`VAULT.md`](./VAULT.md#provider-keys)).
- A failed health check drops the backend from the rendered config, so a machine that is asleep or down leaves rotation automatically. Until then, requests sent to it fail. That window is the check's interval and timeout, plus the template's quiet period and the gateway's reload: with a 2 s interval it was about 3 s (spike S9). Requests already sent to the backend can wait until the gateway's timeout.

## Prices and budgets: Consul KV

```
aisa/pricing/<model>              {"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": "EUR"}
aisa/budgets/<consumer>           {"monthly": 10.00, "soft_ratio": 0.8, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}
aisa/quotas/<profile>             {"tokens_per_hour": 200000}
aisa/state/<consumer>/<YYYY-MM>   persisted monthly spend (written by aisa)
```

- A quota profile is a JSON object with `tokens_per_hour`, a non-negative integer; other fields are ignored. A profile without it, or one that is not valid JSON, is logged and counted in `aisa_quota_profiles_invalid`, and its consumers are denied with 503 until it is fixed ([`../features/quotas-and-budgets.md`](../features/quotas-and-budgets.md#token-quotas-v020)).
- Local models cost 0 by default. An energy-based price per token can be set from measured power use.
- **Single source of truth:** prices, budgets and quota profiles are Terraform variables. Terraform writes them to Consul KV, and aisa watches the prefix (blocking queries) and picks up changes without a restart.
- Hot counters live in Redis. Consul KV only gets the persisted monthly totals once a minute, because Consul's Raft log is not meant for a write per request.

## ACLs

Terraform-managed Consul policies. The tokens come from Vault's Consul secrets engine (`consul/creds/<role>`), so no static Consul token is deployed. aisa reads its token from the file `CONSUL_HTTP_TOKEN_FILE` for every request, so the Vault Agent can render and renew it there; `CONSUL_HTTP_TOKEN` is for development:

| Role | Rules |
|---|---|
| `aisa` | `key_prefix "aisa/" { policy = "write" }`, `service "aisa-backend" { policy = "read" }` |
| `aisa-render` | `service "aisa-backend" { policy = "read" }`, `node_prefix "" { policy = "read" }` |
