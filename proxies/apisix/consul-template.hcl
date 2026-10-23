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

consul {
  # A query answered 403 (ACL not found) is retried until it succeeds, never given up on (#90).
  # The Consul token comes from Vault's Consul secrets engine and is rotated as often as the
  # lease of the consul/ mount allows; consul-token-watch.sh reloads consul-template with the
  # new one, and a query still in flight with the old one, or the new one before Consul has
  # replicated it, is answered 403 until then. With the default of 12 attempts that window
  # could end the process. A failure that lasts is visible in the log, and the gateway keeps
  # serving the last config it promoted.
  retry {
    attempts    = 0
    backoff     = "250ms"
    max_backoff = "1m"
  }
}

template {
  source     = "/etc/apisix/aisa/apisix.yaml.ctmpl"
  destination = "/rendered/apisix.yaml.staged"
  command     = "/etc/apisix/aisa/guard.sh /rendered/apisix.yaml.staged /rendered/apisix.yaml"
  perms       = 0644

  # consul-template exits when the command takes longer. guard.sh waits for its lock 20 s at
  # most (GUARD_LOCK_WAIT), for APISIX 10 s (GUARD_VERIFY_TIMEOUT), and a few seconds more for
  # the file and the Control API; the default of 30 s is too close to that.
  command_timeout = "60s"

  # A missing meta key or secret field stops the render instead of writing an empty value.
  error_on_missing_key = true

  wait {
    min = "1s"
    max = "5s"
  }
}
