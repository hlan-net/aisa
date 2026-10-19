# Admin Interface & Management API

## Concept

aisa provides an embedded Web UI and REST Administration API at `/admin` to inspect and manage consumers, quota profiles, backends, and models without requiring direct manipulation of Consul KV and Vault KV v2 storage paths.

The administration interface follows aisa's core design principles:
- **Consul and Vault remain the sources of truth**: the admin interface reads from and writes to Consul KV and Vault KV v2. It stores nothing in local process memory or separate databases.
- **Zero external dependencies**: the Web UI is self-contained and compiled directly into the single Go binary using `embed.FS`. It requires no external Node/npm build steps, no external CDN assets, and runs airgapped on both `linux/amd64` and `linux/arm64`.
- **Secrets never leak**: consumer API keys are generated as cryptographically secure random tokens (`aisa-<hex>`). Vault stores only the SHA-256 hash. The plaintext key is displayed exactly once upon creation or rotation and cannot be retrieved afterwards.

---

## Architecture and Endpoints

When enabled (via `AISA_ADMIN_ENABLED=true`, disabled by default), aisa serves the Web UI and JSON API on the primary listener address (`AISA_ADDR`):

```
Browser / Client ──▶ GET /admin ────────▶ 301 Redirect to /admin/
Browser / Client ──▶ GET /admin/ ───────▶ Embedded HTML Dashboard (Single-Page App)
Browser / Client ──▶ GET /admin/assets/*▶ Embedded CSS and Vanilla JavaScript
API Client       ──▶ /admin/api/* ──────▶ REST Administration JSON API
```

### Configuration

| Environment Variable | Description | Default |
|----------------------|-------------|---------|
| `AISA_ADMIN_ENABLED` | Enable the admin dashboard and API under `/admin` | `false` |
| `AISA_ADDR` | Primary listening address | `:8080` |
| `AISA_VAULT_KV_MOUNT` | KV v2 mount in Vault for consumer key hashes | `secret` |
| `AISA_VAULT_PREFIX` | Secret path prefix in Vault | `aisa` |
| `CONSUL_HTTP_ADDR` | Consul agent address | required |
| `AISA_CONSUL_PREFIX` | Key prefix in Consul KV | `aisa` |

---

## Features

### 1. Dashboard Overview
Displays real-time system readiness checks (Vault connectivity, Consul declarations, Redis quota store, and loaded consumers), as well as live gauges for declared consumers, keys present, quota profiles, active backends, and registered models.

### 2. Consumer Management
- **List consumers**: Displays declared consumers in Consul KV (`aisa/consumers/<name>`), their assigned quota profile, Kubernetes namespace and Secret binding, Vault key hash status, and synchronization state (`ready`, `missing_key`, `undeclared`, `invalid`).
- **Create consumer**: Declares a consumer in Consul KV with optional quota profile, Kubernetes target Secret, and either automated key generation or custom key specification (minimum 16 characters). Key issuance is supported for standalone consumers; consumers bound to a Kubernetes Secret have their keys managed by Kubernetes reconciliation. Checks existence with check-and-set (CAS) and returns 409 Conflict if the consumer already exists.
- **Rotate key**: Generates a new API key for standalone consumers, replaces the key hash in Vault with check-and-set concurrency protection, and reveals the plaintext key once. Refuses rotation for Kubernetes-managed consumers (handled via Secret reconciliation).
- **Delete consumer**: Removes the declaration from Consul KV and permanently deletes the key metadata and versions from Vault KV v2.

### 3. Quota Profiles
- **List quota profiles**: Inspects rate-limiting profiles configured in Consul KV (`aisa/quotas/<name>`).
- **Create quota profile**: Creates hourly token limits (`tokens_per_hour`) using check-and-set (returns 409 Conflict if already exists), with preset buttons (100k, 500k, 1M, 5M).
- **Delete quota profile**: Deletes profiles no longer in use.

### 4. Backends & Models Catalog
- **Backends**: Discovers instances of `aisa-backend` registered in the Consul service catalog, listing their IP/port, priority, fail policy, check status, and supported models.
- **Models**: Summarizes available models mapped across discovered backends and indicates overall model health (healthy if at least one backend providing the model passes checks).

---

## REST API Reference

All API endpoints return standard JSON responses with appropriate HTTP status codes.

### Overview
`GET /admin/api/overview`
Returns system status, readiness probe states, and entity counts.

### Consumers
- `GET /admin/api/consumers`
  Returns an array of consumers with their declaration and key status.
- `POST /admin/api/consumers`
  Creates a consumer declaration in Consul and stores its key hash in Vault.
  Payload:
  ```json
  {
    "name": "chat-app",
    "quota_profile": "interactive",
    "namespace": "frontend",
    "secret": "aisa-token",
    "generate_key": true
  }
  ```
  Response (201 Created):
  ```json
  {
    "name": "chat-app",
    "quota_profile": "interactive",
    "has_key": true,
    "status": "ready",
    "plaintext_key": "aisa-...",
    "message": "Consumer created. Save this API key now; it cannot be shown again."
  }
  ```
- `POST /admin/api/consumers/{name}/rotate-key`
  Rotates the key for the given standalone consumer by replacing the key hash in Vault with check-and-set concurrency protection. Returns the newly generated plaintext key. Refuses rotation for Kubernetes-managed consumers.
- `DELETE /admin/api/consumers/{name}`
  Deletes the consumer declaration from Consul and removes its key from Vault.

### Quotas
- `GET /admin/api/quotas`
  Lists all quota profiles and their tokens per hour limit.
- `POST /admin/api/quotas`
  Creates a quota profile in Consul KV (checks existence with check-and-set and returns 409 Conflict if already declared).
  Payload:
  ```json
  {
    "name": "interactive",
    "tokens_per_hour": 1000000
  }
  ```
- `DELETE /admin/api/quotas/{name}`
  Deletes a quota profile from Consul KV.

### Backends & Models
- `GET /admin/api/backends`
  Lists instances of `aisa-backend` discovered from the Consul service catalog.
- `GET /admin/api/models`
  Lists unique models aggregate across healthy backends.

---

## Ingress & Authentication in Kubernetes

aisa's Helm chart (`deploy/helm/aisa`) provides an optional Ingress resource (`admin.ingress.enabled`) for exposing the Admin UI.

To keep aisa lean and avoid managing session tokens or user passwords in application code, standard ingress authentication proxies (such as `oauth2-proxy`, Traefik `ForwardAuth` middleware, Pomerium, or Cloudflare Access) can be placed in front of the Admin Ingress, authenticating users against HashiCorp Vault (acting as an OIDC Provider) or an enterprise IdP:

```yaml
admin:
  enabled: true
  ingress:
    enabled: true
    className: traefik
    annotations:
      # Traefik middleware / oauth2-proxy forward-auth
      traefik.ingress.kubernetes.io/router.middlewares: default-oauth2-proxy@kubernetescrd
    hosts:
      - host: aisa-admin.example.com
        paths:
          - path: /admin
            pathType: Prefix
    extraFrom:
      # Allow ingress controller traffic through NetworkPolicy to port 8080
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: kube-system
```

---

## Security Considerations

- **No Built-in Authentication**: The `/admin` Web UI and `/admin/api` management endpoints perform no internal authentication. In-cluster, the API port is reachable by the proxy and by any pod if NetworkPolicies are disabled. Ingress authentication annotations protect only the Ingress path, not internal ClusterIP calls. When exposing `/admin` externally, always configure authentication annotations on the Ingress (such as Traefik forward-auth, OIDC, or oauth2-proxy). For this reason, the admin interface is disabled by default (`AISA_ADMIN_ENABLED=false` and `admin.enabled=false`).
- **CSRF & Request Validation**: Mutating API endpoints (`POST`, `PUT`, `DELETE`) require `Content-Type: application/json` and reject cross-site browser requests (`Sec-Fetch-Site: cross-site`), preventing CSRF attacks when deployed behind cookie-authenticated forward-auth proxies.
- **Name Restrictions**: Declared consumer names and quota profile names must start with an alphanumeric character and are strictly limited to `[A-Za-z0-9][A-Za-z0-9._-]*` (1 to 253 characters, excluding `.` and `..`) and cannot be `unknown`.
- **Vault and Consul ACLs**: aisa interacts with Consul and Vault using its configured service tokens. In production Kubernetes clusters, aisa authenticates via Kubernetes ServiceAccount auth to Vault and obtains Consul ACL tokens via Vault Agent. Consumer key deletion permanently purges metadata and versions via `DELETE secret/metadata/aisa/consumers/<name>`, requiring the `delete` capability on metadata.
- **Key Safety**: Plaintext keys are generated in-memory using crypto-secure randomness (`crypto/rand`) and hashed with SHA-256 before writing to Vault. Plaintext keys are never logged and never stored in Consul or Vault. Standalone key rotation updates `key_sha256` using check-and-set concurrency protection.
