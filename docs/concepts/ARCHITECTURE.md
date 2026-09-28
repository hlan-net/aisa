# Concept: Architecture

aisa is split into a gateway-agnostic **core** and a replaceable **AI gateway** connected through an adapter. This document describes the reference deployment: the core with the APISIX adapter on Kubernetes, with Vault and Consul already in place.

Related: [`ADAPTER_CONTRACT.md`](./ADAPTER_CONTRACT.md), [`VAULT.md`](./VAULT.md), [`CONSUL.md`](./CONSUL.md), [`../features/quotas-and-budgets.md`](../features/quotas-and-budgets.md), [`../features/usage-metrics.md`](../features/usage-metrics.md).

## Traffic path

```
clients (chat UIs, batch jobs, CLI tools)
   │  Authorization: Bearer <consumer key>
   ▼
cluster ingress  (internal only, see Exposure)
   ▼
APISIX  (standalone mode)                                        ┌──────────────┐
   ├─ pre-step: strip X-Aisa-*, copy body model to a header      │              │
   ├─ forward-auth  ── /v1/decide ──────────────────────────────▶│    aisa      │
   │                 ◀─ X-Aisa-Consumer / X-Aisa-Model / 401/429 │              │
   └─▶ internal listener (127.0.0.1), one route per X-Aisa-Model │              │
         ├─ ai-proxy-multi → the backends of that model          │              │
         └─ http-logger   ── usage event ───────────────────────▶│ ledger       │
   ▼                                                             └──────┬───────┘
backends                                                                │ /metrics aisa_*
   ├─ Ollama / vLLM hosts (inside or outside the cluster)               ▼
   └─ cloud APIs (keys rendered from Vault)                      Prometheus → Grafana
```

## Components

| Component | Notes |
|---|---|
| **aisa** | Go, a single static binary (amd64 and arm64). Decision API, usage ingestion, budgets and metrics. Reads Vault (Kubernetes auth) and Consul (ACL token from Vault's Consul secrets engine). One replica to start. |
| Redis | Hot counters for quotas and budgets. Monthly totals are also persisted to Consul KV once a minute, so they survive a Redis loss with at most a minute of drift. |
| **APISIX adapter** | `apache/apisix` 3.18+ in standalone mode. Routes and plugins come from the rendered `apisix.yaml`. No etcd, no Admin API writes, and no Vault or Consul access of its own. Routing by `X-Aisa-Model` needs a second hop inside APISIX, because routes are matched before `forward-auth` runs (spike S8). |
| consul-template | Sidecar next to the gateway. Renders the gateway config from the Consul catalog (backends) and Vault (provider keys, KV v2) and triggers the reload. |
| ServiceMonitors | aisa's `/metrics` (primary) and the gateway's own metrics (cross-check). |

## Code layout (planned)

```
aisa/
├── cmd/aisa/                 # main
├── internal/                 # decide, ledger, budget, pricing, vault, consul, metrics
├── adapters/
│   └── apisix/
│       ├── apisix.yaml.ctmpl # consul-template template
│       ├── helm-values.yaml  # standalone mode
│       └── README.md
├── deploy/helm/              # chart for aisa (+ Redis)
├── terraform/                # module: Vault mount/policies/roles, Consul ACLs/KV/registrations
└── dashboards/               # Grafana JSON + alert rules
```

## Exposure

Keep the gateway endpoint internal: behind an IP allowlist or on an internal-only ingress. A leaked consumer key on a public endpoint means unmetered use of paid providers until the key is revoked. aisa's decision and ingest endpoints are cluster-internal only (ClusterIP plus a NetworkPolicy that allows only the gateway pods).

## Failure modes

| Failure | Behaviour |
|---|---|
| aisa down | Per-consumer fail policy: `closed` for paid providers (the request is rejected with 503), `open` for local models (allowed; usage is recorded later if the log sink retries). The gateway cannot identify the consumer without aisa, so how to apply this is still open; until then adapters fail closed (spike S8). |
| Redis down | aisa falls back to the persisted monthly totals in Consul KV. Quotas are enforced approximately, budgets still work. |
| A backend down | Consul health check → consul-template re-renders without it → the gateway falls back to another instance |
| Vault down | aisa keeps its cached identities and keys until their TTL expires, then fails closed |

## Non-goals (for now)

- Prompt caching, guardrails, PII masking. Gateways already have plugins for these, and they stay gateway-side features.
- Multi-replica HA for aisa itself.
- End-user management. Consumers are applications or people with API keys, not users of a chat UI.
