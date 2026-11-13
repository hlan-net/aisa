variable "namespace" {
  description = "Kubernetes namespace where aisa and its proxy are installed."
  type        = string
  default     = "aisa"
}

variable "aisa_service_account" {
  description = "Kubernetes ServiceAccount name of aisa."
  type        = string
  default     = "aisa"
}

variable "proxy_service_account" {
  description = "Kubernetes ServiceAccount name of aisa's proxy."
  type        = string
  default     = "aisa-apisix"
}

variable "vault_kv_mount" {
  description = "Path of the Vault KV v2 secrets engine mount."
  type        = string
  default     = "secret"
}

variable "vault_kv_prefix" {
  description = "Prefix under the Vault KV v2 mount where aisa stores consumer hashes and provider keys."
  type        = string
  default     = "aisa"
}

variable "create_vault_kv_mount" {
  description = "Whether to create the Vault KV v2 mount at vault_kv_mount."
  type        = bool
  default     = false
}

variable "vault_kubernetes_mount" {
  description = "Mount path of Vault's Kubernetes auth method."
  type        = string
  default     = "kubernetes"
}

variable "vault_kubernetes_audience" {
  description = "Token audience required by the Vault Kubernetes auth roles. Must match vault.auth.audience and apisix.vaultAgent.auth.audience in the aisa Helm chart."
  type        = string
  default     = "vault"
}

variable "vault_token_ttl" {
  description = "TTL in seconds for Vault tokens issued to aisa and its proxy."
  type        = number
  default     = 3600
}

variable "vault_role_aisa" {
  description = "Name of the Vault policy, Kubernetes auth role, and Consul secrets engine role for aisa."
  type        = string
  default     = "aisa"
}

variable "vault_role_render" {
  description = "Name of the Vault policy, Kubernetes auth role, and Consul secrets engine role for the proxy renderer."
  type        = string
  default     = "aisa-render"
}

variable "extra_render_read_paths" {
  description = "Additional Vault paths that the proxy renderer role (aisa-render) may read, e.g. [\"gcp/roleset/vertex/token\"] for minted credentials."
  type        = list(string)
  default     = []
}

variable "enable_consul_acls" {
  description = "Create Consul ACL policies and Vault Consul secrets engine roles for aisa and aisa-render."
  type        = bool
  default     = true
}

variable "vault_consul_mount" {
  description = "Mount path of Vault's Consul secrets engine."
  type        = string
  default     = "consul"
}

variable "create_vault_consul_mount" {
  description = "Whether to mount and configure Vault's Consul secrets engine at vault_consul_mount."
  type        = bool
  default     = false
}

variable "consul_address" {
  description = "Consul address (host:port) for Vault's Consul secrets engine when create_vault_consul_mount is true."
  type        = string
  default     = ""
}

variable "consul_scheme" {
  description = "URL scheme (http or https) for Vault's Consul secrets engine when create_vault_consul_mount is true."
  type        = string
  default     = "http"
}

variable "consul_management_token" {
  description = "Consul token with acl = \"write\" for Vault's Consul secrets engine when create_vault_consul_mount is true."
  type        = string
  default     = ""
  sensitive   = true
}

variable "consul_role_ttl" {
  description = "TTL in seconds for Consul ACL tokens minted by Vault's Consul secrets engine."
  type        = number
  default     = 3600
}

variable "consul_kv_prefix" {
  description = "Consul KV prefix under which aisa reads declarations, profiles, models, prices, and budgets."
  type        = string
  default     = "aisa"
}

variable "quota_profiles" {
  description = "Quota profiles stored at <consul_kv_prefix>/quotas/<name>."
  type = map(object({
    tokens_per_hour = number
  }))
  default = {}

  validation {
    condition     = alltrue([for p in values(var.quota_profiles) : p.tokens_per_hour >= 0 && floor(p.tokens_per_hour) == p.tokens_per_hour])
    error_message = "quota_profiles tokens_per_hour must be a whole number >= 0."
  }
}

variable "consumers" {
  description = "Consumer declarations stored at <consul_kv_prefix>/consumers/<name>."
  type = map(object({
    quota_profile = optional(string)
    namespace     = optional(string)
    secret        = optional(string)
  }))
  default = {}
}

variable "models" {
  description = "Model descriptions stored at <consul_kv_prefix>/models/<name> for Kubernetes Model objects and model aliases."
  type = map(object({
    features       = optional(list(string))
    context_length = optional(number)
    capabilities   = optional(list(string))
    aliases        = optional(list(string))
  }))
  default = {}
}

variable "model_prices" {
  description = "Model prices stored at <consul_kv_prefix>/pricing/<name>."
  type = map(object({
    input_per_1k  = number
    output_per_1k = number
    currency      = string
  }))
  default = {}
}

variable "budgets" {
  description = "Monthly consumer token budgets stored at <consul_kv_prefix>/budgets/<consumer>."
  type = map(object({
    monthly_tokens = number
    soft_ratio     = optional(number, 0.8)
    on_exhausted   = optional(string, "reject")
    downgrade_to   = optional(string)
  }))
  default = {}

  validation {
    condition = alltrue([
      for b in values(var.budgets) : (
        b.monthly_tokens >= 0 &&
        floor(b.monthly_tokens) == b.monthly_tokens &&
        b.soft_ratio > 0 &&
        b.soft_ratio <= 1 &&
        contains(["reject", "downgrade"], trimspace(b.on_exhausted)) &&
        (trimspace(b.on_exhausted) != "downgrade" || (b.downgrade_to != null && trimspace(b.downgrade_to) != ""))
      )
    ])
    error_message = "budgets require whole monthly_tokens >= 0, 0 < soft_ratio <= 1, on_exhausted in [\"reject\", \"downgrade\"], and non-empty downgrade_to when on_exhausted == \"downgrade\"."
  }
}

variable "provider_budgets" {
  description = "Monthly provider-account token budgets stored at <consul_kv_prefix>/budgets/providers/<key>."
  type = map(object({
    monthly_tokens = number
    soft_ratio     = optional(number, 0.8)
    on_exhausted   = optional(string, "reject")
    downgrade_to   = optional(string)
  }))
  default = {}

  validation {
    condition = alltrue([
      for b in values(var.provider_budgets) : (
        b.monthly_tokens >= 0 &&
        floor(b.monthly_tokens) == b.monthly_tokens &&
        b.soft_ratio > 0 &&
        b.soft_ratio <= 1 &&
        contains(["reject", "downgrade"], trimspace(b.on_exhausted)) &&
        (trimspace(b.on_exhausted) != "downgrade" || (b.downgrade_to != null && trimspace(b.downgrade_to) != ""))
      )
    ])
    error_message = "provider_budgets require whole monthly_tokens >= 0, 0 < soft_ratio <= 1, on_exhausted in [\"reject\", \"downgrade\"], and non-empty downgrade_to when on_exhausted == \"downgrade\"."
  }
}

variable "backends" {
  description = "Inference backends registered in the Consul catalog as service aisa-backend."
  type = map(object({
    node_name   = string
    address     = string
    create_node = optional(bool, true)
    port        = number
    service_id  = optional(string)
    tags        = optional(list(string), [])
    provider    = optional(string, "openai-compatible")
    scheme      = optional(string)
    path        = optional(string)
    models      = list(string)
    tier        = optional(string)
    priority    = optional(number, 1)
    weight      = optional(number)
    timeout     = optional(number)
    fail_policy = optional(string, "closed")
    key         = optional(string)
    key_path    = optional(string)
    key_field   = optional(string)
    check = optional(object({
      check_id = optional(string)
      name     = optional(string)
      http     = optional(string)
      tcp      = optional(string)
      interval = optional(string, "30s")
      timeout  = optional(string, "5s")
      status   = optional(string, "passing")
    }))
  }))
  default = {}

  validation {
    condition = alltrue([
      for b in values(var.backends) : (
        b.timeout == null || (b.timeout >= 1 && floor(b.timeout) == b.timeout)
      )
    ])
    error_message = "backends timeout must be a whole number >= 1 second when set."
  }
}
