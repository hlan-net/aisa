locals {
  kv_mount     = trim(var.vault_kv_mount, "/")
  kv_prefix    = trim(var.vault_kv_prefix, "/")
  consul_mount = trim(var.vault_consul_mount, "/")
  consul_kv    = trim(var.consul_kv_prefix, "/")

  backend_nodes = {
    for node_name, backends in {
      for k, b in var.backends : b.node_name => b.address... if b.create_node
    } : node_name => backends[0]
  }
}

resource "vault_mount" "kv" {
  count   = var.create_vault_kv_mount ? 1 : 0
  path    = local.kv_mount
  type    = "kv"
  options = { version = "2" }
}

resource "vault_consul_secret_backend" "consul" {
  count   = var.create_vault_consul_mount ? 1 : 0
  path    = local.consul_mount
  address = var.consul_address
  scheme  = var.consul_scheme
  token   = var.consul_management_token
}

resource "vault_policy" "aisa" {
  name = var.vault_role_aisa

  policy = join("\n\n", compact([
    <<-EOT
    path "${local.kv_mount}/data/${local.kv_prefix}/consumers/*" {
      capabilities = ["create", "read", "update"]
    }

    path "${local.kv_mount}/metadata/${local.kv_prefix}/consumers" {
      capabilities = ["list"]
    }

    path "${local.kv_mount}/metadata/${local.kv_prefix}/consumers/*" {
      capabilities = ["list", "delete"]
    }
    EOT
    ,
    var.enable_consul_acls ? <<-EOT
    path "${local.consul_mount}/creds/${var.vault_role_aisa}" {
      capabilities = ["read"]
    }
    EOT
    : "",
  ]))
}

resource "vault_policy" "aisa_render" {
  name = var.vault_role_render

  policy = join("\n\n", compact(concat(
    [
      <<-EOT
      path "${local.kv_mount}/data/${local.kv_prefix}/providers/*" {
        capabilities = ["read"]
      }
      EOT
      ,
      var.enable_consul_acls ? <<-EOT
      path "${local.consul_mount}/creds/${var.vault_role_render}" {
        capabilities = ["read"]
      }
      EOT
      : "",
    ],
    [
      for p in var.extra_render_read_paths : <<-EOT
      path "${trim(p, "/")}" {
        capabilities = ["read"]
      }
      EOT
    ]
  )))
}

resource "vault_kubernetes_auth_backend_role" "aisa" {
  backend                          = trim(var.vault_kubernetes_mount, "/")
  role_name                        = var.vault_role_aisa
  bound_service_account_names      = [var.aisa_service_account]
  bound_service_account_namespaces = [var.namespace]
  audience                         = var.vault_kubernetes_audience != "" ? var.vault_kubernetes_audience : null
  token_policies                   = [vault_policy.aisa.name]
  token_ttl                        = var.vault_token_ttl
}

resource "vault_kubernetes_auth_backend_role" "aisa_render" {
  backend                          = trim(var.vault_kubernetes_mount, "/")
  role_name                        = var.vault_role_render
  bound_service_account_names      = [var.proxy_service_account]
  bound_service_account_namespaces = [var.namespace]
  audience                         = var.vault_kubernetes_audience != "" ? var.vault_kubernetes_audience : null
  token_policies                   = [vault_policy.aisa_render.name]
  token_ttl                        = var.vault_token_ttl
}

resource "consul_acl_policy" "aisa" {
  count = var.enable_consul_acls ? 1 : 0
  name  = var.vault_role_aisa

  rules = <<-EOT
    key_prefix "${local.consul_kv}/" {
      policy = "write"
    }
    service "aisa-backend" {
      policy = "read"
    }
    node_prefix "" {
      policy = "read"
    }
  EOT
}

resource "consul_acl_policy" "aisa_render" {
  count = var.enable_consul_acls ? 1 : 0
  name  = var.vault_role_render

  rules = <<-EOT
    service "aisa-backend" {
      policy = "read"
    }
    node_prefix "" {
      policy = "read"
    }
  EOT
}

resource "vault_consul_secret_backend_role" "aisa" {
  count           = var.enable_consul_acls ? 1 : 0
  name            = var.vault_role_aisa
  backend         = var.create_vault_consul_mount ? vault_consul_secret_backend.consul[0].path : local.consul_mount
  consul_policies = [consul_acl_policy.aisa[0].name]
  ttl             = var.consul_role_ttl
}

resource "vault_consul_secret_backend_role" "aisa_render" {
  count           = var.enable_consul_acls ? 1 : 0
  name            = var.vault_role_render
  backend         = var.create_vault_consul_mount ? vault_consul_secret_backend.consul[0].path : local.consul_mount
  consul_policies = [consul_acl_policy.aisa_render[0].name]
  ttl             = var.consul_role_ttl
}

resource "consul_keys" "quota_profiles" {
  for_each = var.quota_profiles

  key {
    path = "${local.consul_kv}/quotas/${each.key}"
    value = jsonencode({
      tokens_per_hour = each.value.tokens_per_hour
    })
  }
}

resource "consul_keys" "consumers" {
  for_each = var.consumers

  key {
    path = "${local.consul_kv}/consumers/${each.key}"
    value = jsonencode({
      for k, v in {
        quota_profile = each.value.quota_profile
        namespace     = each.value.namespace
        secret        = each.value.secret
      } : k => v if v != null
    })
  }
}

resource "consul_keys" "models" {
  for_each = var.models

  key {
    path = "${local.consul_kv}/models/${each.key}"
    value = jsonencode({
      for k, v in {
        features      = each.value.features
        contextLength = each.value.context_length
        capabilities  = each.value.capabilities
        aliases       = each.value.aliases
      } : k => v if v != null
    })
  }
}

resource "consul_keys" "model_prices" {
  for_each = var.model_prices

  key {
    path = "${local.consul_kv}/pricing/${each.key}"
    value = jsonencode({
      input_per_1k  = each.value.input_per_1k
      output_per_1k = each.value.output_per_1k
      currency      = each.value.currency
    })
  }
}

resource "consul_keys" "budgets" {
  for_each = var.budgets

  key {
    path = "${local.consul_kv}/budgets/${each.key}"
    value = jsonencode({
      for k, v in {
        monthly_tokens = each.value.monthly_tokens
        soft_ratio     = each.value.soft_ratio
        on_exhausted   = each.value.on_exhausted
        downgrade_to   = each.value.downgrade_to
      } : k => v if v != null
    })
  }
}

resource "consul_keys" "provider_budgets" {
  for_each = var.provider_budgets

  key {
    path = "${local.consul_kv}/budgets/providers/${each.key}"
    value = jsonencode({
      for k, v in {
        monthly_tokens = each.value.monthly_tokens
        soft_ratio     = each.value.soft_ratio
        on_exhausted   = each.value.on_exhausted
        downgrade_to   = each.value.downgrade_to
      } : k => v if v != null
    })
  }
}

resource "consul_node" "backend" {
  for_each = local.backend_nodes

  name    = each.key
  address = each.value

  lifecycle {
    precondition {
      condition     = length(distinct([for k, b in var.backends : b.address if b.create_node && b.node_name == each.key])) == 1
      error_message = "Backends sharing a node_name with create_node = true must have the same address."
    }
  }
}

resource "consul_service" "backend" {
  for_each = var.backends

  name       = "aisa-backend"
  service_id = coalesce(each.value.service_id, each.key)
  node       = each.value.create_node ? consul_node.backend[each.value.node_name].name : each.value.node_name
  port       = each.value.port
  tags       = each.value.tags

  meta = {
    for k, v in {
      provider    = each.value.provider
      scheme      = each.value.scheme
      path        = each.value.path
      models      = join(",", each.value.models)
      tier        = each.value.tier
      priority    = tostring(each.value.priority)
      weight      = each.value.weight != null ? tostring(each.value.weight) : null
      timeout     = each.value.timeout != null ? tostring(each.value.timeout) : null
      fail_policy = each.value.fail_policy
      key         = each.value.key
      key_path    = each.value.key_path
      key_field   = each.value.key_field
    } : k => v if v != null
  }

  dynamic "check" {
    for_each = each.value.check != null ? [each.value.check] : []
    content {
      check_id = coalesce(check.value.check_id, "service:${coalesce(each.value.service_id, each.key)}")
      name     = coalesce(check.value.name, each.key)
      http     = check.value.http
      tcp      = check.value.tcp
      interval = check.value.interval
      timeout  = check.value.timeout
      status   = check.value.status
    }
  }

  lifecycle {
    precondition {
      condition     = length(distinct([for k, b in var.backends : coalesce(b.service_id, k)])) == length(var.backends)
      error_message = "Resolved backend service_id values must be unique across the catalog."
    }
  }
}
