# Concept: Consul integration

Consul holds three things for aisa: **where the model backends are**, **what models cost** and **what each consumer may spend**. aisa reads them directly. Gateways get backends only through rendered config ([`ADAPTER_CONTRACT.md`](./ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration)).

Related: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`VAULT.md`](./VAULT.md), [`../features/quotas-and-budgets.md`](../features/quotas-and-budgets.md).

## Backends: Consul catalog

Model backends are registered as Consul services. Many of them run outside Kubernetes (a workstation with a GPU, a Mac mini, a vLLM box), so the Terraform module registers them statically:

```hcl
resource "consul_service" "ollama_1" {
  name = "aisa-backend"
  node = consul_node.gpu_box.name
  port = 11434
  tags = ["ollama", "local"]
  meta = {
    provider = "openai-compatible"
    models   = "qwen3,phi4-reasoning,qwen2.5-coder"
    priority = "1"
    # key    = "openai"   for a backend that needs an API key: the secret's name in Vault
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
- A backend that needs an API key names it with the meta `key`: the secret `secret/aisa/providers/<key>` in Vault ([`VAULT.md`](./VAULT.md#provider-keys)).
- A failed health check drops the backend from the rendered config, so a machine that is asleep or down leaves rotation automatically. Until the check fails, requests sent to it fail, so the check's interval is the longest time that lasts (spike S9).

## Prices and budgets: Consul KV

```
aisa/pricing/<model>              {"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": "EUR"}
aisa/budgets/<consumer>           {"monthly": 10.00, "soft_ratio": 0.8, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}
aisa/quotas/<profile>             {"tokens_per_hour": 200000}
aisa/state/<consumer>/<YYYY-MM>   persisted monthly spend (written by aisa)
```

- Local models cost 0 by default. An energy-based price per token can be set from measured power use.
- **Single source of truth:** prices, budgets and quota profiles are Terraform variables. Terraform writes them to Consul KV, and aisa watches the prefix (blocking queries) and picks up changes without a restart.
- Hot counters live in Redis. Consul KV only gets the persisted monthly totals once a minute, because Consul's Raft log is not meant for a write per request.

## ACLs

Terraform-managed Consul policies. The tokens come from Vault's Consul secrets engine (`consul/creds/<role>`), so no static Consul token is deployed:

| Role | Rules |
|---|---|
| `aisa` | `key_prefix "aisa/" { policy = "write" }`, `service "aisa-backend" { policy = "read" }` |
| `aisa-render` | `service "aisa-backend" { policy = "read" }`, `node_prefix "" { policy = "read" }` |
