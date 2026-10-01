# Spike S5: consul-template next to APISIX in one pod. The backend of the spike needs no key,
# so there is no vault block; with one, consul-template logs in to Vault with the pod's service
# account (docs/concepts/VAULT.md).

consul {
  address = "consul:8500"
}

template {
  source      = "/spike/apisix.yaml.ctmpl"
  destination = "/rendered/apisix.yaml"
  perms       = 0644

  error_on_missing_key = true

  wait {
    min = "1s"
    max = "5s"
  }
}
