# Concept: Vault integration

Related: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`CONSUL.md`](./CONSUL.md), [`../../ROADMAP.md`](../../ROADMAP.md).

## Goals

- No secrets in Git or in any committed gateway config.
- Provider keys (OpenAI and others) live in Vault. Clients never see them.
- Consumer credentials live in Vault and can be rotated there.
- Every component authenticates to Vault with short-lived credentials (Kubernetes auth), never a static token.
- **Gateways never talk to Vault directly.** That keeps adapters simple and avoids depending on each gateway's own Vault support.

## Who reads what

The mount name (`secret/`) and the `aisa/` prefix are configurable in the Terraform module.

| Reader | Auth | Reads | Why |
|---|---|---|---|
| aisa | Kubernetes auth, role `aisa` | `secret/aisa/consumers/*` (KV v2) | Validates consumer credentials in `/v1/decide` |
| consul-template (next to the gateway) | Kubernetes auth, role `aisa-render` | `secret/aisa/providers/*` (KV v2), `consul/creds/aisa-render` | Renders provider keys into the gateway config |

Each role can only read its own paths.

## Consumer credentials

### First version: consumer keys
```
secret/aisa/consumers/chat-ui     key_sha256=<hash>  fail_policy=open    quota_profile=interactive
secret/aisa/consumers/batch-jobs  key_sha256=<hash>  fail_policy=closed  quota_profile=batch
```
- Vault stores only the **SHA-256 of the key**. The plaintext key is shown once when created and handed to the client.
- aisa caches the consumer list, refreshing it every 60 s and when a key is not found, so key validation does not call Vault on every request.
- Rotation: write a second hash, move the client to the new key, then delete the old hash.

### Later: Vault-issued JWTs
Clients authenticate with **JWTs from Vault's identity/OIDC provider** instead of static keys. Pods log in with Kubernetes auth, and people log in through an external identity provider via Vault's OIDC auth method (e.g. Entra ID). aisa validates the JWT against Vault's JWKS and maps a claim to the consumer. No long-lived gateway keys remain, and no gateway change is needed, because authentication happens in aisa.

## Provider keys

```
secret/aisa/providers/openai   api_key=<key>
```
A backend in Consul names its secret with the service meta `key` (`key = "openai"` for the one above). consul-template renders these into the gateway config (for APISIX, the `ai-proxy-multi` `auth.header`) inside the pod. It reads a secret again after `default_lease_duration`, which the template sets: a rotated key reaches the gateway within that time (spike S9). The rendered file is on an in-memory `emptyDir` and never written to disk.

Where the provider supports it, dynamic credentials are better than static keys, e.g. Azure OpenAI through Vault's Azure secrets engine.

## APISIX's own Vault support

APISIX's `$secret://vault/...` supports only KV v1 and a static token ([docs](https://apisix.apache.org/docs/apisix/terminology/secret/)). With config rendering, aisa does not need it. Adding KV v2 and Kubernetes auth to APISIX is still a worthwhile, independent upstream contribution ([`ROADMAP.md`](../../ROADMAP.md)).
