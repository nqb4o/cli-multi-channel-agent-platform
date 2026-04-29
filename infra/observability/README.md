# LGTM observability stack

Local Grafana / Loki / Tempo / Mimir-style (Prometheus) stack for the
CLI-First Multi-Provider Agent Platform (F14).

## What runs

| Service | Image | Port | Purpose |
|---|---|---|---|
| `otel-collector` | otel/opentelemetry-collector-contrib | 4317 / 4318 | OTLP receiver, fan-out to backends |
| `tempo` | grafana/tempo | 3200 | Trace store |
| `loki` | grafana/loki | 3100 | Log store |
| `prometheus` | prom/prometheus | 9090 | Metric store (remote-write target) |
| `grafana` | grafana/grafana | 3000 | UI + provisioned datasources + dashboards |

## Boot

```bash
docker compose -f infra/observability/docker-compose.observability.yml up -d
docker compose -f infra/observability/docker-compose.observability.yml ps
```

Open http://localhost:3000 (admin / admin or anonymous viewer).

## Wire a service

Set the OTLP endpoint and `init` will fan out traces / metrics there:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_TRACES_SAMPLER_ARG=1.0
.venv/bin/python -m gateway   # or any service that calls platform_telemetry.init
```

## Dashboards (provisioned)

Folder: **Platform**.

* **Agent Runs** — runs/min, latency p50/p95/p99, error rate, CLI turn p95.
* **Sandbox Pool** — cold-start latency, cold vs warm starts, MCP tool call rate.
* **Provider Health** — per-provider failure rate, p99 latency, success/error
  counts, fallback activations.

## Tear down

```bash
docker compose -f infra/observability/docker-compose.observability.yml down
# add -v to also drop the named volumes (tempo / loki / prometheus / grafana)
```

## Validate config

```bash
docker compose -f infra/observability/docker-compose.observability.yml config >/dev/null
```
