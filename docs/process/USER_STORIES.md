# User stories

The stories aisa is built for. A story says who benefits and what must be true for them; the design that delivers it is in [`docs/concepts/`](../concepts/) and the schedule in [`ROADMAP.md`](../../ROADMAP.md).

## US-1: Cost-safe inference that looks Kubernetes-native

> **As a** Kubernetes cluster operator,
> **I want to** offer an inference service that looks Kubernetes-native to the applications that use it, while the models run on services outside the cluster,
> **so that** applications use LLMs the way they use any other in-cluster service and the spend can never exceed what I have allowed.

### Scope of "Kubernetes-native"

Kubernetes-native describes the **applications that consume inference**, not the way aisa is operated.

- **Consumer side:** an application needs nothing but ordinary Kubernetes primitives. It has no knowledge of Vault, Consul, aisa or where the model runs.
- **Operator side:** identities, quotas, budgets and backends are managed in whatever way is most natural for aisa. Today that is Vault and Consul through Terraform. Kubernetes resources (CRDs, an operator) are not required.

### Acceptance criteria

1. **An ordinary Service.** An application reaches inference at an in-cluster `Service` DNS name with an OpenAI-compatible API.
2. **Ordinary credentials.** An application gets its credential through a Kubernetes primitive (a `Secret` or its service account), not by talking to Vault itself.
3. **External backends.** The models run outside the cluster (e.g. `gpu-box.internal`, a cloud provider). Applications cannot tell, and provider keys are never visible to them.
4. **Cost-safe.** Every application has an identity, a token quota and a budget, both in tokens; currency is how spend is shown, not how it is limited. Once the accounted usage has reached a limit, every further request is denied or downgraded to a local model, never silently passed on. A request's own tokens are known only after it, so the last request before the limit can overshoot it by that much.
5. **Fails safe.** When aisa is unreachable, backends that cost money fail closed. Only backends with `fail_policy = "open"` stay reachable.
6. **Visible.** The operator sees usage and cost per application from the `aisa_*` metrics.
7. **Explains itself.** The service tells an application developer how to use it: the endpoint, how to get a credential, which models are offered, and what a denied or downgraded request looks like. Existing applications are not assumed to know any Kubernetes-native inference service; most call a provider directly with a base URL and an API key, or a provider SDK with no base URL at all. The instructions cover moving such an application over, and no other source is needed.
8. **Models are found through Kubernetes.** The models on offer are objects in the cluster, in the manner of KubeAI's `Model`. An application lists them with a query to the Kubernetes API, by what they can do, and maps its own tasks to them. An application that does not talk to the Kubernetes API finds the same list at `/v1/models`.
9. **No way around.** An application reaches a model only through the service. It cannot call a backend directly, past the quota and the books.

### Gaps

| Criterion | State | Where |
|---|---|---|
| 2 | Done: aisa generates the key, writes it to a Secret in the application's namespace with the proxy's URL, and rotates it ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#the-consumers-secret)). | `ROADMAP.md` v0.2.0 PR 12, [#32](https://github.com/hlan-net/aisa/issues/32) |
| 4 | Done: token quotas, monthly budgets on priced models with the downgrade to a local model, a consumer without a quota profile denied unless the holder sets a default, and a budget per provider account above the consumers ([#33](https://github.com/hlan-net/aisa/issues/33), [`quotas-and-budgets.md`](../features/quotas-and-budgets.md)). | `ROADMAP.md` v0.3.0 |
| 5 | Done. Backends with `fail_policy = "open"` are served while aisa is unreachable; every other model answers 503 `aisa_unreachable`. | `ROADMAP.md` v0.2.0 PR 5 ([#39](https://github.com/hlan-net/aisa/pull/39)) |
| 6 | Done: token metrics (`aisa_tokens_total`), cost metrics (`aisa_cost_total`), consumer and provider budget gauges (`aisa_budget_*`, `aisa_provider_budget_*`), and the Grafana dashboard ([`dashboards/aisa.json`](../../dashboards/aisa.json)). | `ROADMAP.md` v0.2.0 PR 7, v0.3.0 |
| 7 | Done: [`client-integration.md`](../features/client-integration.md) tells the developer the endpoint, the Secret with the credential, how models are found, every denied answer and what to do about it, and that provider-native APIs are not offered; the chart's install notes point to it. The proxy serves `/v1/models`, `/v1/chat/completions` and `/v1/embeddings`. What a downgraded request looks like comes with budgets. | `ROADMAP.md` v0.2.0 PR 10, [#34](https://github.com/hlan-net/aisa/issues/34), v0.3.0 |
| 8 | Done: a `Model` object per model on offer, in the shape of KubeAI's, found by label selectors on its features and capabilities, readable by every service account. | [`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#model), `ROADMAP.md` v0.2.0 PR 12 |
| 9 | Open. A backend without a key, such as a local Ollama, answers anyone who knows its address. | Needs a design ([`ARCHITECTURE.md`](../concepts/ARCHITECTURE.md#exposure)) |

## US-2: The provider accounts stay mine

> **As the** holder of the contracts with the model providers, the one whose account pays for every cloud request,
> **I want** nothing about those accounts to reach the applications that use aisa,
> **so that** an application, or whoever controls one, can neither read my provider credentials nor use my accounts in any way other than the requests aisa decided.

This was implicit in the design from the start (provider keys in Vault, rendered into the gateway only). It is written down because the applications are not trusted with anything about the accounts, and a leak need not be the key itself.

### Acceptance criteria

1. **No credential reaches an application.** A provider key appears in no response, no error body and no header an application can see, and in no file or endpoint an application can read.
2. **Nothing about the account reaches an application.** Organisation and project ids, account-level rate-limit headers, billing or tier information and provider request ids are not passed through. A response carries what the model produced and what aisa adds, nothing a provider says about the account.
3. **An application cannot steer the account.** Request headers that select an organisation, a project or a provider feature under the operator's key are not forwarded. The only path to a provider is a model route that aisa decided; an application cannot name a backend or a provider.
4. **Nothing is served on a paid account without a decision.** A backend with a key is never reached while aisa is unreachable (`fail_policy` defaults to `closed`; an operator who sets `open` on a keyed backend does so knowingly).
5. **Credentials are in no log, event or metric.** Usage events, aisa's logs and the `aisa_*` labels carry no credential and nothing from a provider's account.

### Gaps

| Criterion | State | Where |
|---|---|---|
| 1, 2 | Met in the APISIX proxy: a client gets only the `Content-Type`, `Cache-Control`, `Retry-After` and framing of a provider's answer, and a provider's error body is replaced by the gateway's ([`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md#account-isolation)). The gateway's log gets the provider's status and the gateway's classification, nothing of the provider's text or codes. | [#35](https://github.com/hlan-net/aisa/issues/35) |
| 3 | Met in the APISIX proxy: a provider gets only the `Content-Type` and `Accept` of a client's request, nothing that selects an organisation or a project, and no `X-Aisa-*`. | [#35](https://github.com/hlan-net/aisa/issues/35) |
| 4 | Met by design ([`CONSUL.md`](../concepts/CONSUL.md#backends-consul-catalog)); the routes are in progress. | `ROADMAP.md` v0.2.0 PR 5 ([#4](https://github.com/hlan-net/aisa/issues/4)) |
| 5 | Met for credentials ([#5](https://github.com/hlan-net/aisa/issues/5)): usage events carry explicit fields only and aisa never logs a raw event. | — |

## US-3: The holder manages aisa through aisa

> **As the** holder, who operates aisa and holds the provider contracts,
> **I want to** manage consumers, models, limits and provider credentials in aisa's own admin interface,
> **so that** I work with the things I care about, not with keys and paths in Consul and Vault, and can still trace every credential to where it came from.

aisa stands between the holder and its sources of truth, as it stands between the consumers and the providers. Consul and Vault stay the truth: the admin interface writes there and keeps nothing of its own, so using them directly, or through Terraform, stays possible.

### Acceptance criteria

1. **One place.** The holder creates and changes consumers, quota profiles, prices, budgets, backends and provider credentials in aisa's admin interface, without opening Consul or Vault.
2. **Vault knows the holder.** The holder logs in through Vault, and who may change what is decided by Vault's policies, not by a list of users in aisa.
3. **A secret goes in and does not come out.** The holder enters a provider credential; the interface never shows its value again.
4. **A credential can be traced.** Next to the secret the holder records what identifies it at the provider and when it ends: for example the id of an Azure app registration's client secret and its expiry date, where it was created, and a note. aisa adds who changed it and when. When a provider announces that a secret is about to expire, the holder finds by its id which credential to renew.
5. **Expiry is not a surprise.** aisa warns before a credential expires, from the recorded date or from the lease of a secret that Vault maintains.
6. **Secrets that Vault maintains can be used.** A provider credential may be one that Vault issues and rotates itself. The interface then shows it as managed by Vault, with its validity, and asks for no value.

### Gaps

| Criterion | State | Where |
|---|---|---|
| 1 | In part. The admin interface (`/admin`, since v0.2.1) creates and removes consumers and quota profiles, rotates a consumer's key, and sets the consumers' monthly budgets ([#86](https://github.com/hlan-net/aisa/issues/86)); it lists backends and models. Open: prices, provider budgets, backends and provider credentials, which the holder still writes to Consul and Vault with Terraform and the Vault CLI ([`../features/admin-interface.md`](../features/admin-interface.md)). | [#133](https://github.com/hlan-net/aisa/issues/133) for the credentials; the rest has no issue yet |
| 2 | Open, and decided in part (2026-11-20). The admin interface has no login: whoever reaches `/admin` can do what it offers, so it is off by default and belongs behind an authenticating ingress. **Decided:** a write to Vault that needs more rights than aisa's own is made with the user's own Vault token, which the interface asks for, per session, when rights must be elevated. aisa's service account stays a reader, Vault's policies decide who may write, and Vault's audit log names the person. Open: how the token is held for a session, which actions need it, whether it also elevates the writes to Consul, and a login for the interface as a whole. | [#133](https://github.com/hlan-net/aisa/issues/133) |
| 3 | Open. aisa cannot write a provider credential at all: its Vault policy covers the consumers only, and it does not read `secret/aisa/providers/`. With the decision above, the holder enters the credential and the user's token writes it; the value is not shown, returned or logged again. | [#133](https://github.com/hlan-net/aisa/issues/133) |
| 4, 5 | Open. Proposed: the identifying data in the custom metadata of the KV v2 secret, which a policy can make readable without the secret's value. Nothing from a provider's account goes into a metric label (US-2). | Needs a design |
| 6 | In part. A backend can name a credential that Vault issues, by the path and the field it is read from (`key_path`, `key_field`; [#92](https://github.com/hlan-net/aisa/issues/92), [`CONSUL.md`](../concepts/CONSUL.md#backends-consul-catalog)), so a secret that Vault maintains can be used. Open: the admin interface does not show a credential as managed by Vault, with its validity; and a provider that needs a step from the issued credential to a token needs that in its adapter. | Needs a design |
