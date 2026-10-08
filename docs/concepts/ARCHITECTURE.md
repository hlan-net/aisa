# Concept: Architecture

aisa offers inference to the applications of a Kubernetes cluster as a service of the cluster itself, while the models run somewhere else. This document says what aisa is, what it consists of and how the reference deployment looks: aisa with APISIX as its proxy on Kubernetes, with Vault and Consul already in place.

Who it is for is in [`../process/USER_STORIES.md`](../process/USER_STORIES.md).

Related: [`PROXY_CONTRACT.md`](./PROXY_CONTRACT.md), [`VAULT.md`](./VAULT.md), [`CONSUL.md`](./CONSUL.md), [`../features/quotas-and-budgets.md`](../features/quotas-and-budgets.md), [`../features/usage-metrics.md`](../features/usage-metrics.md).

## What aisa is

Three roles, and aisa is the one in the middle:

```
 1. CONSUMER              3. AISA                          2. PROVIDER
 ┌────────────┐      ┌───────────────────────┐      ┌───────────────────┐
 │ application│ ───▶ │ a Kubernetes-native   │ ───▶ │ solves the        │
 │ in the     │      │ inference service     │      │ inference         │
 │ cluster    │ ◀─── │                       │ ◀─── │ (Ollama, a cloud) │
 └────────────┘      │ solves nothing itself │      └───────────────────┘
  its own identity   └───────────────────────┘       the holder's account
```

1. A **consumer** is an application that sends inference requests.
2. A **provider** is a service that answers them.
3. **aisa** gives the cluster a Kubernetes-native inference service for the consumers. It solves no request itself: it decides whether a request may pass, passes it to a provider with the holder's credentials, and keeps the books. A consumer manages no provider key and no provider login; aisa maintains them and follows their use.

Everything below is the inside of the third box.

### Parts and terms

| Term | Meaning | How many |
|---|---|---|
| **aisa** | The control side: decides, accounts, manages. A Go binary. It never carries model traffic. | one |
| **proxy** | The data side: carries the requests and answers between consumers and providers. The muscle comes from outside, an existing AI gateway (APISIX first), because proxying, streaming and its performance are solved problems that aisa does not re-solve. | one |
| **adapter** | What is specific to one kind of provider: how to authenticate to it, where its credential comes from, what it reports as usage, which headers carry its account. Today there is one, `openai-compatible`, selected by a backend's `provider`. | one per kind of provider |
| **holder** | The person who holds the contracts with the providers and operates aisa. | |

The proxy is aisa's own part, not somebody else's system: **aisa installs and removes it**, and decides its configuration. At run time it works as independently of aisa as it can: it renders its own configuration and keeps serving `fail_policy = "open"` backends while aisa is down. Seen from the cluster it is the egress of inference traffic, so it must be the only way from an application to a provider ([Exposure](#exposure)).

A gateway that already exists and belongs to someone else can still be connected through the same contracts, but aisa is not designed around that case first: no user story asks for it yet.

> **On the words.** "Gateway" in these documents is the product that serves as the proxy, such as APISIX. What makes a gateway work as aisa's proxy lives in `proxies/<name>/`, and the contract between aisa and its proxy is [`PROXY_CONTRACT.md`](./PROXY_CONTRACT.md). Until 2026-10 these were called adapters (`adapters/apisix/`, `ADAPTER_CONTRACT.md`); the spike files under `proxies/apisix/spikes/` and the released entries of `CHANGELOG.md` keep that word. "Adapter" now means only the provider-specific part of the table above.

### Where things are written, and what aisa makes of them

```
 holder ──▶ aisa's admin ──▶ Consul  everything that is not a secret ─┐
            (or directly)──▶ Vault   the secrets ─────────────────────┴─▶ aisa ──▶ Kubernetes objects ──▶ consumers
```

- **Consul holds everything except secrets; Vault holds the secrets.** They are the only sources of truth. aisa has no database of its own.
- **Kubernetes objects are aisa's output, never its input.** aisa creates them from Consul and Vault, and restores one that is changed by hand. They are modelled on KubeAI's `Model`, in aisa's own API group ([`KUBERNETES_OBJECTS.md`](./KUBERNETES_OBJECTS.md)): a consumer finds the models on offer with a query to the Kubernetes API and maps its tasks to them.
- **The holder need not use Consul or Vault directly.** aisa's admin interface sits in between; Vault can authenticate the holder. Direct use, and Terraform, stay possible, because the truth is in Consul and Vault either way.

### State of this direction

The sections after this one describe what is designed and, where the status says so, built. The direction above adds the following, decided on 2026-10-01:

| Part | State |
|---|---|
| Decision API, usage ledger, token quotas | Built |
| The proxy rendering its own configuration (consul-template next to APISIX) | Built (`ROADMAP.md` v0.2.0 PR 5) |
| aisa installing and removing the proxy with its own chart | Built: aisa's chart depends on the proxy's ([Packaging](#packaging-and-deployment)) |
| `Model` objects for consumers | Designed, not built ([`KUBERNETES_OBJECTS.md`](./KUBERNETES_OBJECTS.md#model)); `ROADMAP.md` v0.2.0 PR 12 |
| A consumer's credential as a Kubernetes object | Designed, not built: a Secret with a key aisa generates ([`KUBERNETES_OBJECTS.md`](./KUBERNETES_OBJECTS.md#the-consumers-secret), [#32](https://github.com/hlan-net/aisa/issues/32)) |
| The non-secret part of a consumer (name, quota profile) in Consul instead of Vault | Decided, follows from the rule above; `ROADMAP.md` v0.2.0 PR 11. Today the whole consumer is in Vault ([`VAULT.md`](./VAULT.md)) |
| Admin interface | Decided, not designed (US-3) |
| Provider adapters beyond `openai-compatible`; a provider key as a reference to any Vault path, so that secrets Vault maintains itself can be used | Decided, not designed |
| No way around the proxy | Decided, not designed |

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
| **APISIX proxy** | `apache/apisix` 3.18+ in standalone mode. Routes and plugins come from the rendered `apisix.yaml`. No etcd, no Admin API writes, and no Vault or Consul access of its own. Routing by `X-Aisa-Model` needs a second hop inside APISIX, because routes are matched before `forward-auth` runs (spike S8). |
| consul-template | Sidecar next to the gateway. Renders the gateway config from the Consul catalog (backends) and Vault (provider keys, KV v2) and triggers the reload. |
| Vault Agent | Second sidecar next to the gateway. Logs in to Vault with the pod's service account and hands consul-template its Vault token and its Consul ACL token; consul-template cannot log in by itself. |
| ServiceMonitors | aisa's `/metrics` (primary) and the gateway's own metrics (cross-check). |

## Code layout (planned)

```
aisa/
├── cmd/aisa/                 # main
├── internal/                 # decide, ledger, budget, pricing, vault, consul, metrics
├── proxies/
│   └── apisix/
│       ├── apisix.yaml.ctmpl # consul-template template
│       ├── config.yaml       # standalone mode
│       ├── chart/            # the proxy's chart (spike S5); aisa's chart depends on it
│       └── README.md
├── deploy/helm/aisa/         # chart for aisa (+ Redis, + the proxy's chart)
├── deploy/helm/test/         # chart tests and the kind install test
├── terraform/                # module: Vault mount/policies/roles, Consul ACLs/KV/registrations
└── dashboards/               # Grafana JSON + alert rules
```

## Packaging and deployment

aisa is released as two artifacts, both on GitHub Container Registry:

| Artifact | Where | Built by |
|---|---|---|
| Container image | `ghcr.io/hlan-net/aisa`, multi-arch (linux/amd64 and linux/arm64) | `.github/workflows/release.yml` on `v*` tags (`<version>` and `latest`) |
| Helm charts | `oci://ghcr.io/hlan-net/charts/aisa` (aisa, Redis and the proxy) and `oci://ghcr.io/hlan-net/charts/aisa-proxy-apisix` (the proxy alone), pushed with `helm push` | The same release workflow; each chart's `version` is the release without the `v`, its `appVersion` the tag |

A cluster installs aisa from the OCI chart with its own values, from a separate deployment repository or a GitOps tool:

```bash
helm install aisa oci://ghcr.io/hlan-net/charts/aisa --version <x.y.z> \
  --namespace aisa --create-namespace -f values.yaml
```

- **Own namespace.** aisa, the gateway and Redis run together in a dedicated namespace (`aisa` by default), not in `kube-system`. Vault's Kubernetes auth binds roles to a namespace and service account, so aisa's Vault access stays separate from other workloads, and NetworkPolicies, resource quotas and upgrades apply to aisa alone.
- **Shared service.** Applications in other namespaces use the gateway's Service as an OpenAI-compatible endpoint (e.g. `http://<gateway-service>.aisa.svc.cluster.local/v1`); clients outside the cluster come in through an internal ingress (see [Exposure](#exposure)). A namespace is not an identity: each application authenticates with its own consumer credential ([`VAULT.md`](./VAULT.md)).
- **Proxy.** aisa's chart installs and removes the proxy: each proxy has a chart of its own in `proxies/<name>/chart`, and aisa's chart depends on it (`apisix.enabled`), so `deploy/helm/aisa` names no gateway beyond that dependency. For APISIX it is not the upstream chart, which has no place for the sidecar that renders the config (spike S5).
- **Prerequisites.** The Vault roles and policies of [`VAULT.md`](./VAULT.md) and the Consul policies of [`CONSUL.md`](./CONSUL.md#acls) exist before the install; the Terraform module (ROADMAP v0.2.0 PR 6) is to create them. `deploy/helm/test/install_test.sh` sets them up by hand for a test cluster.

## Exposure

Keep the gateway endpoint internal: behind an IP allowlist or on an internal-only ingress. A leaked consumer key on a public endpoint means unmetered use of paid providers until the key is revoked. aisa's decision and ingest endpoints are cluster-internal only (ClusterIP plus a NetworkPolicy that allows only the gateway pods). aisa serves `/metrics` on a port of its own (`AISA_METRICS_ADDR`; 9090 in the chart), so a scraper such as Prometheus is allowed that port only: `/v1/usage` takes usage events without authentication, and a peer that could reach it could charge consumers' quotas ([#46](https://github.com/hlan-net/aisa/issues/46)).

The proxy is the egress of inference traffic, and a quota holds only if nothing goes around it. A paid provider is safe without further measures, because no application has its key. A backend without a key is not: an application that knows the address of a local Ollama can call it directly, past the quotas and the books. That path must be closed in the network, with a NetworkPolicy that denies applications the backends or a firewall on the backend's host. This is not designed yet.

## Failure modes

| Failure | Behaviour |
|---|---|
| aisa down | Per-backend fail policy (Consul service meta `fail_policy`): a request is served by the requested model's `open` backends, typically local models, and rejected with 503 when the model has none, as for paid providers. The consumer plays no part, because the gateway cannot identify it without aisa. Usage is recorded under the consumer `unknown` if the log sink delivers it ([#4](https://github.com/hlan-net/aisa/issues/4)). |
| Redis down | Token quotas are not enforced: aisa allows the requests of consumers with a quota and counts the failed reads in `aisa_quota_errors_total`, and tokens used meanwhile are not counted towards the window. The credential is still checked; a quota only limits the rate. Budgets fall back to the persisted monthly totals in Consul KV (v0.3.0). |
| Consul down | aisa keeps the quota profiles it has read and follows changes again when Consul is back. A consumer whose profile is missing or broken gets 503 (fail closed), so a typo does not lift a limit unnoticed. |
| A backend down | Consul health check → consul-template re-renders without it → the gateway falls back to another instance |
| Vault down | aisa keeps deciding with its cached consumers for up to 15 min (`AISA_CONSUMER_MAX_STALE`), then fails closed: `/v1/decide` answers 503. Keys created meanwhile are not accepted until Vault is back ([`VAULT.md`](./VAULT.md#consumer-credentials)) |

## Non-goals (for now)

- Proxying in aisa itself. The proxy is an existing AI gateway; aisa decides and accounts.
- Prompt caching, guardrails, PII masking. Gateways already have plugins for these, and they stay gateway-side features.
- Multi-replica HA for aisa itself.
- End-user management. Consumers are applications or people with API keys, not users of a chat UI.
