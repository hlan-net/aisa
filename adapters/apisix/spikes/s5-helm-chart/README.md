# Spike S5: APISIX in standalone mode on Kubernetes

The question and outcome are recorded in [`docs/process/SPIKES.md`](../../../../docs/process/SPIKES.md#s5-apisix-in-standalone-mode-on-kubernetes). This directory holds what they are based on, so they can be re-run on another cluster or against a newer chart.

```bash
OLLAMA_ADDR=gpu-box.internal:11434 ./adapters/apisix/spikes/s5-helm-chart/run.sh             # both parts
OLLAMA_ADDR=gpu-box.internal:11434 ./adapters/apisix/spikes/s5-helm-chart/run.sh manifests   # one part
kubectl delete namespace aisa-spike
```

It uses the current `kubectl` context. The backend is a real OpenAI-compatible server that the cluster can reach; everything else comes from public images, so the cluster needs no registry of its own.

| File | Contents |
|---|---|
| [`values-standalone.yaml`](values-standalone.yaml) | Values for the official chart `apisix/apisix`: standalone mode, no etcd, one worker, the internal listener, and the routes as a static ConfigMap |
| [`adapter.yaml`](adapter.yaml) | The adapter as plain manifests: APISIX and consul-template in one pod, sharing a directory in memory; an init container renders once before APISIX starts |
| [`config.yaml`](config.yaml) | APISIX's config for it: the internal listener and one worker |
| [`consul-template.hcl`](consul-template.hcl) | consul-template's configuration. The template is the one of [spike S9](../s9-config-rendering/apisix.yaml.ctmpl) |
| [`support.yaml`](support.yaml) | What the adapter talks to in the spike: a dev-mode Consul and an nginx that answers `/v1/decide` and `/v1/usage` in place of aisa |
| [`run.sh`](run.sh) | Installs each variant into the namespace `aisa-spike`, sends requests through it, changes its routes and measures how long the change takes. Exits non-zero on any unexpected result |

## The two variants

| | Official chart | Plain manifests |
|---|---|---|
| Standalone mode without etcd | yes; a Secret and an environment variable for etcd are left over | yes |
| One nginx worker, the internal listener | yes, as values | yes, in `config.yaml` |
| Where `apisix.yaml` comes from | a ConfigMap, mounted at a fixed path | a file that consul-template renders, in a directory in memory |
| A sidecar that renders the file | not possible: the path is taken by the ConfigMap | yes |
| Provider keys | in the ConfigMap, which the cluster stores | in memory only |
| A change reaches APISIX after | 17 to 61 s, measured through requests | 2 to 3 s to the rendered file, measured through the Kubernetes API; APISIX reads it once a second (S9) |
| Requests that failed during a change | none | none, for a route that stays in place (routes that change: S9) |

## Output of the recorded run (2026-09-29, APISIX 3.18.0, chart 2.17.0, consul-template 0.43.0)

k3s 1.36 on Raspberry Pi 4 Model B nodes (arm64, 4 cores, 8 GB), Ollama 0.31.1 outside the cluster with `llama3.2:3b`. CPU and memory are what the kubelet reports (`kubectl top`), a moment after a few requests, not under load.

```
  ok    the backend answers from the cluster                           200

== The official chart apisix/apisix 2.17.0 in standalone mode
  ok    kinds that the chart renders                                   ConfigMap ConfigMap Deployment Secret Service
  ok      ... left of etcd: a Secret and an environment variable       1 1
        installed and ready after                                      41 s
        node                                                           Raspberry Pi 4 Model B Rev 1.4 4 cores
  ok    pods of etcd                                                   0
  ok    nginx workers                                                  1
  ok    the internal listener of the two-hop route                     1
  ok    a request, not streamed                                        200
  ok    a request, streamed                                            200
  ok      ... to its end, with usage                                   1 1
        APISIX, cores / memory                                         10m / 46Mi

== A changed route, through helm upgrade
  ok    the new route before the change                                404
        helm upgrade returned after                                    3 s
        the change reached APISIX after                                17 s
  ok    the pod was replaced                                           no

== Can the chart take a file that a sidecar renders?
  ok    a shared directory at the path of apisix.yaml                  mountPath: Invalid value: "/apisix-config": must be unique

== Plain manifests: APISIX and consul-template in one pod
        applied and ready after                                        5 s
        node                                                           Raspberry Pi 4 Model B Rev 1.5 4 cores
  ok    containers ready                                               true true
  ok    the rendered file is in memory                                 tmpfs
  ok    nginx workers                                                  1
  ok    routes                                                         client model-llama3.2-3b-925b2caf model-no-colon-model no-backend
  ok    routes that APISIX rejected                                    0

== Traffic: client, decision, backend, usage event
  ok    a request, not streamed                                        200
  ok      ... with usage in the answer                                 true
  ok    a request, streamed                                            200
  ok      ... to its end, with usage                                   1 1
  ok    a model that no backend serves                                 503
  ok    decisions that the stub answered                               3
  ok    usage events that the stub received                            3
        APISIX, cores / memory (requests 100m / 96Mi, limit 192Mi)     16m / 48Mi
        consul-template, cores / memory (requests 10m / 32Mi, limit 64Mi) 0m / 4Mi

== A backend leaves and returns 5 times, under a request loop
  ok    requests answered otherwise than expected during 10 reloads    0
        requests sent meanwhile                                        191
        from the change in Consul to the new file, leaving             2.3 s 1.9 s 2.0 s 3.0 s 2.0 s
        from the change in Consul to the new file, returning           2.4 s 2.0 s 1.8 s 3.0 s 3.0 s
  ok    a request after the reloads                                    200

== The pod is replaced
        a new pod is ready after                                       5 s
  ok    a request to the new pod                                       200

all checks as expected
```

In four tries, a change of the chart's ConfigMap reached APISIX after 17, 30, 56 and 61 s.
