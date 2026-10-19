# Client Application Integration Guide

This guide describes how to integrate client applications (such as JFYI, Moirai, or any custom service) with `aisa` in Kubernetes, while preserving the application's ability to run standalone and connect directly to AI providers.

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

---

## 2. Environment Variables & Secret Contract

`aisa` manages a Kubernetes `Secret` containing standard OpenAI-compatible environment variables ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#the-consumers-secret)).

### Secret Contract

To ensure forward compatibility, client applications should treat the Secret contract as divided into **mandatory keys** and **optional extensions**:

| Key | Status | Description | Example Value |
|---|---|---|---|
| `OPENAI_BASE_URL` | **Mandatory** | The HTTP endpoint of the in-cluster AISA proxy | `http://aisa-apisix.aisa.svc.cluster.local/v1` |
| `OPENAI_API_KEY` | **Mandatory** | Plaintext API key issued and rotated by AISA | `aisa-1a2b3c...` |
| `OPENAI_MODEL` / `LLM_MODEL` | *Optional (Reserved)* | Default model assigned to this consumer or quota profile | `qwen3:8b` |

> [!NOTE]
> Client applications must ignore unknown keys in the Secret so future AISA additions (such as default model or telemetry tags) do not break existing deployments.

### Configuration Precedence & Kubernetes Overrides

In Kubernetes, standard environment variables defined in `env` **override** values of the same name imported via `envFrom`:

```yaml
# env takes precedence over envFrom of the same name
envFrom:
  - secretRef:
      name: my-app-aisa-key
env:
  - name: OPENAI_BASE_URL
    value: "https://api.openai.com/v1" # Overrides AISA proxy endpoint
```

Inside the application's configuration loader, the recommended order of precedence is:

```text
1. Explicit Direct Provider Keys (e.g. ANTHROPIC_API_KEY, direct vendor credentials)
2. In-cluster AISA Environment (OPENAI_BASE_URL + OPENAI_API_KEY from Secret)
3. Local Default Fallback (e.g. http://localhost:11434/v1)
```

When `OPENAI_BASE_URL` and `OPENAI_API_KEY` are populated (via AISA's Secret), standard OpenAI SDKs (Python `openai`, Go `go-openai`, LangChain, etc.) automatically route requests through `aisa` without requiring custom networking or authorization code.

---

## 3. Kubernetes Deployment Patterns

### A. Deployment Manifest (`envFrom`)

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
          # 1. Ingest AISA credentials when available
          envFrom:
            - secretRef:
                name: my-app-aisa-key
                optional: true
          # 2. Application-specific configuration
          env:
            - name: DEFAULT_MODEL
              value: "qwen3:8b"
```

> [!IMPORTANT]
> **First-start behavior with `optional: true`:**
> `envFrom` is evaluated only once when the container processes are created. If the pod starts before AISA has created the Secret, the pod starts without the environment variables and will **not** receive them until the pod is restarted! Once AISA creates the Secret, restart the deployment or ensure AISA consumer setup precedes application startup.

### B. Key Rotation & Grace Period

AISA automatically rotates consumer API keys on a configured schedule (default: 30 days).

1. **Grace Period:** When a new key is issued, the old key remains valid in Vault for a grace window (`AISA_KEY_ROTATION_GRACE`, 7 days by default). During this window, requests signed with either the old or new key are accepted.
2. **Pod Restarts (`envFrom`):** Because `envFrom` variables are only loaded at pod creation, running pods must restart within the 7-day grace window to pick up the rotated key from the Secret. Using an automatic Secret-watching controller such as [Stakater Reloader](https://github.com/stakater/Reloader) is recommended for production deployments.
3. **Alternative — Volume Mount (Zero-Restart):** If the application supports reading keys from a file (e.g. `OPENAI_API_KEY_FILE`), mount the Secret as a volume:
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
   Kubelet automatically updates the mounted files in `/etc/aisa/OPENAI_API_KEY` within minutes of rotation, allowing apps that dynamically read the file to rotate credentials without any pod restarts.

### C. Helm Chart Values (`values.yaml`)

Allow operators to toggle between in-cluster AISA and direct provider configuration:

```yaml
inference:
  # In-cluster AISA proxy integration
  aisa:
    enabled: true
    secretName: "my-app-aisa-key"

  # Direct provider fallback (used when aisa.enabled is false)
  direct:
    enabled: false
    openaiApiKey: ""
    baseUrl: "https://api.openai.com/v1"
```

In the Helm deployment template:

```yaml
{{- if .Values.inference.aisa.enabled }}
envFrom:
  - secretRef:
      name: {{ .Values.inference.aisa.secretName }}
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
| `unlimited` | None (omit `quota_profile`) | Unbounded | Internal admin tools, critical cluster infrastructure |

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
> Reading the `Model` CRD requires access to the Kubernetes API. If your pod sets `automountServiceAccountToken: false` (common in security-hardened workloads such as JFYI), the pod cannot communicate with the Kubernetes API server and **cannot use Level 2**. Use **Level 1 HTTP Discovery (`GET /v1/models`)** instead, which works purely through the proxy endpoint.

---

## 6. Error & Quota Semantics

When designing error handling in client applications:

1. **HTTP 429 Too Many Requests:**
   - Returned when the consumer exceeds its token quota within the sliding 1-hour window.
   - The response includes a standard `Retry-After: <seconds>` header indicating when sufficient tokens will leave the window.
   - Response body follows the standard OpenAI error format:
     ```json
     {
       "error": {
         "message": "Token quota exceeded; retry after 142s",
         "type": "aisa_quota_exceeded",
         "code": 429
       }
     }
     ```
2. **HTTP 503 Service Unavailable:**
   - Returned when no backend serving the requested model is currently healthy, or when quota profile data is temporarily unavailable.
3. **Usage Field Integrity (`usage.total_tokens`):**
   - The AISA proxy (APISIX) **does not strip or alter** the response JSON body from the upstream model provider.
   - The response `usage` object (`prompt_tokens`, `completion_tokens`, `total_tokens`) passes through untouched, allowing client applications to perform local cost and token tracking without discrepancy.

---

## 7. In-Cluster Verification & Testing

Before deploying your application, verify that the Secret and proxy connectivity are operational within the cluster.

### Step 1: Check Secret Creation

Verify that AISA has reconciled the Secret in your target namespace:

```bash
kubectl get secret my-app-aisa-key -n my-namespace
```

Inspect the base URL stored in the Secret:

```bash
kubectl get secret my-app-aisa-key -n my-namespace -o jsonpath='{.data.OPENAI_BASE_URL}' | base64 -d
# Output: http://aisa-apisix.aisa.svc.cluster.local/v1
```

### Step 2: Test Inference from an Ephemeral Pod

Run an ephemeral curl container mounting the Secret to verify connectivity and authentication:

```bash
kubectl run aisa-test -n my-namespace --rm -it --restart=Never \
  --image=curlimages/curl \
  --env-from=secret/my-app-aisa-key -- \
  curl -fsS -H "Authorization: Bearer $OPENAI_API_KEY" "$OPENAI_BASE_URL/models"
```

If successful, the command prints the JSON list of available models.

---

## 8. Implementation Checklist for Agents

When updating a client repository (e.g. JFYI or Moirai):

- [ ] **Preserve Direct Keys**: Verify direct provider settings (direct API keys, custom endpoints) remain operational.
- [ ] **Config Hierarchy**: Ensure the configuration loader checks `OPENAI_BASE_URL` and `OPENAI_API_KEY` before falling back to local defaults or standalone URLs.
- [ ] **Startup Logging**: Log resolved provider, base URL, and model at startup (**never log secret keys**). This ensures missing or misconfigured credentials are immediately diagnosed.
- [ ] **Secret Binding in Manifests**: Add `envFrom` with `optional: true` referencing the designated AISA secret name in the Kubernetes Deployment / Helm chart.
- [ ] **ServiceAccount Verification**: If `automountServiceAccountToken: false` is enabled, rely strictly on Level 1 HTTP discovery (`GET /v1/models`).
- [ ] **Model Resolution**: Prefer dynamic model querying (`client.models.list()`) or configurable model names over hardcoded identifiers.
- [ ] **Documentation**: Document in the application's README how to configure the app for standalone/direct inference vs. in-cluster AISA inference.
