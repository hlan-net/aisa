# aisa on a local cluster, with the machine's Ollama

The whole path on one laptop: an application's Secret, aisa's decision, the proxy, and a model that the host's Ollama runs. For trying aisa out and for working on it; not for a real cluster.

```bash
./deploy/helm/local/up.sh     # into the current kubectl context
./deploy/helm/local/down.sh   # and out again
```

## What you need

- A local Kubernetes: Docker Desktop's (Kubernetes 1.29 or newer) or [kind](https://kind.sigs.k8s.io/). The proxy is published as a LoadBalancer, which both put on `localhost`.
- [Ollama](https://ollama.com/) on the machine with at least one model pulled. The cluster reaches it as `host.docker.internal:11434` (`OLLAMA_ADDR` to change it). On Linux, Ollama must listen on all interfaces (`OLLAMA_HOST=0.0.0.0`), not only on `127.0.0.1`.
- `docker`, `kubectl`, `helm`, `curl`, `jq`, `openssl`.

## What `up.sh` does

1. Builds `aisa:ci` and `aisa-dev-tools:ci` and loads them into the cluster's own image store (Docker Desktop's Kubernetes and kind do not see the Docker engine's images).
2. Applies the fixtures of the install test ([`../test/fixtures.yaml`](../test/fixtures.yaml)): a dev-mode Vault with Kubernetes auth, a dev-mode Consul with ACLs, and the mock backends, which stay unused.
3. Registers the host's Ollama in Consul as an `aisa-backend` with every model it serves, `fail_policy = "open"` and a 600 s timeout; creates the Consul policies, a quota profile `interactive`, the Vault policies and roles, and Vault's Consul secrets engine ([`VAULT.md`](../../../docs/concepts/VAULT.md), [`CONSUL.md`](../../../docs/concepts/CONSUL.md)).
4. Creates one consumer (`CONSUMER`, by default `opencode`): a random key in `.local/<consumer>.key` (git-ignored, mode 600) and its SHA-256 in Vault.
5. Installs aisa's chart with the proxy as a LoadBalancer.
6. Writes the consumer's Secret `aisa` into the namespace `APP_NAMESPACE` (by default the consumer's name), with `OPENAI_BASE_URL` and `OPENAI_API_KEY`, in the shape of [`KUBERNETES_OBJECTS.md`](../../../docs/concepts/KUBERNETES_OBJECTS.md#the-consumers-secret). A pod in that namespace takes it with `envFrom` and calls the proxy like a provider.
7. Checks `GET /v1/models` and one chat completion on `http://localhost`, and prints what to set for an application on the machine and for OpenCode.

Steps 4 and 6 are a stand-in: ROADMAP v0.2.0 PR 12 has aisa generate the key and write the Secret itself. Run `up.sh` again after a change: it rebuilds the images, re-packages the proxy's chart (`helm dependency update`) and upgrades the release; the key is kept.

## Using it

From the machine, any OpenAI SDK:

```bash
export OPENAI_BASE_URL=http://localhost/v1
export OPENAI_API_KEY=$(cat .local/opencode.key)
curl -s $OPENAI_BASE_URL/models | jq -r '.data[].id'
```

[OpenCode](https://opencode.ai/) takes a custom provider in `opencode.json`; `up.sh` prints the block. Open WebUI takes the base URL and the key as an OpenAI connection and lists the models from `/v1/models`.

What aisa saw: `kubectl -n aisa port-forward svc/aisa 8080:8080`, then `aisa_requests_total` and `aisa_tokens_total` at `http://127.0.0.1:8080/metrics`, per consumer and model. The usage of a request answered while aisa is down (the backend fails open) is accounted under `unknown`.

## Variables

| Variable | Default | |
|---|---|---|
| `CONTEXT` | the current context | `docker-desktop` or `kind-<name>` load the images themselves |
| `NAMESPACE`, `RELEASE` | `aisa`, `aisa` | |
| `OLLAMA_ADDR` | `host.docker.internal:11434` | As the cluster reaches it |
| `CONSUMER`, `APP_NAMESPACE` | `opencode`, the consumer's name | The consumer and where its Secret goes |
| `TOKENS_PER_HOUR` | `2000000` | The consumer's quota |
