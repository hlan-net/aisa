# Concept: Kubernetes objects

> **Status: draft design.** Nothing here is built. It turns the direction of 2026-10-01 ([`ARCHITECTURE.md`](./ARCHITECTURE.md#state-of-this-direction)) into objects, and closes the design gaps of US-1 criteria 2 and 8 ([`../process/USER_STORIES.md`](../process/USER_STORIES.md#us-1-cost-safe-inference-that-looks-kubernetes-native)).

Related: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`VAULT.md`](./VAULT.md), [`CONSUL.md`](./CONSUL.md), [#32](https://github.com/hlan-net/aisa/issues/32), [#34](https://github.com/hlan-net/aisa/issues/34).

## The idea: inference used like storage

An application that needs storage does not manage disks, quotas or credentials. It uses a volume that the cluster gives it. aisa does the same for inference: an application in the cluster sees

- **a Service** with an OpenAI-compatible API,
- **a Secret** in its own namespace with the base URL and its key,
- **`Model` objects** that say which models are on offer and what they can do,

and nothing else. It manages no identity, no quota, no budget and no provider account; those are the holder's, in Consul and Vault ([`ARCHITECTURE.md`](./ARCHITECTURE.md#where-things-are-written-and-what-aisa-makes-of-them)).

## The rule

**Kubernetes objects are aisa's output, never its input.** aisa creates them from Consul and Vault, updates them when those change, deletes them when their source is gone, and restores one that is changed by hand. Nothing an application writes to the Kubernetes API changes what it may do.

This keeps one source of truth, and the holder's admin interface (US-3) and Terraform both work on it. The cost is that a consumer cannot be requested from the application's side: the holder adds it in Consul, and its Secret appears.

## Existing standards checked first

| Candidate | What it is | Why it is not used as is |
|---|---|---|
| KubeAI `Model` (`kubeai.org/v1`) | A model that KubeAI serves: `url` names the weights (`hf://`, `ollama://`, `s3://` …), `engine` the server, and the controller runs pods for it | It describes a model to run in the cluster; aisa's models run outside. The `kubeai.org` group belongs to KubeAI's controller, which would act on the objects if it is installed. Its `features` vocabulary is reused |
| Gateway API Inference Extension `InferencePool` (`inference.networking.k8s.io/v1`) | A backend that replaces a Service for model server pods, with inference-aware routing | It selects pods in the cluster. External backends do not fit, and it is the gateway's input, not a catalog for applications |
| Envoy AI Gateway `AIServiceBackend`, `AIGatewayRoute` | The gateway's configuration for providers and routes | The operator's input to one gateway, not what an application reads |

None of them is a read-only catalog of models an application may use. aisa defines its own `Model`, in the shape of KubeAI's where they overlap, so that an application written for one reads the other with little change.

## `Model`

One object per model name that the proxy offers, cluster-scoped, created from the Consul catalog (the `models` meta of the backends, [`CONSUL.md`](./CONSUL.md#backends-consul-catalog)) and from the model's description in Consul KV.

```yaml
apiVersion: aisa.hlan.net/v1alpha1
kind: Model
metadata:
  name: qwen3-8b-6f1d2c9a       # derived from the model name, see below
  labels:
    feature.aisa.hlan.net/TextGeneration: "true"
    capability.aisa.hlan.net/tools: "true"
spec:                           # written by aisa only
  model: "qwen3:8b"             # the name to send in requests
  features: [TextGeneration]    # KubeAI's vocabulary: TextGeneration, TextEmbedding, Reranking, SpeechToText
  contextLength: 32768
  capabilities: [tools]         # tools, vision, json_mode
  endpoint: http://<proxy-service>.<namespace>.svc.cluster.local/v1   # of this install
status:
  available: true               # at least one healthy backend
  conditions: [...]
```

- **Name.** `spec.model` is the exact name to send. The object's name is derived from it as follows, and the same rule makes the consumer label below:
  1. Lowercase it, replace every run of characters outside `[a-z0-9]` with one `-`, and trim `-` from both ends.
  2. Cut it to 54 characters and trim a trailing `-` again. If nothing is left (`🔥`, `:::`), use `model` (or `consumer`).
  3. Append `-` and the first 8 hex digits of the SHA-256 of the exact name.

  The result is at most 63 characters, starts and ends with a letter or digit, and is a valid object name and label value. `qwen3:8b` and `qwen3-8b` get different names. Should two names still map to one, aisa creates neither, logs an error and reports it in its metrics.
- **Found through Kubernetes.** An application lists the objects by what they can do with a label selector, for example `kubectl get models.aisa.hlan.net -l feature.aisa.hlan.net/TextGeneration,capability.aisa.hlan.net/tools`, and maps its tasks to them (US-1 criterion 8). aisa sets one label per feature and per capability from `spec`, because a selector cannot match an element of a list. A ClusterRole that aggregates to `view` lets any namespace read them. An application that does not talk to the Kubernetes API reads the same list at `/v1/models` through the proxy ([#34](https://github.com/hlan-net/aisa/issues/34)).
- **Endpoint.** The URL of the proxy's Service in the namespace aisa is installed in, both taken from the install (the chart's values), never fixed.
- **Nothing about the provider.** No backend address, provider, account or key is in the object (US-2). Whether a model is local or paid is open question 3.
- **Description in Consul.** `features`, `contextLength` and `capabilities` are not in the catalog today. They go in Consul KV, `aisa/models/<model>`, written by the holder; a model without a description gets `features: [TextGeneration]` and no other fields.
- **A model without healthy backends** keeps its object with `available: false`, so an application can tell "not on offer" from "down".
- **Every consumer sees every model.** Which models a consumer may use is not decided per consumer today; when it is, the object stays the same and the decision API denies.

## The consumer's Secret

A consumer is declared by the holder in Consul (the non-secret part, [`ARCHITECTURE.md`](./ARCHITECTURE.md#state-of-this-direction)) and says where its application runs:

```
aisa/consumers/batch-jobs   {"quota_profile": "batch", "namespace": "news", "secret": "aisa"}
```

aisa then:

1. **Writes the Secret** `aisa` in the namespace `news`, with a key it generates:
   ```yaml
   metadata:
     labels:
       aisa.hlan.net/consumer: batch-jobs-3c9e1f0a   # the consumer's name by the rule above
     annotations:
       aisa.hlan.net/consumer-name: batch-jobs     # exact
   stringData:
     OPENAI_BASE_URL: http://<proxy-service>.<namespace>.svc.cluster.local/v1
     OPENAI_API_KEY: <key>
   ```
   An application takes it with `envFrom` and calls the proxy with any OpenAI SDK, as it would call a provider (US-1 criterion 2). The plaintext key exists only in this Secret.
2. **Accepts the key** by moving its SHA-256 from `pending_sha256` to `key_sha256` in Vault (`secret/aisa/consumers/batch-jobs`), which the decision API reads as it does today ([`VAULT.md`](./VAULT.md#consumer-credentials)). Before step 1, aisa records the new key's hash as `pending_sha256`; the decision API does not accept a pending hash.
3. **Rotates the key**: the same two steps with a new key, keeping the old hash in `key_sha256` and removing it after a grace period (`AISA_KEY_ROTATION_GRACE`). When a hash was superseded is kept in the custom metadata of the Vault secret, so the grace period survives a restart. A pod that read the key into its environment keeps the old one until it restarts; open question 4.
4. **Deletes** the Secret and the hashes when the consumer is removed from Consul.

**Reconciling is restart-safe, and accepts only keys aisa issued.** The plaintext lives only in the Secret, and Vault records which hash aisa meant to issue before the Secret is written. Each pass compares the two and repairs the difference, so a crash between any two steps is finished on the next pass, and a key written into the Secret by anyone else never becomes valid:

| State found | Action |
|---|---|
| Secret owned by the consumer, its key's hash in `key_sha256` | Nothing |
| Secret owned by the consumer, its key's hash is `pending_sha256` | Accept it (step 2): a crash after step 1 |
| Secret owned by the consumer, its key's hash in neither | The key was not issued by aisa (edited by hand, or a crash before the pending hash was written). Issue a new key with steps 1 and 2; the unknown key is never accepted. Logged and counted |
| No Secret | Issue a new key with steps 1 and 2. Hashes already in `key_sha256` are kept for the grace period, because a running pod may still hold their key. The consumer's credential changes, so this is logged and counted |
| Hashes in `key_sha256` that the Secret does not hold, older than the grace period; a `pending_sha256` that the Secret does not hold | Remove them |

**Ownership.** aisa changes or deletes only a Secret that carries its label with this consumer's name. A Secret of that name without the label, or with another consumer's, is never adopted, overwritten or deleted: the consumer's reconcile fails, and the failure is logged and reported in aisa's metrics until the holder renames one of them. Two consumers that name the same namespace and Secret both fail the same way.

This is option 1 of [#32](https://github.com/hlan-net/aisa/issues/32) with aisa as the sync, so no Vault Secrets Operator or External Secrets is needed, and no pod logs in to Vault. Option 2 (the service account token) stays possible as a second credential type later.

## What aisa needs

- **Kubernetes API access**, the first time aisa talks to it: create and update `Model` objects (cluster-wide), and create, update and delete Secrets with a label of its own in the namespaces of its consumers. A ClusterRole on Secrets is broad; open question 2.
- **Write access in Vault** to `secret/aisa/consumers/*` for the hashes and their metadata. Today aisa only reads there.
- **The CRD**, shipped in aisa's chart ([`ARCHITECTURE.md`](./ARCHITECTURE.md#packaging-and-deployment)).
- **One writer.** With more than one replica, a lease elects the one that reconciles.

## Failure modes

| Failure | Behaviour |
|---|---|
| aisa down | The objects stay as they are. Applications keep their keys; the proxy decides per its fail policy ([`ARCHITECTURE.md`](./ARCHITECTURE.md#failure-modes)) |
| Consul down | No change is applied; existing objects stay |
| Vault down | No new key is issued and no rotation finishes; existing keys keep working as long as the decision API can decide |
| Kubernetes API down | Nothing changes in the cluster; aisa retries |
| An object changed by hand | Restored on the next reconcile |

## Open questions

1. **API group.** Decided: `aisa.hlan.net`, a domain the maintainer controls (2026-10-07).
2. **Scope of Secret writes.** A ClusterRole, or a Role per namespace that the chart creates from a list in its values. The second is narrower but needs a chart change for each new namespace.
3. **Local or paid in `Model`.** An application may want to prefer a free model. It reveals nothing about the account, but it is a fact about the holder's costs.
4. **Rotation and running pods.** Mount the Secret as a file and read it per request, rely on a restarter such as Reloader, or rotate only on request.
5. **Where the key hash lives.** In Vault as today, or in Consul next to the rest of the consumer; a SHA-256 of a random key is not a secret, but the rule keeps credentials in Vault.
