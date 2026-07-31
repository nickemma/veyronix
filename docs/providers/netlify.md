# Provider — Netlify

| | |
|---|---|
| **Registry name** | `netlify` |
| **Status** | Planned — V1 |
| **Target type** | Static sites and edge functions |
| **API** | Netlify REST API v1 |

---

## Capabilities

```go
sdk.Capabilities{
    Rollback:        true,   // instant, by deploy ID
    LogStreaming:    true,
    ZeroDowntime:    true,   // atomic swap of the published deploy
    BlueGreen:       false,
    Canary:          false,  // no traffic-splitting primitive
    HealthCheck:     true,   // HTTP probe against the site URL
    EnvironmentVars: true,
    Cancel:          true,
}
```

Netlify is the easiest provider to make correct and the best illustration of why `Capabilities()` exists: it does rollback superbly and traffic splitting not at all. Declaring `Canary: false` is the honest answer, and the platform hides the control rather than faking it.

---

## Configuration

```yaml
provider: netlify
config:
  site_id: 1a2b3c4d-5e6f-7890-abcd-ef1234567890   # required
  build_command: pnpm build                        # optional, if Veyronix builds
  publish_dir: dist                                # required when building
  functions_dir: netlify/functions                 # optional
  branch_deploy: false                             # deploy to the production alias
credentials:
  personal_access_token: <secret-ref>              # required
```

`Validate` checks at project-save time that the token is valid, that the site ID exists and is reachable with that token, and that `publish_dir` is set when a build command is present.

---

## Minimum credential scope

Netlify personal access tokens are **account-wide** and cannot be scoped to a single site. This is a real limitation and it is stated here rather than glossed over.

Mitigations, in order of preference:

1. Create a dedicated Netlify account, invite it to the specific team as a **Collaborator** rather than Owner, and issue the token from that account. Collaborators cannot manage billing or delete the team.
2. Use one token per Veyronix project so revocation is granular.
3. Rotate on a schedule; `veyronix target credentials rotate` exists for this.

**Accepted risk:** a leaked Netlify token grants access to every site that account can reach. Blast radius is controlled by account scoping, not by the token itself. Recorded in the [threat model](../THREAT_MODEL.md) under B6.2.

---

## How the operations map

| Interface method | Netlify API |
|---|---|
| `Validate` | `GET /sites/{site_id}` |
| `Deploy` | `POST /sites/{site_id}/deploys` with a digest manifest, then upload missing files |
| `Rollback` | `POST /sites/{site_id}/deploys/{deploy_id}/restore` |
| `Status` | `GET /deploys/{deploy_id}` |
| `Logs` | Poll `GET /deploys/{deploy_id}` build log endpoint, stream lines |
| `HealthCheck` | HTTP `GET` against the site URL, expect 2xx |

### Idempotency

Netlify's digest-based deploy is naturally idempotent: an identical file manifest produces the same deploy rather than a new one. The provider additionally records `req.IdempotencyKey` in the deploy title and checks recent deploys for the key before creating one — belt and braces, because relying on an external service's incidental idempotency is relying on an implementation detail.

### Health check timing

CDN propagation is not instantaneous. The health check must retry with backoff rather than probing once immediately after the API returns success — a deploy marked failed because the CDN had not propagated yet is a false failure that triggers an unnecessary rollback. Default: 5 retries, exponential backoff, `VEYRONIX_HEALTHCHECK_TIMEOUT` overall bound.

---

## Failure modes

| Failure | Signal | Behaviour |
|---|---|---|
| Invalid or revoked token | 401 | `Validate` fails at save time; at deploy time, fail pre-flight before any mutation |
| Site deleted | 404 | Deploy fails with `error_class = target_not_found`; no retry |
| Rate limited | 429 | Backoff and retry; circuit breaker after `VEYRONIX_PROVIDER_CIRCUIT_THRESHOLD` |
| Build fails at Netlify | Deploy state `error` | Classified as **user error**, excluded from the deployment-success SLI |
| Upload interrupted | Partial file set | Deploy never becomes live; retry is safe — Netlify only publishes complete deploys |
| Health check fails post-publish | Non-2xx | Auto-rollback via `restore` to the previous deploy ID |

---

## Manual intervention

For [runbook § rollback failure](../RUNBOOK.md#rollback-failure), when the platform cannot recover:

```bash
# List recent deploys
curl -H "Authorization: Bearer $NETLIFY_TOKEN" \
  "https://api.netlify.com/api/v1/sites/$SITE_ID/deploys?per_page=10"

# Restore a known-good deploy
curl -X POST -H "Authorization: Bearer $NETLIFY_TOKEN" \
  "https://api.netlify.com/api/v1/sites/$SITE_ID/deploys/$DEPLOY_ID/restore"
```

Record any manual action in the incident timeline — Veyronix's record of the live release is now stale and must be reconciled afterwards.
