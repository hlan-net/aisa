# Client Application Integration Guide

This guide describes how to integrate client applications (a chat UI, a batch service, a CLI tool or any other service) with `aisa` in Kubernetes, while preserving the application's ability to run standalone and connect directly to AI providers.

---

## 1. Principles

1. **Dual-Mode Operation (No Vendor Lock-in):**
   - The application **must continue supporting direct inference** (e.g. direct API keys for OpenAI, Anthropic, or an explicit local Ollama endpoint).
   - In-cluster `aisa` support is an additive, transparent drop-in that presents an OpenAI-compatible API.
2. **Kubernetes-Native Credential Ingestion:**
   - In Kubernetes, `aisa` reconciles and manages a Kubernetes `Secret` directly inside the application's namespace.
   - The application consumes this Secret via `envFrom` or `valueFrom.secretKeyRef`.
3. **No Direct Vault or Consul Access:**
   - Client applications never access Vault or Consul. Identity, quotas, budgets, rate-limiting, and provider isolation are handled upstream by `aisa` and its proxy.

### What the endpoint offers

The proxy serves the OpenAI-compatible API, and these paths of it:

- `POST /v1/chat/completions`, streamed or not, through aisa's decision
- `POST /v1/embeddings`, through aisa's decision; its prompt tokens count towards the consumer's quota. Use a model whose `Model` object has the feature `TextEmbedding`: every model is routed on both paths, and a chat model asked for embeddings answers with its backend's error
- `GET /v1/models`, the models on offer, without a credential

Provider-native APIs, such as Anthropic's `/v1/messages`, are not offered: use an OpenAI-compatible client, or the OpenAI-compatible endpoint of the provider's SDK. The legacy `/v1/completions` is not offered either ([#34](https://github.com/hlan-net/aisa/issues/34)).

---

## 2. Environment Variables & Secret Contract

`aisa` manages a Kubernetes `Secret` containing standard OpenAI-compatible environment variables ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#the-consumers-secret)).

### Secret Contract

To ensure forward compatibility, client applications should treat the Secret contract as divided into **mandatory keys** and **optional extensions**:

| Key | Status | Description | Example Value |
|---|---|---|---|
| `OPENAI_BASE_URL` | **Mandatory** | The HTTP endpoint of the in-cluster AISA proxy | `http://aisa-apisix.aisa.svc.cluster.local/v1` |
| `OPENAI_API_KEY` | **Mandatory** | Plaintext API key issued and rotated by AISA | `aisa-1a2b3c...` |
| `AISA_BASE_URL` | *Recommended Alias* | Namespaced alias to prevent collision with direct OpenAI | `http://aisa-apisix.aisa.svc.cluster.local/v1` |
| `AISA_API_KEY` | *Recommended Alias* | Namespaced alias to prevent collision with direct OpenAI | `aisa-1a2b3c...` |
| `OPENAI_MODEL` / `AISA_MODEL` / `LLM_MODEL` | *Optional (Reserved)* | Default model assigned to this consumer or quota profile | `qwen3:8b` |

> [!NOTE]
> Client applications must ignore unknown keys in the Secret so future AISA additions (such as telemetry tags or key rotation metadata) do not break existing deployments.

### Application Integration Patterns

Applications integrating with AISA typically follow one of two patterns:

#### Pattern A: Implicit Fallback Hierarchy (e.g. a batch service, `batch-jobs`)

When the application has a single global inference engine, use a strict configuration precedence:

```text
1. Explicit Direct Provider Keys (e.g. `ANTHROPIC_API_KEY`, direct vendor credentials)
2. In-cluster AISA Environment (OPENAI_BASE_URL + OPENAI_API_KEY from Secret)
3. Local Default Fallback (e.g. http://localhost:11434/v1 for local Ollama)
```

#### Pattern B: Multi-Provider / Explicit Provider Selection (e.g. a chat UI, `chat-ui`)

When the application lets users or tenants select between different providers (e.g. `ollama`, `openai`, `gemini`, `aisa`):

- Treat `aisa` as a distinct, first-class provider next to the others.
- Do not let AISA's environment variables silently redirect direct `openai` or `ollama` traffic.
- Resolve AISA credentials only when the user or task selects `provider: "aisa"`.

---

## 3. Kubernetes Deployment Patterns

Depending on whether your application uses global env vars (Pattern A) or multi-provider settings (Pattern B), choose either `envFrom` or `secretKeyRef`:

### Pattern 1: Simple Single-Provider (`envFrom`)

Bind the container to AISA's managed Secret using `optional: true` so the pod can deploy even before AISA completes its initial reconciliation pass:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-namespace
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
        - name: app
          image: my-app:latest
          # Ingest AISA credentials when available
          envFrom:
            - secretRef:
                name: my-app-aisa-key
                optional: true
          env:
            - name: DEFAULT_MODEL
              value: "qwen3:8b"
```

> [!IMPORTANT]
> **First-start behavior with `optional: true`:**
> `envFrom` is evaluated only once when container processes are created. If the pod starts before AISA creates the Secret, the pod starts without the environment variables and will **not** receive them until the pod is restarted! Once AISA creates the Secret, restart the deployment (`kubectl rollout restart`).

### Pattern 2: Multi-Provider / No-Collision Mapping (`secretKeyRef`)

If your application also supports direct OpenAI credentials, using `envFrom` can be hazardous:
1. Kubernetes `env` variables **always override** `envFrom` variables of the same name (even if the `env` variable is empty `""`).
2. Standard OpenAI SDKs automatically pick up `OPENAI_BASE_URL` from `envFrom`, which might accidentally redirect direct OpenAI calls to the AISA proxy with the wrong key.

To prevent collisions, map the Secret keys explicitly to `AISA_*` variables using `secretKeyRef`:

```yaml
containers:
  - name: app
    env:
      # Direct OpenAI credentials remain unpolluted:
      - name: OPENAI_API_KEY
        value: "sk-proj-direct..."
      # AISA credentials cleanly mapped with optional: true
      - name: AISA_BASE_URL
        valueFrom:
          secretKeyRef:
            name: my-app-aisa-key
            key: OPENAI_BASE_URL
            optional: true
      - name: AISA_API_KEY
        valueFrom:
          secretKeyRef:
            name: my-app-aisa-key
            key: OPENAI_API_KEY
            optional: true
      - name: AISA_MODEL
        valueFrom:
          secretKeyRef:
            name: my-app-aisa-key
            key: OPENAI_MODEL
            optional: true
```

### Key Rotation, Grace Period & Volume Mounts

AISA automatically rotates consumer API keys on a configured schedule (default: 30 days).

1. **Grace Period:** When a new key is issued, the old key remains valid in Vault for a grace window (`AISA_KEY_ROTATION_GRACE`, 7 days by default). During this window, requests signed with either the old or new key are accepted.
2. **Pod Restarts (`envFrom` / `secretKeyRef`):** Because environment variables are only loaded at pod creation, running pods must restart within the 7-day grace window. Using an automatic Secret-watching controller such as [Stakater Reloader](https://github.com/stakater/Reloader) is recommended for production deployments:
   ```yaml
   metadata:
     annotations:
       secret.reloader.stakater.com/reload: "my-app-aisa-key"
   ```
3. **Zero-Restart Alternative — Volume Mount (`OPENAI_API_KEY_FILE`):**
   Mount the Secret as a volume. Kubelet automatically updates mounted files within minutes of rotation:
   ```yaml
   volumeMounts:
     - name: aisa-secret-vol
       mountPath: /etc/aisa
       readOnly: true
   volumes:
     - name: aisa-secret-vol
       secret:
         secretName: my-app-aisa-key
   ```
   Because the official `openai` Python SDK does not re-read files automatically, read the file dynamically on client instantiation or before requests:
   ```python
   import os
   from openai import OpenAI

   def get_aisa_client() -> OpenAI:
       key_file = os.getenv("AISA_API_KEY_FILE", "/etc/aisa/OPENAI_API_KEY")
       if os.path.exists(key_file):
           with open(key_file, "r", encoding="utf-8") as f:
               api_key = f.read().strip()
       else:
           api_key = os.getenv("AISA_API_KEY") or os.getenv("OPENAI_API_KEY")
       
       base_url = os.getenv("AISA_BASE_URL") or os.getenv("OPENAI_BASE_URL")
       return OpenAI(api_key=api_key, base_url=base_url)
   ```

### Helm Chart Values (`values.yaml`)

Allow operators to toggle between in-cluster AISA and direct provider configuration:

```yaml
inference:
  # In-cluster AISA proxy integration
  aisa:
    enabled: true
    secretName: "my-app-aisa-key"
    reloader: true  # Enables stakater/reloader annotation

  # Direct provider fallback (used when aisa.enabled is false)
  direct:
    enabled: false
    openaiApiKey: ""
    baseUrl: "https://api.openai.com/v1"
```

In the Helm deployment template:

```yaml
{{- if .Values.inference.aisa.enabled }}
env:
  - name: AISA_BASE_URL
    valueFrom:
      secretKeyRef:
        name: {{ .Values.inference.aisa.secretName }}
        key: OPENAI_BASE_URL
        optional: true
  - name: AISA_API_KEY
    valueFrom:
      secretKeyRef:
        name: {{ .Values.inference.aisa.secretName }}
        key: OPENAI_API_KEY
        optional: true
{{- end }}
```

---

## 4. Registering the Application in AISA & Quota Profiles

To have `aisa` issue a key and manage the `Secret`, declare the consumer using either the Web UI or Consul KV.

### Quota Profiles (Concrete Rates)

AISA enforces token quotas via a sliding 60-minute window in Redis. When declaring a consumer, select an appropriate profile:

| Profile | Typical Capacity (`tokens_per_hour`) | Typical Rate | Intended Workload |
|---|---|---|---|
| `batch` | `100,000` to `200,000` | ~30–50 tokens/sec (~1 request/min) | Background jobs, scrapers, bulk data processing, embeddings |
| `interactive` | `1,000,000` to `5,000,000` | ~300–1,500 tokens/sec (~10–30 rpm) | User-facing chat, real-time UI queries, interactive agents |

A consumer without `quota_profile` is denied with 403 `no_quota`, unless the holder has set a default profile; no consumer is unlimited.

> [!TIP]
> Choose `batch` for non-urgent tasks to prevent background processes from consuming quota needed by user-facing services.

### Option A: AISA Admin Web UI (`/admin`)

1. Open the AISA Admin Dashboard (`http://<aisa-admin-url>/admin`).
2. Navigate to **Consumers** and click **Create Consumer**.
3. Specify:
   - **Name**: `my-app`
   - **Namespace**: `my-namespace`
   - **Target Secret**: `my-app-aisa-key`
   - **Quota Profile**: `interactive`
4. Submit the form. `aisa` creates the Secret in `my-namespace` within seconds.

### Option B: Consul KV Declaration (GitOps / Terraform)

Write a JSON declaration to `aisa/consumers/<name>` in Consul KV:

```json
{
  "quota_profile": "interactive",
  "namespace": "my-namespace",
  "secret": "my-app-aisa-key"
}
```

*(Note: In future releases, consumer self-declaration will be supported via a namespace-scoped `Consumer` CRD or a Deployment annotation such as `aisa.hlan.net/consumer: my-app`.)*

---

## 5. Model Discovery

Applications can dynamically discover models available in the cluster using two approaches:

### Level 1: HTTP Discovery (OpenAI SDK — Standard & Portable)

AISA's proxy exposes `GET /v1/models` in OpenAI-compatible format. Only models with at least one healthy backend instance are returned. This approach works in all environments and requires **no Kubernetes permissions**.

#### Python (Official `openai` SDK)

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url=os.getenv("OPENAI_BASE_URL"),
    api_key=os.getenv("OPENAI_API_KEY"),
)

# Lists all currently available, healthy models
available_models = [m.id for m in client.models.list().data]
print("Available cluster models:", available_models)
```

#### Go (`github.com/sashabaranov/go-openai`)

```go
package main

import (
	"context"
	"fmt"
	"os"

	openai "github.com/sashabaranov/go-openai"
)

func main() {
	cfg := openai.DefaultConfig(os.Getenv("OPENAI_API_KEY"))
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		cfg.BaseURL = baseURL
	}
	client := openai.NewClientWithConfig(cfg)

	models, err := client.ListModels(context.Background())
	if err != nil {
		panic(err)
	}
	for _, m := range models.Models {
		fmt.Println("Available model:", m.ID)
	}
}
```

### Level 2: Kubernetes Native Discovery (`Model` CRD)

AISA publishes cluster-scoped `Model` CRDs (`models.aisa.hlan.net/v1alpha1`). By default, `aisa` binds a ClusterRole to `system:serviceaccounts`, allowing pods to inspect models by **features** and **capabilities** ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#model)):

```yaml
apiVersion: aisa.hlan.net/v1alpha1
kind: Model
metadata:
  name: qwen3-8b-6f1d2c9a
  labels:
    feature.aisa.hlan.net/TextGeneration: "true"
    capability.aisa.hlan.net/tools: "true"
    capability.aisa.hlan.net/vision: "true"
spec:
  model: "qwen3:8b"       # Name to specify in API requests
  contextLength: 32768
status:
  available: true         # True when at least one backend is healthy
```

> [!WARNING]
> **ServiceAccount Token Requirement:**
> Reading the `Model` CRD requires access to the Kubernetes API. If your pod sets `automountServiceAccountToken: false` (common in security-hardened workloads such as batch services), the pod cannot communicate with the Kubernetes API server and **cannot use Level 2**. Use **Level 1 HTTP Discovery (`GET /v1/models`)** instead, which works purely through the proxy endpoint.

---

## 6. Error, Timeout & Quota Semantics

When designing error handling in client applications:

### Status Codes and Error Types

Every error is an OpenAI-style body, `{"error": {"message": …, "type": …, "code": …}}`. The answers of aisa's decision have a `code`; the proxy's own answers have only a `type`. Match on `code` first, then on `type`; the message is for people and may change.

| HTTP Status | `type` / `code` | From | Description | Client Action |
|---|---|---|---|---|
| **400** | `invalid_request_error` / `missing_model` | aisa | The request names no model | Send `model` in the body |
| **400** | `invalid_request_error` / `invalid_model` | aisa | The model name is longer than 256 bytes | Use a name from `GET /v1/models` |
| **401** | `invalid_request_error` / `invalid_api_key` | aisa | No `Authorization: Bearer` key, a key aisa does not know, or a replaced key after its grace period | Re-read the Secret (restart the pod, or mount it as a file), or check the consumer's declaration |
| **403** | `invalid_request_error` / `no_quota` | aisa | The consumer has no quota profile, and the holder has set no default | Ask the holder to give the consumer a `quota_profile` |
| **413** | `invalid_request_error` / `request_too_large` | aisa | The body is too large for aisa to find the model in it (the proxy normally spares aisa the body) | Make the request smaller |
| **429** | `tokens` / `rate_limit_exceeded` | aisa | The consumer's hourly token quota is used up | Wait for `Retry-After` seconds; or ask for a larger quota profile |
| **429** | `insufficient_quota` / `budget_exhausted` | aisa | The consumer's monthly token budget on priced models is used up, and its budget says `reject` | Use a free model, or wait for the next month; ask the holder for a larger budget or a downgrade |
| **429** | `insufficient_quota` / `provider_budget_exhausted` | aisa | The holder's monthly budget for the provider account that serves the model is used up, for every consumer | Use a model of another provider or a local one; the holder decides |
| **429** | `provider_rate_limited` | proxy | The provider limits the gateway's account | Back off with jitter and retry |
| **431** | `invalid_request_error` | proxy | The request has more headers than the proxy reads | Send fewer headers |
| **4xx** | `invalid_request_error` | proxy | The provider rejected the request (its own text is not passed on) | Check the request against the model's limits, such as its context length |
| **502** | `provider_error` | proxy | The provider failed, or refused the gateway's credentials; or every backend of the model failed in a row (a fast failure is retried on another backend, when there is one) | Retry later; the holder sees the cause in the gateway's log |
| **5xx** | `gateway_error` | proxy | The gateway could not reach a provider | Retry later |
| **503** | `server_error` / `consumers_unavailable`, `quota_unavailable` or `budget_unavailable` | aisa | aisa cannot verify keys, or the consumer's quota or budget, at the moment, and refuses rather than guesses | Retry with backoff |
| **503** | `aisa_unreachable` | proxy | aisa is down and this model's backends do not serve without it | Retry with backoff, or use a local model that fails open |
| **503** | `no_backend` | proxy | No healthy backend serves the model | Check the name in `GET /v1/models`; the model may be on a machine that is asleep |

Sample 429 quota error response, with `Retry-After: 142`:

```json
{
  "error": {
    "message": "token quota exhausted: 1000412 of 1000000 tokens per hour used",
    "type": "tokens",
    "code": "rate_limit_exceeded"
  }
}
```

A consumer with a monthly budget sees `X-Aisa-Budget-Remaining` on every answer: the tokens it may still use this month on priced models, 0 when none are left. The budget bounds priced models only; a free local model is always served, subject to the hourly quota. When the budget is used up, the holder's choice for the consumer decides: `reject`, a 429 as above, or `downgrade`, in which case the request is served by the budget's local model instead, and the answer's `model` field names it. An application that must know which model answered reads that field, not the one it asked for.

### Timeouts & Streaming (`stream: true`)

1. **Upstream Timeouts:**
   - Model inference requests can be long-running (especially with large local models or complex reasoning). The proxy waits for a backend's next byte **300 seconds** by default; the holder can raise it per backend to at most **600 seconds** ([`CONSUL.md`](../concepts/CONSUL.md#backends-consul-catalog), meta `timeout`).
   - The timeout applies to the wait for the next byte, so a streamed answer can take longer than it in total, and a non-streamed one cannot. Stream (`stream: true`) when an answer may take minutes.
   - Client HTTP clients (such as `httpx` or `requests`) should set an explicit read timeout of at least 300 s rather than relying on default short timeouts.
2. **Streaming Semantics:**
   - The AISA proxy natively streams Server-Sent Events (SSE) when `stream: true` is passed.
   - For OpenAI SDKs and compatible servers (such as Ollama), token counts are transmitted in the final chunk (`stream_options: {"include_usage": true}`).
   - If a client disconnects mid-stream, AISA's usage ledger records an estimated token count based on the consumer's recent average for that model to prevent unmetered quota draining.
3. **Usage Field Integrity (`usage.total_tokens`):**
   - The AISA proxy (APISIX) **does not strip or alter** the response JSON body from the upstream model provider.
   - The response `usage` object (`prompt_tokens`, `completion_tokens`, `total_tokens`) passes through untouched, allowing client applications to perform local cost and token tracking without discrepancy.

---

## 7. Verification & Testing

### 7.1 In-Cluster Verification

Verify that the Secret and proxy connectivity are operational within the cluster.

#### Step 1: Check Secret Creation

Verify that AISA has reconciled the Secret in your target namespace:

```bash
kubectl get secret my-app-aisa-key -n my-namespace
```

Inspect the base URL stored in the Secret:

```bash
kubectl get secret my-app-aisa-key -n my-namespace -o jsonpath='{.data.OPENAI_BASE_URL}' | base64 -d
# Output: http://aisa-apisix.aisa.svc.cluster.local/v1
```

#### Step 2: Test Inference from an Ephemeral Pod

Run an ephemeral curl container mounting the Secret.

> [!CAUTION]
> Wrap the command in `sh -c '...'` so that `$OPENAI_API_KEY` and `$OPENAI_BASE_URL` are evaluated **inside the container**, not by your local shell:

```bash
kubectl run aisa-test -n my-namespace --rm -it --restart=Never \
  --image=curlimages/curl \
  --env-from=secret/my-app-aisa-key -- \
  sh -c 'curl -fsS -H "Authorization: Bearer $OPENAI_API_KEY" "$OPENAI_BASE_URL/models"'
```

If successful, the command prints the JSON list of available models.

### 7.2 Local Development & Outside-Cluster Testing

To test applications running locally (or in Docker Compose) against in-cluster AISA without deploying into Kubernetes:

1. **Port-Forward the AISA Proxy:**
   ```bash
   kubectl port-forward -n aisa svc/aisa-apisix 8080:80
   ```
2. **Retrieve the Consumer API Key:**
   ```bash
   OPENAI_API_KEY=$(kubectl get secret my-app-aisa-key -n my-namespace -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)
   OPENAI_BASE_URL="http://localhost:8080/v1"
   ```
3. **Test with Curl or SDK:**
   ```bash
   curl -s -H "Authorization: Bearer $OPENAI_API_KEY" "$OPENAI_BASE_URL/models"
   ```

---

## 8. Implementation Checklist for Agents

When updating a client repository (e.g. `chat-ui` or `batch-jobs`):

- [ ] **Preserve Direct Keys**: Verify direct provider settings (direct API keys, custom endpoints) remain operational and are not hijacked by AISA variables.
- [ ] **Select Integration Pattern**:
  - For single-engine apps: Implement implicit hierarchy (`Direct Keys` → `AISA` → `Local fallback`).
  - For multi-provider apps: Treat `aisa` as an explicit provider choice and map secrets to `AISA_*` variables using `secretKeyRef`.
- [ ] **Startup Logging**: Log resolved provider, base URL, and model at startup (**never log secret keys**).
- [ ] **Secret Binding in Manifests**: Use `optional: true` referencing the designated AISA secret name in the Kubernetes Deployment / Helm chart.
- [ ] **ServiceAccount Verification**: If `automountServiceAccountToken: false` is enabled, rely strictly on Level 1 HTTP discovery (`GET /v1/models`).
- [ ] **Model Resolution**: Prefer dynamic model querying (`client.models.list()`) or configurable model names (`OPENAI_MODEL` / `AISA_MODEL`) over hardcoded identifiers.
- [ ] **Verification**: Test using `sh -c` pod commands or local `kubectl port-forward` to ensure end-to-end connectivity.
- [ ] **Documentation**: Document in the application's README how to configure the app for standalone/direct inference vs. in-cluster AISA inference.
