# Plinth API

This document describes the HTTP API exposed by `plinthd`. It is written for
someone using the platform, not for someone reading its Go implementation.

The API is also available as machine-readable OpenAPI at `/openapi.yaml` and
as an interactive Swagger UI at `/docs`. The browser test client is at
`/playground`.

## Run the API

Start the dependency-free local control plane from the repository root:

```bash
go run ./cmd/plinthd --backend=fake --addr :8080 --state /tmp/plinth-state.json
```

The base URL is `http://localhost:8080`. The Kubernetes backend uses the same
API after starting `plinthd` with `--backend=kubernetes` and a target
namespace. PostgreSQL can replace the file store with `--database-dsn` or
`PLINTH_DATABASE_DSN`.

## Request identity and tenancy

Every request may include:

| Header | Default | Meaning |
| --- | --- | --- |
| `X-Plinth-Actor` | `local` | Caller identity used by the local policy. |
| `X-Plinth-Team` | `default` | Team whose namespace and quota apply. |

`local` and `admin` are global actors. Other actors must be listed in the
team's `members` array. When a service is submitted, the server chooses its
namespace from the team policy; a client-supplied manifest namespace is not a
way to escape tenancy.

## Common response rules

Service writes return a `Service` object. With the worker enabled, a write is
normally accepted quickly with phase `pending`; the worker then reconciles it.
Poll `GET /api/v1/services/{name}` until the phase is terminal.

Possible phases are:

| Phase | Meaning |
| --- | --- |
| `pending` | A desired revision exists and is queued. |
| `reconciling` | Plinth is applying or waiting for the backend. |
| `ready` | The desired revision converged. |
| `failed` | The revision could not converge and no automatic restore completed. |
| `rolled_back` | A failed revision was restored to a known-good revision. |
| `paused` | Reconciliation is paused; current workload resources remain. |
| `destroyed` | Workload resources were removed; revision history remains. |

Errors use this shape:

```json
{"error":"human-readable explanation"}
```

Successful action responses use this shape:

```json
{"service": {"name":"tessera-gateway", "phase":"ready"}, "error":""}
```

## Manifest

The service submission body is JSON. YAML manifests used by the CLI are
converted to the same contract.

```json
{
  "name": "tessera-gateway",
  "image": "ghcr.io/nickemma/tessera:v0.4.1",
  "port": 8080,
  "replicas": 3,
  "env": {"LOG_LEVEL": "info"},
  "secrets": ["DATABASE_URL"],
  "resources": {"cpu": "500m", "memory": "512Mi"}
}
```

Required fields are `name`, `image`, `port`, `replicas`, and both resource
quantities. Names are DNS-safe; ports are 1–65535; replicas are 0–1000; and
environment and secret names must be valid, unique environment-variable
names.

An optional progressive rollout is configured as follows:

```json
"rollout": {
  "enabled": true,
  "steps": [10, 50, 100],
  "max_error_rate": 0.05
}
```

Steps must increase from 1 through 100. If omitted, enabled rollouts use
`10, 50, 100`. Kubernetes rollouts require `--prometheus-url` (and optionally
`--prometheus-query`); a missing or failing metric source fails closed.

## Health and browser surfaces

### `GET /healthz`

Returns HTTP 200 when the process is alive.

### `GET /readyz`

Returns HTTP 200 when the HTTP control-plane endpoint is ready to serve.

### `GET /docs`

Returns Swagger UI. The UI loads the live contract from `/openapi.yaml`.

### `GET /playground`

Returns a small browser client for applying manifests, inspecting status,
events, logs, drift repair, rollback, pause, resume, and destroy.

### `GET /openapi.yaml`

Returns the OpenAPI 3.0 contract used by Swagger UI.

## Teams

### `GET /api/v1/teams`

Lists the configured teams. Global actors can list all teams; team members
can list the policy visible to their team.

### `POST /api/v1/teams`

Registers or updates a team. Only `local` and `admin` may call this endpoint.
The server creates or labels the team's namespace when the Kubernetes backend
is active.

Request:

```json
{
  "name": "payments",
  "members": ["alice", "bob"],
  "namespace": "plinth-payments",
  "service_quota": 20
}
```

Team names and namespaces must be DNS-safe. A namespace can belong to only one
team. Team records are persisted by both the file and PostgreSQL stores.

## Services

### `POST /api/v1/services`

Validates and stores a new revision, then queues it for reconciliation when a
worker is running. Re-submitting an unchanged desired manifest is idempotent.

```bash
curl -X POST http://localhost:8080/api/v1/services \
  -H 'Content-Type: application/json' \
  -H 'X-Plinth-Actor: local' \
  -H 'X-Plinth-Team: default' \
  --data @manifest.json
```

Returns HTTP 200 with the current service record, or HTTP 400 for validation
errors, HTTP 403 for policy violations, and HTTP 429 when the team's quota is
exceeded.

### `GET /api/v1/services`

Lists services visible to the caller's team. Global actors see all services.

### `GET /api/v1/services/{name}`

Returns status, desired/active/known-good revisions, observed resources,
history, events, and reconciliation logs.

### `GET /api/v1/services/{name}/events`

Returns the service's level-triggered reconciliation event history.

### `GET /api/v1/services/{name}/logs`

Returns reconciliation output and error logs. This is control-plane activity,
not a replacement for application log shipping.

### `POST /api/v1/services/{name}/rollback`

Creates a new desired revision from a historical manifest and reconciles it.
Send an empty body to restore the last known-good revision, or specify any
historical revision:

```json
{"revision": 1}
```

Rollback is revision selection, not an imperative special case; the normal
reconciler applies the selected manifest.

### `POST /api/v1/services/{name}/pause`

Stops reconciliation for the service while leaving its current Kubernetes
resources running.

### `POST /api/v1/services/{name}/resume`

Re-enables reconciliation and immediately attempts to converge the service.

### `POST /api/v1/services/{name}/destroy`

Deletes Plinth-managed backend resources and retains the service's revision
history.

### `POST /api/v1/services/{name}/test/drift`

Test-only helper. Deletes one managed fake or Kubernetes resource and invokes
reconciliation so drift repair can be demonstrated. The default resource kind
is `Deployment`; it can be selected explicitly:

```json
{"kind":"Deployment"}
```

## Audit

### `GET /api/v1/audit`

Returns audit records for accepted, denied, failed, rollback, lifecycle, and
team actions. Optional query parameters are `from` and `to` (RFC3339 times),
and `format=json` or `format=csv`.

```bash
curl 'http://localhost:8080/api/v1/audit?format=csv'
```

## CLI mapping

The CLI uses the API and accepts `PLINTH_API`, `PLINTH_ACTOR`, and
`PLINTH_TEAM` environment variables:

| CLI command | API operation |
| --- | --- |
| `plinth up -f manifest.yaml` | `POST /api/v1/services` |
| `plinth status [name] [--watch]` | `GET /api/v1/services` or service status |
| `plinth logs name [--follow]` | `GET .../logs` |
| `plinth rollback name [revision]` | `POST .../rollback` |
| `plinth pause name` | `POST .../pause` |
| `plinth resume name` | `POST .../resume` |
| `plinth destroy name` | `POST .../destroy` |

For a complete test sequence, continue with
[`docs/walkthrough.md`](docs/walkthrough.md).
