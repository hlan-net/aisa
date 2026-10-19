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

`aisa` manages a Kubernetes `Secret` containing standard OpenAI-compatible environment variables ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#the-consumers-secret)):

| Environment Variable | Description | Example Value from AISA |
|---|---|---|
| `OPENAI_BASE_URL` | The HTTP endpoint of the in-cluster AISA proxy | `http://aisa-apisix.aisa.svc.cluster.local/v1` |
| `OPENAI_API_KEY` | Plaintext API key issued and rotated by AISA | `aisa-1a2b3c...` |

### Recommended Configuration Precedence

In the client application's configuration loader:

```text
1. Explicit Direct Provider Keys (e.g. ANTHROPIC_API_KEY, direct OPENAI_API_KEY override)
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

### B. Helm Chart Values (`values.yaml`)

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

## 4. Registering the Application in AISA

To have `aisa` issue a key and manage the `Secret`, declare the consumer using either the Web UI or Consul KV.

### Option A: AISA Admin Web UI (`/admin`)

1. Open the AISA Admin Dashboard (`http://<aisa-admin-url>/admin`).
2. Navigate to **Consumers** and click **Create Consumer**.
3. Specify:
   - **Name**: `my-app`
   - **Namespace**: `my-namespace`
   - **Target Secret**: `my-app-aisa-key`
   - **Quota Profile**: `interactive` (or another declared quota)
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

---

## 5. Model Discovery

Applications can dynamically discover models available in the cluster using two approaches:

### Level 1: HTTP Discovery (OpenAI SDK — Standard & Portable)

AISA's proxy exposes `GET /v1/models` in OpenAI-compatible format. Only models with at least one healthy backend instance are returned.

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

AISA publishes cluster-scoped `Model` CRDs (`models.aisa.hlan.net/v1alpha1`). By default, `aisa` binds a ClusterRole to `system:serviceaccounts`, allowing all pods in the cluster to inspect models by **features** and **capabilities** ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md#model)):

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

Query with `kubectl`:

```bash
# Find all healthy models supporting function calling (tools):
kubectl get models.aisa.hlan.net -l capability.aisa.hlan.net/tools=true
```

---

## 6. Implementation Checklist for Agents

When updating a client repository (e.g. JFYI or Moirai):

- [ ] **Preserve Direct Keys**: Verify direct provider settings (direct API keys, custom endpoints) remain operational.
- [ ] **Config Hierarchy**: Ensure the configuration loader checks `OPENAI_BASE_URL` and `OPENAI_API_KEY` before falling back to local defaults or standalone URLs.
- [ ] **Secret Binding in Manifests**: Add `envFrom` with `optional: true` referencing the designated AISA secret name in the Kubernetes Deployment / Helm chart.
- [ ] **Model Resolution**: Prefer dynamic model querying (`client.models.list()`) or configurable model names over hardcoded identifiers.
- [ ] **Documentation**: Document in the application's README how to configure the app for standalone/direct inference vs. in-cluster AISA inference.
