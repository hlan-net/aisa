# Roadmap

## Phases

1. **Spikes**: verify the open questions in [spikes.md](spikes.md).
2. **First version**:
   - aisa core: decision API (consumer keys from Vault, token quotas), usage ingestion, `aisa_*` metrics
   - APISIX adapter: forward-auth, http-logger, rendered `apisix.yaml`
   - Terraform module, Helm chart, Grafana dashboard
3. **Money budgets**: prices in Consul KV, monthly budgets, downgrade to a local model.
4. **Second adapter**: prove the contract with a second gateway. **LiteLLM** is the most likely: its open source proxy supports custom auth and callbacks, which map to the decision API and usage events. Candidates after that: Envoy AI Gateway / Agent Router and API7 AISIX.
5. **Identity**: Vault-issued JWTs instead of static consumer keys ([vault.md](vault.md)).

## Upstream contributions

Improvements that belong in the gateways rather than in aisa:
1. APISIX Vault secret manager: KV v2 support
2. APISIX Vault secret manager: Kubernetes auth
3. Anything the spikes uncover, e.g. token usage in log variables for streaming responses

## Principles

- aisa has no knowledge of any specific gateway. Adapter code lives only under `adapters/<name>/`.
- aisa never proxies model traffic itself. That is the gateway's job.
- The Terraform module takes Vault and Consul addresses and mount names as variables and assumes nothing about a specific environment.
