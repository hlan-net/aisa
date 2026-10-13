# Admin Interface & Management API

## Concept

aisa provides an embedded Web UI and REST Administration API at `/admin` to inspect and manage consumers, quota profiles, backends, and models without requiring direct manipulation of Consul KV and Vault KV v2 storage paths.

The administration interface follows aisa's core design principles:
- **Consul and Vault remain the sources of truth**: the admin interface reads from and writes to Consul KV and Vault KV v2. It stores nothing in local process memory or separate databases.
- **Zero external dependencies**: the Web UI is self-contained and compiled directly into the single Go binary using `embed.FS`. It requires no external Node/npm build steps, no external CDN assets, and runs airgapped on both `linux/amd64` and `linux/arm64`.
- **Secrets never leak**: consumer API keys are generated as cryptographically secure random tokens (`aisa_sec_<hex>`). Vault stores only the SHA-256 hash. The plaintext key is displayed exactly once upon creation or rotation and cannot be retrieved afterwards.

---

## Architecture and Endpoints

When enabled (via `AISA_ADMIN_ENABLED=true`, enabled by default), aisa serves the Web UI and JSON API on the primary listener address (`AISA_ADDR`):

```
Browser / Client ──▶ GET /admin ────────▶ 301 Redirect to /admin/
Browser / Client ──▶ GET /admin/ ───────▶ Embedded HTML Dashboard (Single-Page App)
Browser / Client ──▶ GET /admin/assets/*▶ Embedded CSS and Vanilla JavaScript
API Client       ──▶ /admin/api/* ──────▶ REST Administration JSON API
```

### Configuration

| Environment Variable | Description | Default |
|----------------------|-------------|---------|
| `AISA_ADMIN_ENABLED` | Enable the admin dashboard and API under `/admin` | `true` |
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
- **Create consumer**: Allows declaring a consumer in Consul with optional quota profile, Kubernetes target Secret, and either automated key generation or custom key specification.
- **Rotate key**: Generates a new API key, updates the SHA-256 hash in Vault KV v2 (`secret/data/aisa/consumers/<name>`), and returns the new plaintext key for one-time copying.
- **Delete consumer**: Removes the declaration from Consul KV and deletes the corresponding key hash from Vault KV v2.

### 3. Quota Profiles
- **List quota profiles**: Inspects rate-limiting profiles configured in Consul KV (`aisa/quotas/<name>`).
- **Create quota profile**: Configures hourly token limits (`tokens_per_hour`) with convenient preset buttons (100k, 500k, 1M, 5M).
- **Delete quota profile**: Deletes profiles no longer in use.

### 4. Backends & Models Catalog
- **Backends**: Discovers healthy instances of `aisa-backend` registered in the Consul service catalog, listing their IP/port, priority, fail policy, and supported models.
- **Models**: Summarizes available models mapped across discovered backends and indicates overall model health.

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
    "plaintext_key": "aisa_sec_...",
    "message": "Consumer created. Save this API key now; it cannot be shown again."
  }
  ```
- `POST /admin/api/consumers/{name}/rotate-key`
  Rotates the key for the given consumer. Returns the newly generated plaintext key.
- `DELETE /admin/api/consumers/{name}`
  Deletes the consumer declaration from Consul and removes its key from Vault.

### Quotas
- `GET /admin/api/quotas`
  Lists all quota profiles and their tokens per hour limit.
- `POST /admin/api/quotas`
  Creates or updates a quota profile in Consul KV.
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

## Security Considerations

- **Vault and Consul ACLs**: aisa interacts with Consul and Vault using its configured service tokens. In production Kubernetes clusters, aisa authenticates via Kubernetes ServiceAccount auth to Vault and obtains Consul ACL tokens via Vault Agent.
- **Key Safety**: Plaintext keys are generated in-memory using crypto-secure randomness (`crypto/rand`) and hashed with SHA-256 before writing to Vault. Plaintext keys are never logged and never stored in Consul or Vault.
- **Network Isolation**: When deployed in Kubernetes using the Helm chart (`deploy/helm/aisa`), aisa's API port is accessible within the cluster via ClusterIP. If desired, access to `/admin` can be restricted using Kubernetes NetworkPolicies, Ingress authentication proxies, or by disabling the admin interface via `admin.enabled: false` (or `AISA_ADMIN_ENABLED=false`).
