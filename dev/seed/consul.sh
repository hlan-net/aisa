#!/bin/sh
# Seeds dev-mode Consul with the example prices, budgets, quota profiles and backend
# registrations from docs/concepts/CONSUL.md.
set -eu

export CONSUL_HTTP_ADDR="${CONSUL_HTTP_ADDR:-http://consul:8500}"

until consul members >/dev/null 2>&1; do sleep 1; done

consul kv put aisa/pricing/qwen3       '{"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": "EUR"}'
consul kv put aisa/pricing/llama3.2    '{"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": "EUR"}'
consul kv put aisa/pricing/cloud-large '{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "EUR"}'

consul kv put aisa/budgets/chat-ui    '{"monthly": 25.00, "soft_ratio": 0.8, "on_exhausted": "reject"}'
consul kv put aisa/budgets/batch-jobs '{"monthly": 10.00, "soft_ratio": 0.8, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}'

consul kv put aisa/quotas/interactive '{"tokens_per_hour": 1000000}'
consul kv put aisa/quotas/batch       '{"tokens_per_hour": 200000}'

consul services register /seed/backends.hcl

# Wait for the first health check runs, so dependants see both backends as passing.
for _ in $(seq 1 60); do
    passing=$(curl -fsS "$CONSUL_HTTP_ADDR/v1/health/service/aisa-backend?passing" | grep -Eo '"CheckID": *"service:' | wc -l)
    [ "$passing" -ge 2 ] && break
    sleep 1
done
[ "$passing" -ge 2 ] || { echo "backends did not become healthy" >&2; exit 1; }

echo "consul seeded"

# Stay up and report healthy, so `docker compose up --wait` also waits for seeding.
touch /tmp/seeded
trap 'exit 0' TERM INT
while :; do sleep 3600 & wait $!; done
