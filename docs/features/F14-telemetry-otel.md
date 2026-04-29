# F14 — OTEL Telemetry Pipeline

**Phase:** 2 | **Wave:** 2.A | **Dependencies:** none (cross-cutting library)

## Goal

A shared `packages/telemetry` library that other services adopt voluntarily, plus a docker-compose LGTM stack for local observability.

## Scope (in)

- `platform_telemetry` library with:
  - `setup.init(service_name)` — first-call-wins global TracerProvider/MeterProvider config.
  - W3C trace context propagation helpers (`inject_traceparent`, `extract_traceparent`).
  - `traced_span(name, **attrs)` async context manager + `@traced` decorators.
  - Canonical span tree constants (`SPAN_GATEWAY_HANDLE_WEBHOOK`, …).
  - Canonical attribute constants (`PLATFORM_USER_ID`, `PLATFORM_PROVIDER`, …).
  - 5 metric helpers: `agent_runs_total`, `agent_run_latency_seconds`, `cli_turn_latency_seconds`, `sandbox_cold_start_seconds`, `mcp_tool_call_total`.
  - `HealthProbe` / `HealthRegistry` / `HealthSnapshot` / `ProviderHealthTracker` (used by F16).
  - Structured logging with `trace_id` / `span_id` correlation.
  - Sampling: configurable ratio + always-sample on `error_class != null`.
- LGTM stack at `infra/observability/`:
  - `docker-compose.observability.yml` (otel-collector + Tempo + Loki + Prometheus + Grafana).
  - 3 Grafana dashboards: `agent-runs`, `sandbox-pool`, `provider-health`.

## Canonical span tree

```
gateway.handle_webhook
└── gateway.enqueue
    └── orchestrator.run
        ├── orchestrator.resume_sandbox
        └── orchestrator.exec_runtime
            └── runtime.run_loop
                ├── runtime.skill_resolve
                ├── runtime.mcp_bridge_start
                ├── runtime.cli_turn
                │   ├── runtime.cli_spawn
                │   └── runtime.cli_parse
                ├── runtime.tool_call
                └── runtime.respond
```

## Scope (out)

- Modifying F01/F05/F06 to emit spans — that is per-service work the brief lists as "small surgical PRs". F14 ships the helpers + tests proving they work end-to-end across propagation hops.
- Custom Prometheus / Loki / Tempo backends — vanilla configs only.

## Deliverables

```
packages/telemetry/
├── pyproject.toml
├── README.md
└── src/platform_telemetry/
    ├── __init__.py
    ├── attrs.py                # canonical span/attr/metric names
    ├── setup.py
    ├── propagation.py
    ├── decorators.py
    ├── metrics.py
    ├── health.py               # HealthProbe + ProviderHealthTracker (F16 uses)
    └── logging.py

packages/telemetry/tests/  (~9 test modules)

infra/observability/
├── docker-compose.observability.yml
├── otel-collector-config.yaml
├── tempo-config.yaml
├── loki-config.yaml
├── prometheus.yml
└── grafana/
    ├── dashboards/{agent-runs,sandbox-pool,provider-health}.json
    └── provisioning/{datasources,dashboards}.yaml
```

## Acceptance criteria

1. `pytest packages/telemetry/tests/` passes (in-memory exporter).
2. Canonical span tree replay test asserts 13 spans + correct parent/child + cross-process propagation.
3. 5 metric helpers create instruments with the documented names + labels.
4. W3C propagation: inject + extract round-trips across both gateway→orchestrator and orchestrator→runtime hops.
5. `HealthSnapshot` has the 3 outcomes (healthy/degraded/unhealthy) computed from a rolling-window failure rate.
6. Sampler: 0% → 0 traces, 100% → all traces, X% → ~X% but `error_class` always sampled.
7. Structured-log records include `trace_id` + `span_id` when emitted inside an active span.
