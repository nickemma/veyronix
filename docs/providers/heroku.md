# Provider — Heroku

| | |
|---|---|
| **Registry name** | `heroku` |
| **Status** | Planned — V1 |
| **Target type** | Heroku applications |
| **API** | Heroku Platform API v3 |

---

## Capabilities

```go
sdk.Capabilities{
    Rollback:        true,   // native release rollback
    LogStreaming:    true,   // log session API
    ZeroDowntime:    true,   // preboot, where enabled
    BlueGreen:       false,  // pipeline promotion is close but not the same primitive
    Canary:          false,
    HealthCheck:     true,
    EnvironmentVars: true,   // config vars
    Cancel:          true,   // build cancellation
}
```

Heroku has the closest thing to a native release model of any V1 provider — immutable, numbered releases with a first-class rollback — which makes it the provider where Veyronix does the least translation work.

---

## Configuration

```yaml
provider: heroku
config:
  app_name: my-production-api      # required
  deploy_method: source            # source | container
  process_types: [web, worker]     # which dynos to verify after release
  run_release_phase: true          # honour the Procfile release phase
  health_path: /healthz
credentials:
  api_key: <secret-ref>            # required
```

---

## Minimum credential scope

Heroku's permission model is per-application through team collaborator roles, which is better than Netlify's account-wide tokens.

Recommended setup:

1. A dedicated machine user, added as a **collaborator** on only the applications Veyronix manages.
2. `deploy` and `view` permissions. **Not** `manage` — Veyronix does not need to add collaborators or change billing.
3. One machine user per environment class where the separation matters, so a staging credential cannot touch production.

Heroku's `deploy` permission does include the ability to change config vars, which Veyronix needs, and cannot be separated from deployment rights. Noted as a known coarseness rather than hidden.

---

## Operation mapping

| Interface method | Heroku API |
|---|---|
| `Validate` | `GET /apps/{app}` and `GET /apps/{app}/collaborators` |
| `Deploy` (source) | `POST /apps/{app}/builds` with a source blob URL, poll build status |
| `Deploy` (container) | `PATCH /apps/{app}/formation` with the new image ID |
| `Rollback` | `POST /apps/{app}/releases` with `{ "release": "<previous-id>" }` |
| `Status` | `GET /apps/{app}/releases/{id}` and `GET /apps/{app}/dynos` |
| `Logs` | `POST /apps/{app}/log-sessions`, stream the returned URL |
| `HealthCheck` | HTTP `GET` against the app URL + `health_path` |

### Idempotency

Heroku creates a new release for every build, so idempotency must be enforced before submission: the provider tags builds with the idempotency key in the source version field and checks recent releases for the key first. A matching key returns the existing release.

### Release phase

If the Procfile defines a release phase, its failure fails the release and Heroku does not promote it. The provider must surface release-phase output in the deploy logs — a failed migration in the release phase is the single most common Heroku deploy failure, and hiding its output makes it unnecessarily hard to diagnose.

---

## Failure modes

| Failure | Signal | Behaviour |
|---|---|---|
| Invalid API key | 401 | Fail pre-flight, before any mutation |
| App not found or no access | 403/404 | `error_class = target_not_found` |
| Build fails | Build status `failed` | **User error** — excluded from the deployment-success SLI |
| Release phase fails | Release status `failed` | Heroku does not promote; previous release still serving |
| Dyno crash loop after release | Health check fails | Auto-rollback to previous release |
| Rate limited | 429 | Backoff; Heroku's limit is per account and shared across all apps, so a busy account can throttle unrelated deployments |
| Platform incident | 503 | Circuit breaker opens; jobs requeue |

---

## Manual intervention

```bash
heroku releases --app "$APP"
heroku releases:info v123 --app "$APP"
heroku rollback v122 --app "$APP"
heroku ps --app "$APP"
heroku logs --tail --app "$APP"
```

Record the action in the incident timeline.
