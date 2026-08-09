# RPD — Veyronix v1

**System:** Veyronix — Internal Developer Platform
**Context:** Internal tool, Westpay Global Resources
**Owner:** Nicholas Emmanuel
**Status:** Proposed
**Date:** August 2026

---

## 1. Problem

Deployment knowledge at Westpay is tribal. The frontend goes to one target, a backend lives on a VPS reached by SSH, an older service is on a PaaS, and each one has a different workflow, a different credential path, and a different person who knows how it works. Three consequences follow:

1. **Deploys are gated on people.** When the person who knows a service is unavailable, that service cannot ship.
2. **There is no shared audit trail.** Who deployed what, to which environment, at what time, and who approved it — the answer lives in someone's terminal history, if anywhere.
3. **Failure handling is per-service and ad hoc.** Rollback means "the person remembers the previous version." A deploy interrupted halfway leaves state nobody can describe.

For a payments company this third point is not just an engineering inconvenience. An unauditable, unrepeatable production change process is an audit finding.

## 2. Goal

One workflow — *which project, which environment, which version* — that works across every deployment target the company uses, produces a durable audit record for every action, and recovers correctly when the machine executing a deploy disappears mid-flight.

## 3. Non-goal (why not just buy something)

This is the question a reviewer will ask first, so it is answered here rather than defended later.

| Tool | Why it does not solve this | Where we use it anyway |
|---|---|---|
| Argo CD | Kubernetes-only. Cannot deploy to SSH hosts, PaaS, or CDN targets. | Used *inside* the Kubernetes provider — Veyronix writes manifests to git, Argo reconciles. |
| Terraform / Pulumi | Provisions infrastructure. Does not model an application release, rollback, or health verification. | Provisions the infrastructure Veyronix deploys onto. Out of scope here. |
| GitHub Actions | Builds artifacts well; has no durable state, no rollback, no per-environment concurrency, no authorization model over targets. | Builds artifacts. Veyronix is triggered by it, not replaced by it. |
| Backstage | A portal and service catalog. Has no deployment engine — Backstage plugins call something like Veyronix. | Candidate front-end in v3. |
| Heroku / Vercel / Render | Single-target platforms. Adopting one means migrating everything to it. | They are targets, not the platform. |

**What Veyronix actually builds is one thing: a provider abstraction over heterogeneous deployment targets.** Everything else in the system is bought, not built. That distinction is the whole design.

## 4. Users

| Role | Needs |
|---|---|
| **Application developer** | Deploy their service without knowing the target's mechanics; see live logs; roll back without asking anyone |
| **Team lead** | Approve production releases; see what is deploying and what failed |
| **Platform engineer (you)** | Add a new target without touching the engine; operate the platform against SLOs |
| **Auditor / compliance** | An immutable, exportable record of every production change: who, what, when, approved by whom |

## 5. User stories

- As a **developer**, I want to deploy my service by choosing project, environment, and version, so that I do not need to know whether the target is SSH, a PaaS, or Kubernetes.
- As a **developer**, I want live deployment logs streamed to me, so that a failure is diagnosable without asking someone for server access.
- As a **developer**, I want a one-command rollback to the last known-good release, so that recovery does not depend on remembering a version string.
- As a **team lead**, I want production deployments to require an approval from someone other than the author, so that we satisfy separation of duties.
- As a **platform engineer**, I want to add a new deployment target by implementing one interface, so that adding a provider is not an engine rewrite.
- As an **auditor**, I want to export every production change for a date range with actor, resource, approval, and outcome, so that change control is evidenced rather than asserted.
- As an **on-call engineer**, I want a deploy interrupted by a worker crash to resume or fail cleanly, so that I never have to determine by hand what state a half-finished deploy left behind.

## 6. Functional requirements

**Projects and targets**
1. Register a project with a repository, environments, and a deployment target per environment.
2. Validate target credentials at save time, not at deploy time.
3. Store per-environment configuration and secret *references* (never secret values).

**Deployment**
4. Accept a deploy request for (project, environment, version) and return immediately with a deployment ID.
5. Execute the deployment as a durable workflow that survives the process running it.
6. Enforce one in-flight deployment per (project, environment); further requests queue.
7. Deduplicate: a repeat request for the same (project, environment, commit) returns the existing deployment rather than creating a second.
8. Stream deployment events and logs to the caller in real time.
9. Run a health check after the provider reports success.
10. Roll back automatically to the last known-good release when the health check fails and the environment has auto-rollback enabled.
11. Support manual rollback to any prior successful release.
12. Support cancellation of a queued or in-flight deployment.

**Access and approval**
13. Authenticate users against the company identity provider via OIDC. No local passwords.
14. Authorize every action against a relationship-based policy: a developer on team X can deploy X's projects and no others.
15. Require an approval from a second person before any deployment to a production environment.
16. Deny by default — a subject with no matching grant can deploy nothing.

**Audit**
17. Write an immutable audit record before every mutating action, containing actor, action, resource, decision, approval reference, and request ID.
18. Export audit records for a date range in a machine-readable format.
19. Retain audit records for the company's stated retention period, independent of deployment history pruning.

**Secrets**
20. Resolve secrets from the secrets manager at deploy time; never persist plaintext.
21. Scrub known secret values from logs and event streams before emission.

**Operations**
22. Expose Prometheus metrics for every pipeline phase.
23. Expose `/healthz` and `/readyz`.
24. Emit OpenTelemetry traces spanning API request through workflow through provider call.

## 7. Acceptance criteria

**Durability — the load-bearing one**
> Given a deployment in the build phase
> When the worker process is killed with `SIGKILL`
> Then a replacement worker resumes the deployment from the last completed step, and exactly one release is produced at the target.

> Given a deployment that has already produced a release
> When the same request is retried with the same idempotency key
> Then no second release is created and the original deployment is returned.

**Concurrency**
> Given a deployment in flight for (payments-api, production)
> When a second deploy is requested for the same pair
> Then it is queued and starts only after the first reaches a terminal state.

**Rollback**
> Given a deployment whose health check fails
> When the verify phase completes
> Then the provider's `Rollback` is invoked, the previous release is restored, and the deployment ends in `RolledBack`.

> Given a rollback that itself fails
> When the rollback phase errors
> Then the environment is frozen, on-call is paged, and the deployment ends in `Failed`.

**Authorization**
> Given a developer on the Payments team
> When they request a deploy of an HR-team project
> Then the request is denied, no provider call occurs, and the denial is audited.

**Approval**
> Given a deployment request targeting a production environment
> When the requester is the only approver
> Then the deployment remains in `AwaitingApproval` and does not execute.

**Secrets**
> Given a build whose output contains a configured secret value
> When logs are streamed to the UI
> Then the secret is replaced with a redaction marker in every emitted line.

**Audit**
> Given any production deployment
> When an auditor exports the change log for that date
> Then the record contains actor, project, environment, version, approver, timestamps, and final state.

**Provider isolation**
> Given a new provider implementing the interface and passing the conformance suite
> When it is registered
> Then it deploys successfully with zero changes to the engine, the authorization model, or the dashboard.

## 8. Non-functional requirements

| Property | Target |
|---|---|
| API availability | 99.9% over 30 days |
| Deployment success rate | 99.0% excluding user build failures |
| Queue time (accepted → started), p95 | < 10s |
| Time to deploy (accepted → healthy), p95 | < 5 min |
| Time to recovery (failure → previous healthy), p95 | < 3 min |
| Event delivery lag, p99 | < 2s |
| Secrets | Never at rest in Veyronix's database in plaintext; never on worker disk |
| Build isolation | User repository code runs in a container with no network egress by default and no access to platform credentials |
| Bus factor | Every operational procedure documented in the runbook; no step that only the author can perform |

## 9. Out of scope for v1

- Canary and blue/green strategies (v2 — providers declare capability, engine does not yet orchestrate)
- Multi-region control plane
- Cost reporting
- Infrastructure provisioning
- Public API and third-party SDK distribution
- Self-service database and namespace provisioning
- Model-serving targets

## 10. Success metrics

**Adoption** — three services onboarded and deploying through Veyronix within one month of v1; zero manual production deploys for those services thereafter.

**Reliability** — SLO table above met over a 30-day window with error budget tracked.

**Compliance** — a production change report generated for an auditor without engineering intervention.

**Bus factor** — a second engineer performs a production rollback using only the runbook, unaided.

## 11. Build order

Each row is a deployable increment. Do not start a row until the row above is in use.

| # | Increment | Requirements | Done when |
|---|---|---|---|
| 1 | Project registry + OIDC login | 1, 13 | A developer logs in with their company account and registers a project |
| 2 | Provider interface + SSH provider | 1, 2 | `veyronix deploy` ships a service to a VPS from the CLI |
| 3 | Temporal workflow wrapping the deploy | 4, 5 | Kill the worker mid-deploy; it resumes and completes |
| 4 | Idempotency + per-environment concurrency | 6, 7 | Double-click produces one release; second deploy queues |
| 5 | Event and log streaming | 8 | Live logs in the terminal and the UI |
| 6 | Health check + auto-rollback | 9, 10, 11 | A deliberately broken release rolls itself back |
| 7 | OpenFGA authorization | 14, 16 | Cross-team deploy is denied and audited |
| 8 | Audit log + export | 17, 18, 19 | Export a day's production changes as CSV/JSON |
| 9 | Vault secret resolution + log scrubbing | 20, 21 | Secret never appears in logs, database, or disk |
| 10 | Production approval gate | 15 | Prod deploy blocks until a second person approves |
| 11 | Second provider (PaaS or container registry) | — | Provider added with no engine change |
| 12 | Metrics, traces, dashboards, runbook | 22, 23, 24 | SLO dashboard live; runbook rehearsed by a second engineer |
| 13 | Kubernetes provider via Argo CD | — | Manifests written to git; Argo reconciles; Veyronix reports status |

---

*This document states what Veyronix must do and how we will know it does it. How each piece is implemented — and which decisions were contested — belongs in `docs/ENGINEERING.md` and `docs/adr/`.*
