# APISIX proxy: consul-template renders apisix.yaml from the Consul catalog and Vault.
# The template renders into a staging file, and guard.sh checks and promotes it (#18).
#
# Nothing here names an environment. The addresses come from CONSUL_HTTP_ADDR and VAULT_ADDR,
# and the tokens from the files that the Vault Agent next to it writes
# (-vault-agent-token-file and -consul-token-file in chart/templates/deployment.yaml), or from
# VAULT_TOKEN and CONSUL_HTTP_TOKEN in development.

vault {
  # The Vault Agent renews the token it wrote; consul-template only reads it.
  renew_token = false

  # A KV v2 secret has no lease, so consul-template polls it after default_lease_duration.
  # This sets how long a rotated provider key takes to reach the gateway.
  default_lease_duration = "10s"
}

template {
  source      = "/etc/apisix/aisa/apisix.yaml.ctmpl"
  destination = "/rendered/apisix.yaml.staged"
  command     = "/etc/apisix/aisa/guard.sh /rendered/apisix.yaml.staged /rendered/apisix.yaml"
  perms       = 0644

  # A missing meta key or secret field stops the render instead of writing an empty value.
  error_on_missing_key = true

  wait {
    min = "1s"
    max = "5s"
  }
}
