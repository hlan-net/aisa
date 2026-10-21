# aisa's Helm chart

Installs aisa, a Redis for its quota counters and its proxy in one namespace ([`ARCHITECTURE.md`](../../../docs/concepts/ARCHITECTURE.md#packaging-and-deployment)). The proxy is the chart of [`proxies/apisix/chart`](../../../proxies/apisix/chart/), a dependency of this one.

```bash
helm install aisa oci://ghcr.io/hlan-net/charts/aisa --version <x.y.z> -n aisa --create-namespace \
  --set global.vault.addr=https://vault.vault.svc:8200 \
  --set global.consul.addr=consul-server.consul.svc:8500
```

Applications call the proxy's Service, `aisa-apisix` for a release named `aisa`, at `http://aisa-apisix.aisa.svc/v1`, never aisa itself. How an application gets its credential, finds the models and handles the errors is in [`client-integration.md`](../../../docs/features/client-integration.md).

## What it installs

| Object | What it is |
|---|---|
| Deployment `<release>` | aisa, and a Vault Agent next to it that keeps a Consul ACL token in memory (`consul.token.fromVault`) |
| Service `<release>` | `/v1/decide`, `/v1/usage` and `/v1/render` on port 8080, for the proxy; always a ClusterIP, because `/v1/usage` and `/v1/render` have no authentication |
| Service `<release>-metrics` | `/metrics` (and the probes) on port 9090, for Prometheus; a ClusterIP. aisa serves them apart from the API (`AISA_METRICS_ADDR`, #46) |
| Deployment, Service `<release>-redis` | Redis for the quota counters, nothing on disk (`redis.enabled`) |
| NetworkPolicies | Only the proxy reaches aisa's API; the peers in `networkPolicy.extraFrom` (none by default) reach its metrics port only; only aisa reaches Redis |
| ServiceMonitor | Optional (`serviceMonitor.enabled`) |
| PrometheusRule | Optional (`prometheusRule.enabled`): the alerts of [`usage-metrics.md`](../../../docs/features/usage-metrics.md#alerts), on aisa's metrics; `AisaDown` watches the ServiceMonitor's job |
| CRD `models.aisa.hlan.net` | The `Model` objects (`crds.install`): installed and upgraded with the chart, kept when it is uninstalled. One install in a cluster owns it; set `crds.install=false` in any other |
| ClusterRole and binding `<namespace>-<release>` | aisa writes each consumer's Secret and the `Model` objects (`kubernetes.enabled`): Secrets in every namespace, changing only those with its own labels, and the Models |
| Role and binding `<release>-leader` | The Lease that elects the replica that writes the objects |
| ClusterRole `<namespace>-<release>-model-reader` | Reading the Models; bound to every service account unless `kubernetes.models.readableByAllServiceAccounts` is off |
| The proxy | Everything of the proxy's chart, `<release>-apisix` (`apisix.enabled`; its values under `apisix:`) |

A release name that does not contain `aisa` gets `-aisa` appended to aisa's objects (`prod-aisa`), unless `global.aisa.fullnameOverride` names them; the proxy derives aisa's address the same way.

## Before the install

The chart creates no Vault or Consul configuration; the Terraform module (ROADMAP v0.2.0 PR 6) is to. Until then, [`../test/install_test.sh`](../test/install_test.sh) shows every step for a test cluster.

- **Vault**, Kubernetes auth: the role `aisa` bound to the service account `<release>` in the release's namespace, reading, creating and updating `secret/aisa/consumers/*` (the consumers' key hashes; aisa writes those of the keys it issues) and reading `consul/creds/aisa`; the role `aisa-render` bound to `<release>-apisix`, reading `secret/aisa/providers/*` and `consul/creds/aisa-render` ([`VAULT.md`](../../../docs/concepts/VAULT.md)).
- **Consul**, with ACLs: the policies `aisa` and `aisa-render` ([`CONSUL.md`](../../../docs/concepts/CONSUL.md#acls)), as roles of Vault's Consul secrets engine. Without ACLs, set `consul.token.fromVault: false` and `apisix.vaultAgent.consulToken.enabled: false`.
- **Kubernetes** 1.29 or newer: the Vault Agents run as sidecar init containers.

## Values

The full list with comments is in [`values.yaml`](./values.yaml). The ones an install usually sets:

| Value | Default | |
|---|---|---|
| `global.vault.addr`, `global.consul.addr` | (required) | Shared with the proxy |
| `vault.auth.role`, `vault.auth.mount`, `vault.auth.audience` | `aisa`, `kubernetes`, `vault` | aisa's login; the Vault role binds the audience (`audience=vault`), which is required with `kubernetes.enabled`, so that the token in the Vault Agent is not valid against the Kubernetes API |
| `consul.token.fromVault`, `consul.token.path` | `true`, `consul/creds/aisa` | aisa's Consul token |
| `quotas.enabled` | `true` | Off: no Redis for aisa, and no quota is enforced; Consul is always read, the consumers are declared there |
| `timezone` | UTC | The time zone of the budgets' calendar month, an IANA name such as `Europe/Helsinki` |
| `quotas.defaultProfile` | none | The profile of a consumer whose declaration names none; without it such a consumer is denied (403 `no_quota`). It must be in Consul: aisa is not ready until it has read it, and `aisa_quota_profiles_missing` counts it absent later ([#82](https://github.com/hlan-net/aisa/issues/82)) |
| `redis.enabled`, `redis.addr` | `true`, none | Off: an external Redis at `redis.addr` |
| `kubernetes.enabled` | `true` | aisa issues the keys of the consumers whose declaration names a namespace and a Secret, writes them into those Secrets, and writes a `Model` per model ([`KUBERNETES_OBJECTS.md`](../../../docs/concepts/KUBERNETES_OBJECTS.md)) |
| `kubernetes.proxyURL` | the proxy's Service | The base URL in the Secrets and the Models |
| `kubernetes.keys.rotationInterval`, `kubernetes.keys.rotationGrace` | `720h`, `168h` | When a key is replaced, and how long the old one stays valid; restart the pods that read it into their environment within the grace |
| `serviceMonitor.enabled`, `prometheusRule.enabled` | `false`, `false` | With the Prometheus Operator: scrape aisa's metrics, and alert on them; `prometheusRule.labels` so that the Prometheus selects the rule, e.g. `release: kube-prometheus-stack` |
| `networkPolicy.extraFrom` | none | Who may reach aisa's metrics port 9090, e.g. Prometheus. Not the API: a peer here cannot send usage events or ask for decisions (#46) |
| `global.aisa.fullnameOverride` | none | The name of aisa's objects; global, so the proxy finds aisa's Service by it |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | none | e.g. `VAULT_CACERT` with a CA from a ConfigMap |
| `apisix.*` | | The proxy's values ([`proxies/apisix/chart/values.yaml`](../../../proxies/apisix/chart/values.yaml)) |

## Working on the charts

The proxy's chart is a packaged dependency: `helm dependency update` copies it into `charts/` (git-ignored), and `helm install` and `helm upgrade` read that copy, not `proxies/apisix/`. After a change to the proxy's files or chart, run `helm dependency update deploy/helm/aisa` again; otherwise the cluster keeps the old template. `helm upgrade --dependency-update` is not enough: it only fetches a dependency that is missing, and leaves an old copy in place. `charts_test.sh` and `install_test.sh` do this themselves.

## Tests

- [`../test/charts_test.sh`](../test/charts_test.sh): `helm lint`, and `kubeconform` for the usual combinations of values. Needs no cluster.
- [`../test/install_test.sh`](../test/install_test.sh): installs both charts into the current kubectl context (CI: kind on amd64 and arm64) with Vault, Consul with ACLs and two mock backends, and checks the logins, the Consul tokens from Vault, a provider key rendered from Vault, requests and usage through the proxy, the NetworkPolicy, and that `helm uninstall` removes everything.
