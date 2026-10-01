# APISIX proxy: the Vault Agent logs in to Vault with the pod's service account and keeps two
# tokens in memory for consul-template, which cannot log in by itself (docs/concepts/VAULT.md):
#
#   /vault/token          the Vault token of the role aisa-render: reads secret/aisa/providers/*
#   /vault/consul-token   a Consul ACL token from Vault's Consul secrets engine
#
# The address comes from VAULT_ADDR. The auth mount, the role and the Consul secrets mount are
# the names of the Terraform module; change them here when the module is given other names.

auto_auth {
  method "kubernetes" {
    mount_path = "auth/kubernetes"
    config = {
      role       = "aisa-render"
      token_path = "/var/run/secrets/aisa/serviceaccount/token"
    }
  }

  sink "file" {
    config = {
      path = "/vault/token"
      # consul-template runs as another user. The directory is in memory and mounted into
      # these two containers only, not into the gateway's.
      mode = 0644
    }
  }
}

template {
  contents    = "{{ with secret \"consul/creds/aisa-render\" }}{{ .Data.token }}{{ end }}"
  destination = "/vault/consul-token"
  perms       = "0644"
}
