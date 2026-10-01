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
4. **Cost-safe.** Every application has an identity, a token quota and a money budget. A request that would exceed them is denied or downgraded to a local model, never silently passed on.
5. **Fails safe.** When aisa is unreachable, backends that cost money fail closed. Only backends with `fail_policy = "open"` stay reachable.
6. **Visible.** The operator sees usage and cost per application from the `aisa_*` metrics.
7. **Explains itself.** The service tells an application developer how to use it: the endpoint, how to get a credential, which models are offered, and what a denied or downgraded request looks like. Existing applications are not assumed to know any Kubernetes-native inference service; most call a provider directly with a base URL and an API key, or a provider SDK with no base URL at all. The instructions cover moving such an application over, and no other source is needed.

### Gaps

| Criterion | State | Where |
|---|---|---|
| 2 | Open. Vault holds only the hash of a consumer key ([`VAULT.md`](../concepts/VAULT.md)); how the key reaches the application's pod is not specified. The planned Vault-issued JWTs make pods log in to Vault, which is not Kubernetes-native for the application. | Needs a design decision |
| 4 | In part. Token quotas are enforced; money budgets and downgrade are not. | `ROADMAP.md` v0.3.0 |
| 5 | In progress. | `ROADMAP.md` v0.2.0 PR 5 ([#4](https://github.com/hlan-net/aisa/issues/4)) |
| 6 | In part. Token metrics exist; cost metrics and the dashboard do not. | `ROADMAP.md` v0.2.0 PR 7, v0.3.0 |
| 7 | Open. The docs describe aisa for the operator and the adapter author; nothing is written for the developer of a consuming application. Where the instructions live (a feature doc, the chart's install notes, an endpoint of the service) is undecided, and so is what is offered to an application that speaks a provider's own API instead of the OpenAI-compatible one. | Needs a design decision |
