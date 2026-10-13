# aisa on a local cluster, with the machine's Ollama

The whole path on one laptop: an application's Secret, aisa's decision, the proxy, and a model that the host's Ollama runs. For trying aisa out and for working on it; not for a real cluster.

```bash
./deploy/helm/local/up.sh     # into the current kubectl context
./deploy/helm/local/down.sh   # and out again
```

## What you need

- A local Kubernetes: Docker Desktop's (Kubernetes 1.29 or newer) or [kind](https://kind.sigs.k8s.io/). On Docker Desktop the proxy is published as a LoadBalancer on `localhost:80`; a plain kind cluster has no load balancer, so there the proxy stays a ClusterIP and the script uses `kubectl port-forward` (`PUBLISH=lb|forward` overrides the choice).
- [Ollama](https://ollama.com/) on the machine with at least one model pulled. The script asks for it as `host.docker.internal:11434` (`OLLAMA_ADDR` to change it) and registers in Consul the host's address on Docker's network, which pods reach on Docker Desktop and on kind alike. On Linux, Ollama must listen on all interfaces (`OLLAMA_HOST=0.0.0.0`), not only on `127.0.0.1`.
- `docker`, `kubectl`, `helm`, `curl`, `jq` (Linux, macOS or WSL).

## What `up.sh` does

1. Builds `aisa:ci` and `aisa-dev-tools:ci` and loads them into the cluster's own image store (Docker Desktop's Kubernetes and kind do not see the Docker engine's images).
2. Applies the fixtures of the install test ([`../test/fixtures.yaml`](../test/fixtures.yaml)): a dev-mode Vault with Kubernetes auth, a dev-mode Consul with ACLs, and the mock backends, which stay unused.
3. Registers the host's Ollama in Consul as an `aisa-backend` with every model it serves, `fail_policy = "open"` and a 600 s timeout; creates the Consul policies, a quota profile `interactive`, the Vault policies and roles, and Vault's Consul secrets engine ([`VAULT.md`](../../../docs/concepts/VAULT.md), [`CONSUL.md`](../../../docs/concepts/CONSUL.md)).
4. Declares one consumer (`CONSUMER`, by default `opencode`) in Consul, with the namespace `APP_NAMESPACE` (by default the consumer's name) and the Secret `aisa`.
5. Installs aisa's chart with the proxy published by `PUBLISH`: a LoadBalancer on `localhost:80` (Docker Desktop), or a ClusterIP that the script reaches with `kubectl port-forward` on `localhost:18080` (kind).
6. Waits for aisa to issue the consumer's key and write the Secret `aisa` into that namespace, with `OPENAI_BASE_URL` and `OPENAI_API_KEY` ([`KUBERNETES_OBJECTS.md`](../../../docs/concepts/KUBERNETES_OBJECTS.md#the-consumers-secret)), and a `Model` object per model of the host's Ollama. A pod in that namespace takes the Secret with `envFrom` and calls the proxy like a provider.
7. Checks `GET /v1/models` and one chat completion with the smallest model, and prints what to set for an application on the machine and for OpenCode.

Namespaces that `up.sh` creates and the fixtures get the label `aisa.hlan.net/local-setup=true`, and `down.sh` deletes only what carries it; a namespace that existed before keeps everything but this setup's objects. In a namespace that existed before, `up.sh` stops instead of taking over an object of the same name that is not this setup's (a `vault` Deployment, a Secret `aisa` that is not aisa's), and it stops if the fixtures' ClusterRoleBinding binds the Vault of another namespace. The Secret is aisa's own: aisa writes and deletes it, and never takes over one it did not write.

Run `up.sh` again after a change: it rebuilds the images, re-packages the proxy's chart (`helm dependency update`) and upgrades the release; the key is kept. aisa replaces the key every 30 days (`kubernetes.keys` in the chart's values); the old one stays valid for a week.

## Using it

From the machine, any OpenAI SDK. With `PUBLISH=lb` (Docker Desktop) the proxy is on `http://localhost`; with `PUBLISH=forward` (kind) keep a port-forward running and use `http://localhost:18080`. `up.sh` prints the commands for the one it used:

```bash
kubectl -n aisa port-forward svc/aisa-apisix 18080:80 &   # PUBLISH=forward only
export OPENAI_BASE_URL=http://localhost:18080/v1          # http://localhost/v1 with PUBLISH=lb
export OPENAI_API_KEY=$(kubectl -n opencode get secret aisa -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)
curl -s $OPENAI_BASE_URL/models | jq -r '.data[].id'
```

[OpenCode](https://opencode.ai/) takes a custom provider in `opencode.json`; `up.sh` prints the block. Open WebUI takes the base URL and the key as an OpenAI connection and lists the models from `/v1/models`.

What aisa saw: `kubectl -n aisa port-forward svc/aisa-metrics 9090:9090`, then `aisa_requests_total` and `aisa_tokens_total` at `http://127.0.0.1:9090/metrics`, per consumer and model. The usage of a request answered while aisa is down (the backend fails open) is accounted under `unknown`.

## Cleaning up

`down.sh` removes the consumer from Consul while aisa still runs, so aisa deletes its Secret and the key's hashes, then uninstalls the release, deletes this install's `Model` objects, the fixtures and their ClusterRoleBinding, and the namespaces `up.sh` created, each only if it is this setup's. The `Model` CRD stays: the chart keeps it when it is uninstalled, and other installs may use it. The next `up.sh` issues a new key.

## Variables

| Variable | Default | |
|---|---|---|
| `CONTEXT` | the current context | `docker-desktop` or `kind-<name>` load the images themselves |
| `NAMESPACE`, `RELEASE` | `aisa`, `aisa` | aisa's Service and ServiceAccount are the release's name if it contains `aisa`, else `<release>-aisa`, as in the chart |
| `OLLAMA_ADDR` | `host.docker.internal:11434` | As the cluster reaches it |
| `CONSUMER`, `APP_NAMESPACE` | `opencode`, the consumer's name | The consumer and where its Secret goes |
| `TOKENS_PER_HOUR` | `2000000` | The consumer's quota |
| `PUBLISH` | `lb` on Docker Desktop, else `forward` | A LoadBalancer on `localhost:80`, or a ClusterIP with `kubectl port-forward` on `localhost:18080` |
