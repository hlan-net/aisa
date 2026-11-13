output "vault_policy_aisa" {
  description = "Name of the Vault policy created for aisa."
  value       = vault_policy.aisa.name
}

output "vault_policy_render" {
  description = "Name of the Vault policy created for the proxy renderer."
  value       = vault_policy.aisa_render.name
}

output "vault_kubernetes_role_aisa" {
  description = "Name of the Vault Kubernetes auth role for aisa."
  value       = vault_kubernetes_auth_backend_role.aisa.role_name
}

output "vault_kubernetes_role_render" {
  description = "Name of the Vault Kubernetes auth role for the proxy renderer."
  value       = vault_kubernetes_auth_backend_role.aisa_render.role_name
}

output "consul_acl_policy_aisa" {
  description = "Name of the Consul ACL policy for aisa, if Consul ACLs are enabled."
  value       = var.enable_consul_acls ? consul_acl_policy.aisa[0].name : null
}

output "consul_acl_policy_render" {
  description = "Name of the Consul ACL policy for the proxy renderer, if Consul ACLs are enabled."
  value       = var.enable_consul_acls ? consul_acl_policy.aisa_render[0].name : null
}
