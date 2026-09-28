# aisa — AI Service [Access, Admin, Authority]

> **Status: design phase.** The documents describe the planned architecture. No code yet.

Pick the A that fits your day:
- **Access**: decides who may use which model, through a decision API that your gateway asks before every request.
- **Admin**: quotas, money budgets, prices and model backends, managed in one place.
- **Authority**: the single source of truth for usage. It keeps the books, and your gateway just proxies.

**aisa** is a governance layer for LLM traffic on HashiCorp-based infrastructure. It sits beside the AI gateway you already run and adds:

- **identities and provider keys from Vault**, and **backends, prices and budgets from Consul**
- **token quotas and money budgets** per client, with optional downgrade to a local model when a budget runs out
- **normalized usage metrics** per client and model, with a Grafana dashboard and alert rules
- **adapters** for existing AI gateways. [Apache APISIX](https://apisix.apache.org/) is the reference adapter.

The name is Finnish: an *aisa* is the shaft that hitches a cart to the horse pulling it, and *pitää aisoissa* means "to keep in check". aisa connects your applications to models and keeps their use in check.

## Why

Proxying requests and translating between providers are solved problems. What is missing is governance that:
- is **native to Vault and Consul**. LiteLLM and Bifrost only offer Vault in their paid tiers, and none of the open source gateways integrates with Consul.
- is **not tied to one gateway**. In existing gateways, budgets and usage accounting live inside the gateway itself.

aisa is not a gateway. Your gateway keeps proxying, streaming and translating between providers; aisa decides who may use what and accounts for it.

## How it fits together

```
            ┌──────────────── aisa ────────────────┐
 Vault ────▶│ identities · provider keys            │
 Consul ───▶│ backends · prices · budgets           │──▶ /metrics (aisa_*)
            │ decision API · usage ledger · render  │
            └────▲─────────────▲────────────┬──────┘
      1. decide  │  2. usage   │  3. config │
            ┌────┴─────────────┴────────────▼──────┐
 clients ──▶│ AI gateway (APISIX, …) via an adapter │──▶ Ollama / vLLM / cloud APIs
            └───────────────────────────────────────┘
```

Three contracts connect aisa to a gateway: a **decision API** (forward-auth pattern), **usage events** (access log sink) and **config rendering** (consul-template). See [docs/interfaces.md](docs/interfaces.md).

## Documents

| Document | Contents |
|---|---|
| [interfaces.md](docs/interfaces.md) | aisa ↔ gateway contracts and the adapter checklist |
| [architecture.md](docs/architecture.md) | Components, traffic path, failure modes, example deployment |
| [vault.md](docs/vault.md) | Identities and provider keys from Vault |
| [consul.md](docs/consul.md) | Backends, prices and budgets in Consul |
| [budgets.md](docs/budgets.md) | Quotas and money budgets |
| [observability.md](docs/observability.md) | Metrics, dashboard and alerts |
| [spikes.md](docs/spikes.md) | Unknowns to verify before implementation |
| [roadmap.md](docs/roadmap.md) | Phases, adapters and upstream contributions |

## License

[Apache License 2.0](LICENSE)
