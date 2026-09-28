# Dev registrations of the mock backends in the Consul catalog, in the shape described in
# docs/concepts/CONSUL.md. Registered on the dev agent so the health checks run.
services {
  id   = "mock-local"
  name = "aisa-backend"
  address = "mock-local"
  port = 8080
  tags = ["mock", "local"]
  meta = {
    provider = "openai-compatible"
    models   = "qwen3,llama3.2"
    priority = "1"
  }
  check {
    id       = "service:mock-local"
    name     = "mock-local"
    http     = "http://mock-local:8080/healthz"
    interval = "10s"
    timeout  = "2s"
  }
}

services {
  id   = "mock-cloud"
  name = "aisa-backend"
  address = "mock-cloud"
  port = 8080
  tags = ["mock", "cloud"]
  meta = {
    provider = "openai-compatible"
    models   = "cloud-large"
    priority = "2"
  }
  check {
    id       = "service:mock-cloud"
    name     = "mock-cloud"
    http     = "http://mock-cloud:8080/healthz"
    interval = "10s"
    timeout  = "2s"
  }
}
