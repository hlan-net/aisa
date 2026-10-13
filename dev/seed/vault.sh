#!/bin/sh
# Seeds dev-mode Vault with the key hashes of the example consumers and the provider key from
# docs/concepts/VAULT.md; the consumers themselves are declared in Consul (seed/consul.sh). The
# keys are dev-only placeholders; the plaintext consumer keys match STUB_KEYS in compose.yaml.
set -eu

export VAULT_ADDR="${VAULT_ADDR:-http://vault:8200}"
export VAULT_TOKEN="${VAULT_TOKEN:-dev-root}"

until vault status >/dev/null 2>&1; do sleep 1; done

sha256() { printf %s "$1" | sha256sum | cut -d' ' -f1; }

vault kv put secret/aisa/consumers/chat-ui key_sha256="$(sha256 dev-key-chat-ui)"
vault kv put secret/aisa/consumers/batch-jobs key_sha256="$(sha256 dev-key-batch-jobs)"
vault kv put secret/aisa/providers/cloud api_key=dev-provider-key

echo "vault seeded"

# Stay up and report healthy, so `docker compose up --wait` also waits for seeding.
touch /tmp/seeded
trap 'exit 0' TERM INT
while :; do sleep 3600 & wait $!; done
