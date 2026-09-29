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
secret/aisa/consumers/chat-ui     key_sha256=<hash>  quota_profile=interactive
secret/aisa/consumers/batch-jobs  key_sha256=<hash>  quota_profile=batch
```
- A consumer has no fail policy: when aisa is unreachable the gateway cannot tell who the consumer is, so the policy belongs to the backend ([`CONSUL.md`](./CONSUL.md#backends-consul-catalog)).
- Vault stores only the **SHA-256 of the key**. The plaintext key is shown once when created and handed to the client.
- aisa caches the consumer list, refreshing it every 60 s (`AISA_CONSUMER_REFRESH`) and when a key is not found, so key validation does not call Vault on every request. Unknown keys cause at most one reload per 5 s (`AISA_CONSUMER_MISS_REFRESH`), and a request waits at most 2 s for it, so random keys cannot flood Vault or stall the gateway.
- A consumer's name is the last element of its path, and any name that Vault accepts works.
- When Vault cannot be read, aisa keeps deciding with the consumers it has, and tries again after 1 s, then 2 s, 4 s and so on up to the refresh interval. This also covers a Vault that is not ready when aisa starts.
- When a single consumer cannot be read (denied by policy, a broken secret), the others are loaded all the same. That consumer keeps the keys it had when it was last read, for `AISA_CONSUMER_MAX_STALE` at most, and is counted in `aisa_consumers_unreadable`. A new consumer that cannot be read cannot authenticate.
- A path without consumers is an empty list, and every key is rejected. A KV mount that does not exist, or an answer that does not come from Vault, is a failed load instead, so a mistyped `AISA_VAULT_KV_MOUNT` does not lock out the consumers aisa has in memory.
- When Vault cannot be read at all, aisa keeps deciding with the consumers it has. After 15 min without a successful reload (`AISA_CONSUMER_MAX_STALE`) it fails closed: `/v1/decide` answers 503 and `/readyz` reports `consumers` as failed, because a key revoked in that time would otherwise still be accepted.
- aisa logs in to Vault again when Vault no longer accepts its token, not when a policy denies a path: a denied request would otherwise leave a new token behind every time.
- Rotation: `key_sha256` holds one hash, or several separated by commas, spaces or line ends. Add the new key's hash next to the old one, move the client to the new key, then remove the old hash. A hash that two consumers share authenticates neither, and is logged as an error.
- aisa logs in with Kubernetes auth (role `aisa`, `AISA_VAULT_K8S_ROLE`). In development `VAULT_TOKEN` replaces the login. The KV mount and the `aisa/` prefix are `AISA_VAULT_KV_MOUNT` and `AISA_VAULT_PREFIX`.

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
