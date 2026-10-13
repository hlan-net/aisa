# Concept: Kubernetes objects

> **Status: built** (`ROADMAP.md` v0.2.0 PR 12, `internal/kube`). It turns the direction of 2026-10-01 ([`ARCHITECTURE.md`](./ARCHITECTURE.md#state-of-this-direction)) into objects, and closes the design gaps of US-1 criteria 2 and 8 ([`../process/USER_STORIES.md`](../process/USER_STORIES.md#us-1-cost-safe-inference-that-looks-kubernetes-native)). aisa writes the objects when `AISA_KUBERNETES=true`, which the chart sets (`kubernetes.enabled`).

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
    app.kubernetes.io/managed-by: aisa
    aisa.hlan.net/install: aisa  # the namespace of the aisa that keeps it
    aisa.hlan.net/model: qwen3-8b-6f1d2c9a
  annotations:
    aisa.hlan.net/model-name: "qwen3:8b"
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
- **Found through Kubernetes.** An application lists the objects by what they can do with a label selector, for example `kubectl get models.aisa.hlan.net -l feature.aisa.hlan.net/TextGeneration,capability.aisa.hlan.net/tools`, and maps its tasks to them (US-1 criterion 8). aisa sets one label per feature and per capability from `spec`, because a selector cannot match an element of a list. Because `Model` is cluster-scoped, only a ClusterRoleBinding can grant reading it. aisa's chart ships a ClusterRole that may `get`, `list` and `watch` models and, by default, a ClusterRoleBinding of it to the group `system:serviceaccounts`, so every pod can read the catalog; it holds nothing secret. An operator who wants it narrower turns the binding off and binds the role to chosen service accounts. An application that does not talk to the Kubernetes API reads the available ones, those with a healthy backend, at `/v1/models` through the proxy ([#34](https://github.com/hlan-net/aisa/issues/34)); the OpenAI shape has no field for `available`, so a model without a backend is not on that list.
- **Endpoint.** The URL of the proxy's Service in the namespace aisa is installed in, both taken from the install (the chart's values, `AISA_PROXY_URL`), never fixed.
- **Which models.** Those of the backends that the proxy's template renders: registered as `aisa-backend` with `models`, the provider `openai-compatible` and a valid `timeout` if any. `available` is true when at least one of them passes all its checks. aisa reads the health of the service, so its Consul token needs read on the nodes too ([`CONSUL.md`](./CONSUL.md#acls)).
- **Ownership.** aisa changes and deletes only the objects with its labels and the install of the namespace it runs in. An object of the same name that is another's, such as another install's, is left alone; the model gets no object and is counted in `aisa_model_conflicts`.
- **The CRD** is in aisa's chart, in `crds/`: Helm installs it with the chart and never upgrades or deletes it, so a new version is applied by hand, and the objects stay after the chart is uninstalled.
- **Nothing about the provider.** No backend address, provider, account or key is in the object (US-2). Whether a model is local or paid is open question 3.
- **Description in Consul.** `features`, `contextLength` and `capabilities` are not in the catalog today. They go in Consul KV, `aisa/models/<model>`, written by the holder, such as `{"features": ["TextGeneration"], "contextLength": 32768, "capabilities": ["tools"]}`; every field is optional. A model without a description gets `features: [TextGeneration]` and no other fields, and so does one whose description cannot be read (not a JSON object, or a value outside the vocabularies above), which is logged and counted in `aisa_model_descriptions_invalid`.
- **A model without healthy backends** keeps its object with `available: false`, so an application can tell "not on offer" from "down".
- **Every consumer sees every model.** Which models a consumer may use is not decided per consumer today; when it is, the object stays the same and the decision API denies.

## The consumer's Secret

A consumer is declared by the holder in Consul (the non-secret part, [`ARCHITECTURE.md`](./ARCHITECTURE.md#state-of-this-direction)) and says where its application runs:

```
aisa/consumers/batch-jobs   {"quota_profile": "batch", "namespace": "news", "secret": "aisa"}
```

aisa then:

1. **Makes a new key valid before anyone sees it.** aisa generates the key (`aisa-` and 32 random bytes in hex) and, in one write of the Vault secret `secret/aisa/consumers/batch-jobs`, adds its SHA-256 to `key_sha256`, the same hash with the time to `unwritten_sha256`, and the time to `issued_at`. One KV v2 write is atomic, and aisa makes it with check-and-set on the version it read, so no crash or concurrent write can leave the hash valid without its mark. The decision API reads `key_sha256` as it does today and ignores the other fields ([`VAULT.md`](./VAULT.md#consumer-credentials)). The marks are JSON objects of hash and RFC 3339 time:
   ```json
   {"key_sha256": "<old>,<new>", "unwritten_sha256": {"<new>": "2026-10-13T12:00:00Z"},
    "superseded_sha256": {"<old>": "2026-10-13T12:00:00Z"}, "issued_at": "2026-10-13T12:00:00Z"}
   ```
2. **Writes the Secret** `aisa` in the namespace `news`:
   ```yaml
   metadata:
     labels:
       aisa.hlan.net/consumer: batch-jobs-405cbf45   # the consumer's name by the rule above
       app.kubernetes.io/managed-by: aisa
       aisa.hlan.net/install: aisa                   # the namespace of the aisa that wrote it
     annotations:
       aisa.hlan.net/consumer-name: batch-jobs     # exact
   stringData:
     OPENAI_BASE_URL: http://<proxy-service>.<namespace>.svc.cluster.local/v1
     OPENAI_API_KEY: <key>
   ```
   An application takes it with `envFrom` and calls the proxy with any OpenAI SDK, as it would call a provider (US-1 criterion 2). The plaintext key exists only in this Secret, and it is valid from the moment it appears there.
3. **Clears the mark**: removes the hash from `unwritten_sha256`, again in one write.
4. **Rotates the key** with the same three steps when it is `AISA_KEY_ROTATION_INTERVAL` old (30 days by default; `0` never rotates), keeping the old hash in `key_sha256` and removing it after a grace period (`AISA_KEY_ROTATION_GRACE`, 7 days by default). The same write records the old hash with the time in `superseded_sha256`, so the grace period survives a restart. A pod that read the key into its environment keeps the old one until it restarts, so it must restart within the grace period (a restarter such as Reloader does it), or mount the Secret as a file and read it again.
5. **Deletes** the Secret and the hashes when the consumer is removed from Consul, or its declaration no longer names a Secret: the hashes first, so a failure leaves the Secret for the next pass to try again. Each pass also goes through the consumers in Vault: one that names no Secret, declared or not, loses the hashes of the keys aisa issued (those with `issued_at`) even when its Secret is already gone; the hashes of a consumer that never had a Secret are the holder's and stay. When it names another Secret, the old one is deleted and the new one gets a new key, the old key's hash staying for the grace period. A consumer whose declaration cannot be read is left as it is.

**Reconciling is restart-safe, and accepts only keys aisa issued.** Every hash in `key_sha256` was added by aisa before its key was written anywhere, so a key written into the Secret by anyone else never becomes valid. Each pass compares the Secret with Vault and repairs the difference, so a crash between any two steps is finished on the next pass:

| State found | Action |
|---|---|
| Secret owned by the consumer, its key's hash in `key_sha256` | Clear a leftover mark in `unwritten_sha256` (a crash after step 2). Restore `OPENAI_BASE_URL` if it was changed; the key stays. Rotate the key when it is due |
| Secret owned by the consumer, its key's hash not in `key_sha256` | The key was not issued by aisa, or was edited by hand. Issue a new key with steps 1 to 3, superseding the hashes already there as in rotation; the unknown key is never accepted. Logged and counted |
| No Secret | Issue a new key with steps 1 to 3. Hashes already in `key_sha256` are recorded in `superseded_sha256` and kept for the grace period, because a running pod may still hold their key. The consumer's credential changes, so this is logged and counted |
| A hash in `unwritten_sha256` that the Secret does not hold, older than a minute | Remove it: a crash between steps 1 and 2 left a valid hash whose key nobody has |
| Hashes in `superseded_sha256` longer ago than the grace period | Remove them from both fields |

**Ownership.** aisa changes or deletes only a Secret that carries its labels, with this consumer's name and the install of the namespace aisa runs in, and the annotation with the exact name; a Secret whose labels were changed by hand is no longer aisa's. A Secret of that name without the label, or with another consumer's, is never adopted, overwritten or deleted: the consumer's reconcile fails, and the failure is logged and reported in aisa's metrics until the holder renames one of them.

**One Secret, one consumer.** Before reconciling, aisa checks the consumers in Consul for duplicate targets. When two or more name the same namespace and Secret, none of them is reconciled, whichever came first, and each is reported until the holder changes one; their existing Secrets and hashes are left as they are.

This is option 1 of [#32](https://github.com/hlan-net/aisa/issues/32) with aisa as the sync, so no Vault Secrets Operator or External Secrets is needed, and no pod logs in to Vault. Option 2 (the service account token) stays possible as a second credential type later.

## What aisa needs

- **Kubernetes API access**: create, update and delete `Model` objects (cluster-wide), and Secrets in the namespaces of its consumers. The chart gives aisa a ClusterRole for both (open question 2), and projects the token for the API into aisa's container only: the Vault Agent next to it gets none, since the service account may read Secrets in every namespace.
- **Write access in Vault** to `secret/aisa/consumers/*` for the hashes: `create`, `read` and `update` ([`VAULT.md`](./VAULT.md#who-reads-what)).
- **Read access to the nodes in Consul**, for the health of the backends ([`CONSUL.md`](./CONSUL.md#acls)).
- **The CRD**, shipped in aisa's chart ([`ARCHITECTURE.md`](./ARCHITECTURE.md#packaging-and-deployment)).
- **One writer.** A Lease in aisa's namespace (`<fullname>-leader`) elects the replica that writes; two would each issue a key. A replica that stops leading releases the Lease only after its passes have returned, so two never write at once. The passes run every `AISA_KUBE_RESYNC` (30 s), and at once when the consumers' declarations change.

## Failure modes

| Failure | Behaviour |
|---|---|
| aisa down | The objects stay as they are. Applications keep their keys; the proxy decides per its fail policy ([`ARCHITECTURE.md`](./ARCHITECTURE.md#failure-modes)) |
| Consul down | No change is applied; existing objects stay |
| Vault down | No new key is issued and no rotation finishes; existing keys keep working as long as the decision API can decide |
| Kubernetes API down | Nothing changes in the cluster; aisa retries |
| An object changed by hand | Restored on the next reconcile |
| A consumer's Secret cannot be written (its namespace does not exist, say) | Counted in `aisa_consumer_secrets{state="error"}` and logged; the next attempt to issue its key waits a minute, doubling up to 30 minutes, so the failure does not add a hash to Vault on every pass |

## Open questions

1. **API group.** Decided: `aisa.hlan.net`, a domain the maintainer controls (2026-10-07).
2. **Scope of Secret writes.** Decided: a ClusterRole (2026-10-13). A Role per namespace from a list in the chart's values would be narrower, but needs a chart change for each new namespace; aisa's code changes only the Secrets that carry its labels.
3. **Local or paid in `Model`.** An application may want to prefer a free model. It reveals nothing about the account, but it is a fact about the holder's costs.
4. **Rotation and running pods.** Decided: rotation on a schedule, with a grace period (2026-10-13). An application restarts within the grace period, or reads the Secret as a file.
5. **Where the key hash lives.** In Vault as today, or in Consul next to the rest of the consumer; a SHA-256 of a random key is not a secret, but the rule keeps credentials in Vault.
