# Terraform module for aisa

Configures Vault and Consul for [`aisa`](../docs/concepts/ARCHITECTURE.md) and its proxy, as specified in [`VAULT.md`](../docs/concepts/VAULT.md) and [`CONSUL.md`](../docs/concepts/CONSUL.md):

- **Vault policies** (`aisa` and `aisa-render`) for consumer key hashes (`<kv_mount>/data/<kv_prefix>/consumers/*`), provider credentials (`<kv_mount>/data/<kv_prefix>/providers/*`), Vault-issued provider credentials (`extra_render_read_paths`), and Consul ACL tokens (`<consul_mount>/creds/*`).
- **Vault Kubernetes auth roles** (`aisa` and `aisa-render`) bound to the service accounts of aisa and its proxy in the release namespace, with `audience = "vault"` by default. When installing the Helm chart with this default, set `apisix.vaultAgent.auth.audience=vault` alongside `vault.auth.audience=vault` (the proxy chart defaults `vaultAgent.auth.audience` to empty).
- **Consul ACL policies** (`aisa` and `aisa-render`) and matching **Vault Consul secrets engine roles** (`enable_consul_acls = true` by default).
- **Consul KV entries** under `<consul_kv_prefix>/`: quota profiles (`quotas/`), consumer declarations (`consumers/`), model descriptions and aliases (`models/`), model prices (`pricing/`), consumer monthly budgets (`budgets/`), and provider-account monthly budgets (`budgets/providers/`).
- **Consul catalog registrations** for inference backends (`service = "aisa-backend"`), with `status = "passing"` on catalog health checks by default ([#69](https://github.com/hlan-net/aisa/issues/69)).

The module takes mount names, prefixes, and service account names as variables and assumes nothing about a specific environment. Provider keys themselves live in Vault and are never passed through this module: write them directly with `vault kv put` ([`VAULT.md`](../docs/concepts/VAULT.md#provider-keys)) or through aisa's admin interface.

## Usage

```hcl
module "aisa" {
  source = "./terraform"

  namespace             = "aisa"
  aisa_service_account  = "aisa"
  proxy_service_account = "aisa-apisix"

  vault_kv_mount         = "secret"
  vault_kubernetes_mount = "kubernetes"
  vault_consul_mount     = "consul"

  quota_profiles = {
    interactive = { tokens_per_hour = 200000 }
    batch       = { tokens_per_hour = 50000 }
  }

  consumers = {
    chat-ui = {
      quota_profile = "interactive"
      namespace     = "chat"
      secret        = "aisa"
    }
    batch-jobs = {
      quota_profile = "batch"
      namespace     = "jobs"
      secret        = "aisa"
    }
  }

  models = {
    "llama3.2" = {
      features       = ["TextGeneration"]
      context_length = 131072
      capabilities   = ["tools"]
      aliases        = ["llama3.2:3b", "llama3.2:latest"]
    }
  }

  model_prices = {
    "llama3.2" = {
      input_per_1k  = 0
      output_per_1k = 0
      currency      = "EUR"
    }
    "gemini-2.5-flash" = {
      input_per_1k  = 0.0003
      output_per_1k = 0.0025
      currency      = "USD"
    }
  }

  budgets = {
    chat-ui = {
      monthly_tokens = 5000000
      soft_ratio     = 0.8
      on_exhausted   = "downgrade"
      downgrade_to   = "llama3.2"
    }
  }

  provider_budgets = {
    gemini = {
      monthly_tokens = 20000000
      soft_ratio     = 0.8
      on_exhausted   = "downgrade"
      downgrade_to   = "llama3.2"
    }
  }

  backends = {
    ollama-1 = {
      node_name   = "gpu-box"
      address     = "gpu-box.internal"
      port        = 11434
      tags        = ["local", "ollama"]
      provider    = "openai-compatible"
      models      = ["llama3.2"]
      priority    = 1
      timeout     = 300
      fail_policy = "open"
      check = {
        http     = "http://gpu-box.internal:11434/api/version"
        interval = "30s"
        timeout  = "5s"
        status   = "passing"
      }
    }
    gemini-1 = {
      node_name   = "gemini-api"
      address     = "generativelanguage.googleapis.com"
      port        = 443
      tags        = ["cloud", "google"]
      provider    = "openai-compatible"
      path        = "/v1beta/openai"
      models      = ["gemini-2.5-flash"]
      priority    = 2
      key         = "gemini"
      fail_policy = "closed"
      check = {
        tcp      = "generativelanguage.googleapis.com:443"
        interval = "60s"
        timeout  = "5s"
        status   = "passing"
      }
    }
  }
}
```
