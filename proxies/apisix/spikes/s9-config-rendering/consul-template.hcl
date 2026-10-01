# Spike S9: consul-template renders apisix.yaml from the Consul catalog and Vault.
# Dev only: a real deployment logs in to Vault with Kubernetes auth (docs/concepts/VAULT.md).

consul {
  address = "consul:8500"
}

vault {
  address     = "http://vault:8200"
  token       = "dev-root"
  renew_token = false

  # A KV secret has no lease, so consul-template reads it again after this time (5 minutes by
  # default). It is how long a rotated provider key takes to reach the gateway.
  default_lease_duration = "10s"
}

template {
  source      = "/spike/apisix.yaml.ctmpl"
  destination = "/rendered/apisix.yaml"
  perms       = 0644

  # A missing meta key or secret field stops the render instead of writing an empty value.
  error_on_missing_key = true

  # Several changes at once (a machine with many models going down) become one render.
  wait {
    min = "1s"
    max = "5s"
  }
}
