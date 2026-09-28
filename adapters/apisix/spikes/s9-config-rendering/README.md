# Spike S9: config rendering with consul-template

The question and outcome are recorded in [`docs/process/SPIKES.md`](../../../../docs/process/SPIKES.md#s9-config-rendering-with-consul-template). This directory holds what they are based on, so they can be re-run against a newer APISIX or consul-template.

```bash
./adapters/apisix/spikes/s9-config-rendering/run.sh
docker compose -f dev/compose.yaml -f adapters/apisix/spikes/s9-config-rendering/compose.override.yaml down -v
```

| File | Contents |
|---|---|
| [`apisix.yaml.ctmpl`](apisix.yaml.ctmpl) | The template: the client-facing route, one internal route per model with the healthy backends that serve it, and a route that answers 503 for every other model |
| [`consul-template.hcl`](consul-template.hcl) | consul-template's configuration: Consul, Vault, the template, its quiet period |
| [`config.yaml`](config.yaml) | The APISIX config of S8, with the internal listener |
| [`compose.override.yaml`](compose.override.yaml) | Adds consul-template and a second backend for `qwen3`, and lets APISIX read the rendered file from a shared volume |
| [`run.sh`](run.sh) | Registers the backends, then checks rendering, routing, reloads under load, streams in flight, a backend that dies, key rotation, unreachable sources and broken files. Exits non-zero on any unexpected result |

## What the template reads

| Source | Used for |
|---|---|
| Consul service `aisa-backend`, healthy instances only | One `ai-proxy-multi` instance per backend and model |
| Service meta `models` | The models of a backend, comma-separated; spaces around the names are ignored |
| Service meta `provider` | The instance's `provider` |
| Service meta `key`, optional | The name of the Vault secret `secret/aisa/providers/<key>`, whose `api_key` becomes the instance's `Authorization` header |

Every value from Consul or Vault is written with `toJSON`, so a name with a quote or a colon in it cannot change the structure of the file.

## How the file reaches APISIX

consul-template writes a new file and renames it over the old one. A bind mount of the file itself keeps showing the old one, so consul-template and APISIX share a directory, and `conf/apisix.yaml` is a link into it. APISIX looks at the file once a second and loads it when it has changed and ends with `#END`.

## Output of the recorded run (2026-09-28, APISIX 3.18.0, consul-template 0.43.0, arm64)

```
== Rendering: backends from the Consul catalog, the provider key from Vault
  ok    routes (client, one per model, no-backend)                         client model-cloud-large model-llama3.2 model-qwen3 no-backend
  ok    backends of qwen3                                                  mock-local mock-local-2
  ok    the key of mock-cloud comes from Vault                             1
  ok    backends without a key get none                                    0
  ok    the file ends with #END                                            #END

== Routing through the rendered config
  ok    a model that no backend serves                                     503
  ok      ... with an error a client can read                              no_backend
  ok    who served what                                                    cloud-large:mock-cloud llama3.2:mock-local qwen3:mock-local qwen3:mock-local-2

== Reload under load: a backend leaves and returns 10 times (Consul maintenance mode)
  ok    failed requests during 20 reloads                                  0
        requests sent meanwhile                                            3792
        reloads APISIX logged (4 workers and the master log each)          100
        from the change in Consul to the new file, leaving                 1.1 s 1.1 s 1.9 s 1.5 s 1.2 s 1.4 s 1.4 s 1.4 s 1.4 s 1.5 s
        from the change in Consul to the new file, returning               1.4 s 1.4 s 1.1 s 1.4 s 1.0 s 1.1 s 1.1 s 1.5 s 1.1 s 1.5 s

== After a backend has left, nothing is sent to it
  ok    backends that served 10 requests for qwen3                         mock-local

== Streams in flight when their backend leaves
        the backend left the config, 1 s into streams of 8 s, after        1.3 s
  ok    streams that ran to their end                                      4
  ok      ... with 400 tokens in the usage event                           4
  ok      ... of which on the backend that left, at least one              true

== A backend that dies (container stopped), under load
        from the stop to the new file                                      3.2 s
        requests sent / failed                                             155 / 18
        statuses of the failed requests                                    000 x 6  500 x 11  incomplete x 1  
        the last request that failed began, after the stop                 2.1 s
        the failed requests took, shortest / longest                       0.0 s / 60.0 s
  ok    afterwards: 10 requests for qwen3, failed                          0
        back in the config after its start                                 1.3 s

== A rotated provider key
        in the rendered file after (default_lease_duration is 10 s)        9.1 s
  ok    the old key is gone from the file                                  0
  ok    requests for cloud-large still work                                200

== Consul or Vault unreachable: the last config stays
  ok    consul paused for 15 s: the rendered file is unchanged             true
  ok      ... and requests work                                            200
  ok      ... and consul-template is still running                         running
  ok    vault paused for 15 s: the rendered file is unchanged              true
  ok      ... and requests work                                            200
  ok      ... and consul-template is still running                         running

== A broken file: what APISIX does with it
  ok    invalid YAML: request for qwen3                                    200
  ok    no #END at the end (a file cut short): request for qwen3           200
  ok    no routes at all (APISIX keeps the ones it has): request for qwen3 200
  ok    one route invalid (the client route): request for qwen3            404
  ok    the good file again: request for qwen3                             200

all checks as expected
```
