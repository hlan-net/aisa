# aisa's Helm chart

Installs aisa, a Redis for its quota counters and its proxy in one namespace ([`ARCHITECTURE.md`](../../../docs/concepts/ARCHITECTURE.md#packaging-and-deployment)). The proxy is the chart of [`proxies/apisix/chart`](../../../proxies/apisix/chart/), a dependency of this one.

```bash
helm install aisa oci://ghcr.io/hlan-net/charts/aisa --version <x.y.z> -n aisa --create-namespace \
  --set global.vault.addr=https://vault.vault.svc:8200 \
  --set global.consul.addr=consul-server.consul.svc:8500
```

Applications call the proxy's Service, `aisa-apisix` for a release named `aisa`, at `http://aisa-apisix.aisa.svc/v1`, never aisa itself.

## What it installs

| Object | What it is |
|---|---|
| Deployment `<release>` | aisa, and a Vault Agent next to it that keeps a Consul ACL token in memory (`consul.token.fromVault`) |
| Service `<release>` | `/v1/decide`, `/v1/usage` and `/metrics` on port 8080, for the proxy and Prometheus |
| Deployment, Service `<release>-redis` | Redis for the quota counters, nothing on disk (`redis.enabled`) |
| NetworkPolicies | Only the proxy, and the peers in `networkPolicy.extraFrom`, reach aisa; only aisa reaches Redis |
| ServiceMonitor | Optional (`serviceMonitor.enabled`) |
| The proxy | Everything of the proxy's chart, `<release>-apisix` (`apisix.enabled`; its values under `apisix:`) |

A release name that does not contain `aisa` gets `-aisa` appended to aisa's objects (`prod-aisa`); the proxy derives aisa's address the same way.

## Before the install

The chart creates no Vault or Consul configuration; the Terraform module (ROADMAP v0.2.0 PR 6) is to. Until then, [`../test/install_test.sh`](../test/install_test.sh) shows every step for a test cluster.

- **Vault**, Kubernetes auth: the role `aisa` bound to the service account `<release>` in the release's namespace, reading `secret/aisa/consumers/*` and `consul/creds/aisa`; the role `aisa-render` bound to `<release>-apisix`, reading `secret/aisa/providers/*` and `consul/creds/aisa-render` ([`VAULT.md`](../../../docs/concepts/VAULT.md)).
- **Consul**, with ACLs: the policies `aisa` and `aisa-render` ([`CONSUL.md`](../../../docs/concepts/CONSUL.md#acls)), as roles of Vault's Consul secrets engine. Without ACLs, set `consul.token.fromVault: false` and `apisix.vaultAgent.consulToken.enabled: false`.
- **Kubernetes** 1.29 or newer: the Vault Agents run as sidecar init containers.

## Values

The full list with comments is in [`values.yaml`](./values.yaml). The ones an install usually sets:

| Value | Default | |
|---|---|---|
| `global.vault.addr`, `global.consul.addr` | (required) | Shared with the proxy |
| `vault.auth.role`, `vault.auth.mount`, `vault.auth.audience` | `aisa`, `kubernetes`, none | aisa's login |
| `consul.token.fromVault`, `consul.token.path` | `true`, `consul/creds/aisa` | aisa's Consul token |
| `quotas.enabled` | `true` | Off: no Consul and no Redis for aisa |
| `redis.enabled`, `redis.addr` | `true`, none | Off: an external Redis at `redis.addr` |
| `networkPolicy.extraFrom` | the namespace `monitoring` | Who else may reach aisa's port |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | none | e.g. `VAULT_CACERT` with a CA from a ConfigMap |
| `apisix.*` | | The proxy's values ([`proxies/apisix/chart/values.yaml`](../../../proxies/apisix/chart/values.yaml)) |

## Tests

- [`../test/charts_test.sh`](../test/charts_test.sh): `helm lint`, and `kubeconform` for the usual combinations of values. Needs no cluster.
- [`../test/install_test.sh`](../test/install_test.sh): installs both charts into the current kubectl context (CI: kind on amd64 and arm64) with Vault, Consul with ACLs and two mock backends, and checks the logins, the Consul tokens from Vault, a provider key rendered from Vault, requests and usage through the proxy, the NetworkPolicy, and that `helm uninstall` removes everything.
