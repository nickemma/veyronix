# Veyronix — System Design Document

| | |
|---|---|
| **Status** | Draft — design ahead of implementation |
| **Author** | [@nickemma](https://github.com/nickemma) |
| **Last updated** | 2026-07-28 |
| **Reviewers** | _open — see [Contributing](../README.md#contributing)_ |
| **Related** | [ADRs](adr/) · [Threat Model](THREAT_MODEL.md) · [SLOs](sre/slo.md) · [Runbook](RUNBOOK.md) |

---

## 1. Problem Statement

Deployment knowledge does not scale. A typical engineering organization accumulates deployment targets the way a house accumulates cables: the frontend is on Netlify, one backend runs on a VPS reached over SSH, an older service is still on Heroku, and the newest thing landed on ECS because that is what its author knew.

Each of those has a distinct workflow, credential path, failure mode, and — critically — a distinct person who understands it. The cost is not the deploy itself. The cost is that deployment becomes tribal knowledge, and tribal knowledge is an outage waiting for a vacation.

Concretely, the failure modes we are designing against:

1. **Bus factor.** One engineer knows how to release the payments service.
2. **Credential sprawl.** Provider tokens live in `.env` files, CI secrets, and someone's password manager.
3. **No audit trail.** "Who deployed what to production at 03:00, and what was the previous version?" has no answer.
4. **Unrecoverable partial state.** A deploy script dies halfway; nobody knows whether the release landed.
5. **No rollback path.** Rolling back means remembering the previous commit and running the script again, by hand, under pressure.

### Goals

- One workflow — *project, environment, version* — for every deployment target.
- Deployments that survive worker death, network partition, and duplicate submission.
- Rollback as a first-class operation, automatic on health-check failure.
- Authorization that models real org structure, not a single admin flag.
- Credentials handled on the assumption the database will eventually leak.
- The platform measured with SLOs the way a production dependency should be.
- Adding a new provider is writing a plugin, not modifying the engine.

### Non-Goals

See [README § Non-Goals](../README.md#non-goals). Restated here because they constrain the design:

- Not a CI system. Veyronix can invoke a build; it does not replace GitHub Actions.
- Not an infrastructure provisioner. Terraform creates the cluster; Veyronix deploys onto it.
- Not a monitoring platform. It emits telemetry into your stack.
- Not a lowest-common-denominator abstraction. Providers declare capabilities honestly.

### Success Criteria for V1

- A developer with no knowledge of Netlify can release the frontend.
- A worker killed with `SIGKILL` mid-deploy results in a completed or cleanly failed deployment, never a stuck one.
- Every deployment has an audit record written *before* its effect.
- No provider credential is readable from a database dump alone.

---

## 2. Users and Use Cases

| Actor | Needs |
|---|---|
| **Developer** | Deploy my service to staging; see logs; roll back when I break it |
| **Tech lead** | Approve production releases; see who deployed what |
| **Platform / SRE** | Add providers; set policy; be paged only when the system cannot self-heal |
| **CI system** | Trigger a deploy on merge, with an idempotency key, and get a job ID back |
| **Auditor** | Answer "who had access to what, when" from durable records |

### Primary flow

1. Developer opens the project, selects environment and version, clicks Deploy.
2. API authenticates, authorizes, resolves the provider, derives the idempotency key.
3. Job and outbox event commit in one Postgres transaction; API returns `202` with a deployment ID.
4. Relay publishes to NATS; a worker claims the job under a lease.
5. Worker clones, builds in an isolated container, calls `Provider.Deploy`, then `Provider.HealthCheck`.
6. Events stream to the dashboard the entire time.
7. Healthy → `Succeeded`. Unhealthy → `Provider.Rollback` → `RolledBack`.

---

## 3. Architecture Overview

Veyronix is a **modular monolith with ports and adapters** ([ADR-0001](adr/0001-modular-monolith.md)). One codebase, several process types, strict module boundaries enforced by dependency direction rather than by network calls.

### Process topology

| Process | Binary | Responsibility |
|---|---|---|
| Control plane | `veyronix-api` | Auth, authorization, project state, RPC surface, job submission |
| Worker | `veyronix-worker` | Claims jobs, executes the deployment pipeline, emits events |
| Outbox relay | `veyronix-relay` | Tails the outbox table, publishes to NATS, marks published |
| CLI | `veyronix` | Thin client over the same Connect API |

All three server binaries are built from the same modules; they differ only in which adapters the composition root wires up.

### Module map

| Module | Owns | Exposes (inbound port) |
|---|---|---|
| `identity` | Users, sessions, OAuth linkage | `Authenticator` |
| `authz` | Roles, policies, attribute rules | `Authorizer`, `Explainer` |
| `project` | Projects, environments, targets, members | `ProjectService` |
| `secrets` | Encrypted credentials and env vars | `SecretResolver` |
| `deployment` | Jobs, leases, state machine, events | `DeploymentService` |
| `audit` | Immutable action log | `AuditRecorder` |
| `notification` | Slack, email, webhook fan-out | `Notifier` |

Rules that keep the boundaries real:

- A module's `domain` package imports nothing from the project.
- A module never reaches into another module's tables or repositories.
- Cross-module calls go through the target's inbound port, injected at the composition root.
- Anything asynchronous crosses as a published event, not a function call.

The payoff: if one module ever must become a service, its adapters change and its `app` package does not.

---

## 4. The Deployment Pipeline

### Why a deploy is not a request handler

A synchronous model — `User → Deploy → Provider → Done` — breaks on first contact:

- A build longer than the load balancer's 60s timeout kills the connection and orphans the work.
- A worker pod rescheduled mid-deploy loses all in-memory state.
- A double-clicked button produces two releases.

So a deploy is modelled as a **durable, resumable job**.

### Write path

```
CreateDeployment(req)
  ├─ authenticate                → identity
  ├─ authorize(subject, action, resource) → authz
  ├─ resolve project + environment + target → project
  ├─ derive idempotency key = H(project, env, version)
  ├─ IF key exists → return existing deployment (no new job)
  └─ BEGIN
       INSERT deployments (state = Queued)
       INSERT jobs        (deployment_id, lease = NULL)
       INSERT outbox      (topic, payload)
       INSERT audit       (subject, action, resource, decision)
     COMMIT
  └─ return 202 + deployment_id
```

The audit row is written inside the same transaction as the effect it describes, so there is no window where an action happened without a record.

### Claim path

Workers claim with a lease rather than a queue pop, so death is recoverable:

```sql
UPDATE jobs
   SET lease_owner   = $1,
       lease_expires = now() + $2::interval,
       lease_version = lease_version + 1,
       claimed_at    = now()
 WHERE id = (
     SELECT id FROM jobs
      WHERE state = 'queued'
        AND (lease_expires IS NULL OR lease_expires < now())
        AND NOT EXISTS (
            SELECT 1 FROM jobs j2
             WHERE j2.project_id  = jobs.project_id
               AND j2.environment = jobs.environment
               AND j2.state IN ('claimed','building','deploying','verifying')
        )
      ORDER BY priority DESC, created_at
      FOR UPDATE SKIP LOCKED
      LIMIT 1
 )
RETURNING *;
```

Three properties in one statement: `SKIP LOCKED` prevents two workers contending on the same row; the `NOT EXISTS` clause enforces one in-flight deployment per `(project, environment)`; the expiry predicate makes an abandoned job reclaimable without a reaper process.

The worker heartbeats every `JOB_HEARTBEAT_INTERVAL`, extending `lease_expires` and checking `lease_version` still matches. A lost heartbeat race means the worker abandons the job rather than double-executing.

### Execution

```
Clone → Checkout → Build (isolated container) → Provider.Deploy → Provider.HealthCheck
                                                                    │
                                                    ┌───────────────┴───────────────┐
                                                 healthy                       unhealthy
                                                    │                               │
                                              Mark Succeeded                Provider.Rollback
```

Each stage emits an event through the outbox, so progress is durable and the UI is a subscriber rather than a poller. Phase timings (`queue`, `build`, `deploy`, `verify`) are recorded separately — "deploys are slow" is not actionable; "build p95 doubled after the base image change" is.

### State machine

```
Queued ──claim──> Claimed ──checkout──> Building ──artifact──> Deploying ──accepted──> Verifying
   │                  │                     │                      │                       │
   │                  └──lease expired──> Queued                   │                       ├─ healthy → Succeeded
   ├─ cancel → Cancelled                    └─ error → Failed      └─ error → Failed       └─ unhealthy → RollingBack
                                                                                                              │
                                                                                        ┌─────────────────────┴──────────┐
                                                                                  RolledBack                   Failed (pages)
```

The transition table lives in `internal/modules/deployment/domain/state.go` as data, and the only legal way to change a deployment's state is `Deployment.Transition(to)`. Illegal transitions return `ErrInvalidTransition` rather than being silently permitted — this is the alternative to boolean soup (`is_running`, `is_failed`, `did_rollback`) where invalid combinations are representable.

`RollingBack → Failed` is the sole paging condition. Everything else is the system doing its job.

---

## 5. Correctness Properties

### Atomic job + event: the transactional outbox

Writing to Postgres and publishing to NATS in sequence is a dual write, and dual writes are silently wrong under partial failure — either a job exists that no worker hears about, or an event fires for a job that rolled back. The outbox makes the two atomic ([ADR-0003](adr/0003-transactional-outbox.md)): both rows commit together, and a relay tails the outbox and publishes.

The relay is at-least-once. Consumers must therefore be idempotent, which they are, because every event carries the deployment ID and a monotonic sequence.

### Exactly-once effect on an at-least-once substrate

The queue may deliver a job twice. Providers are therefore called with `req.IdempotencyKey`, and the `Provider` contract requires that a repeated call with the same key not produce a second release. Providers that cannot enforce this natively (SSH) implement it with a marker file at the target; providers that can (Netlify's deploy API) pass it through.

### One in-flight deployment per environment

Enforced in the claim query, not in application logic, so it holds across arbitrarily many workers without coordination.

---

## 6. Provider Abstraction

The bet the whole platform rests on: the deployment engine never learns what a provider *is*. It knows only that something implements the interface in [`sdk/provider.go`](../sdk/README.md) — `Name`, `Validate`, `Deploy`, `Rollback`, `Status`, `Logs`, `HealthCheck`, `Capabilities`.

The interface lives in the **public** `sdk/` package rather than `internal/`, deliberately: Go's `internal/` rule would make third-party providers impossible, and "others can extend it" would be aspiration rather than fact.

### Capability negotiation

`Capabilities()` exists because honesty beats a leaky abstraction. Netlify cannot do a canary. A bare VPS cannot do blue/green without help. Rather than defining an interface every target pretends to satisfy, providers declare support and the engine and dashboard degrade gracefully — an unsupported strategy is rejected at project-save time by `Validate`, not discovered at 02:00 during a deploy.

### Conformance

`sdk/conformance` is a suite any provider must pass: idempotency under repeated `Deploy`, correct error classification, `Logs` termination on context cancel, `Rollback` restoring the prior release. A provider that passes integrates without engine changes. This is the contract test that keeps the abstraction from rotting as the seventh provider lands.

---

## 7. Authorization

Hybrid RBAC + ABAC ([ADR-0007](adr/0007-hybrid-rbac-abac.md)). Roles answer *what kind of action*; attributes answer *on which resource*.

```
subject: alice@acme.io
role:    developer            → may deploy, may not manage members
attrs:   team = payments      → scoped to resources where project.team = payments
action:  deploy
resource: project/ecommerce-api/production
          + environment.requires_approval = true

decision: DENY (production requires approval; developer role lacks approve)
```

Pure RBAC would need `payments-developer`, `hr-developer`, `payments-lead`… — a role per team per level, growing multiplicatively. Pure ABAC becomes an unreadable policy language nobody audits. The hybrid keeps the role count small and the scoping expressive.

Deny by default: a subject with no matching grant can deploy nothing. Decisions are explainable — `veyronix authz explain` returns the matched (or unmatched) policy, because an authorization system nobody can debug becomes an authorization system somebody disables.

---

## 8. Secrets

Envelope encryption ([ADR-0008](adr/0008-envelope-encryption.md)), on the assumption the database will leak:

- Each project has a **DEK** (data encryption key), used with AES-256-GCM.
- The DEK is stored wrapped by a **KEK** held outside Postgres (env var in V1, KMS later).
- A database dump alone yields ciphertext.
- Secrets are decrypted only in worker memory, at deploy time, never written to disk.
- Log lines are scrubbed against known secret values before emission.
- Rotation re-wraps DEKs without re-encrypting every secret.

Build isolation complements this: user repositories are cloned and built inside a container with no network egress by default and no access to the worker's own credentials. A malicious `postinstall` script is a first-class threat, not an afterthought — see the [threat model](THREAT_MODEL.md).

---

## 9. Data Model (abbreviated)

```
organizations ─┬─ users ── identities (oauth provider, subject)
               ├─ teams ── team_members
               └─ projects ─┬─ environments ─┬─ targets (provider, config)
                            │                ├─ env_vars
                            │                └─ secret_refs ── secrets (ciphertext, dek_id)
                            ├─ project_members (user, role, attrs)
                            └─ deployments ─┬─ jobs (state, lease_owner, lease_expires, lease_version)
                                            ├─ deployment_events (seq, type, payload)
                                            └─ releases (provider_release_id, artifact)

outbox (topic, payload, created_at, published_at)
audit_log (subject, action, resource, decision, request_id, created_at)   -- append only
policies (role, action, resource_pattern, attribute_predicate)
```

Notable choices: `deployments.previous_id` points at the rollback target, so the release history is a linked list rather than requiring a query over timestamps. `deployment_events.seq` is per-deployment and monotonic, so a reconnecting UI can resume from its last seen sequence.

---

## 10. API Surface

Connect over a single `.proto` ([ADR-0004](adr/0004-grpc-vs-rest.md)) — gRPC internally, HTTP/JSON at the browser edge, one schema, generated Go server, TypeScript client, and Zod validators. Server-streaming carries deploy logs and events, which is the concrete reason the transport is not plain REST.

Generated OpenAPI lives at [`api/openapi.yaml`](../api/openapi.yaml).

---

## 11. Observability

Traces span request → job → worker → provider call, propagated across the async boundary via the outbox payload, so a slow deploy is attributed rather than guessed at. Metrics are enumerated in the [README](../README.md#metrics); each one exists to answer a question someone will ask during an incident. Logs are Loki-aggregated and correlated by deployment ID.

SLOs, error budgets, and the policy that gates feature work on budget burn are in [`sre/slo.md`](sre/slo.md).

---

## 12. Rollout Plan

| Phase | Content | Exit criterion |
|---|---|---|
| 0 | Platform floor, domain state machine, migrations | State machine test suite green |
| 1 | Outbox + relay + lease claiming | Kill -9 a worker mid-job; job completes |
| 2 | Execution pipeline against fake provider | Full lifecycle with no real infra |
| 3 | Netlify, SSH, Heroku providers | Conformance suite passes for each |
| 4 | Connect API + dashboard | A developer deploys without reading docs |
| 5 | Veyronix deploys Veyronix | Dogfood; SLOs measured on real traffic |

---

## 13. Open Questions

1. **Build execution location.** In-worker containers are simple but couple build capacity to worker capacity. A separate build service is cleaner and is a second thing to operate. Deferred to post-V1 measurement.
2. **Log retention.** Streaming to Loki is decided; how long deploy logs stay queryable inside Veyronix itself is not.
3. **Multi-region.** The outbox is single-writer today. Cross-region requires either a global Postgres or per-region job stores with a routing rule. Not V1.
4. **Approval workflows.** Modelled as an environment attribute in authz; whether approvals get their own module is unresolved.
5. **Provider versioning.** When a provider's config schema changes, existing targets must migrate. Mechanism undesigned.

---

## 14. Alternatives Considered

Recorded in full in [`adr/`](adr/). Summarized:

| Decision | Chosen | Rejected | Why |
|---|---|---|---|
| Structure | Modular monolith | Microservices | Boundaries without distributed-systems tax at this stage |
| Job store | Postgres | Redis, Temporal, SQS | Job and event must commit atomically |
| Event publication | Transactional outbox | Dual write | Dual write is silently wrong under partial failure |
| Transport | Connect | Plain gRPC, plain REST | One schema, browser-native, real streaming |
| Broker | NATS JetStream | Kafka, Redis Streams | Durable streams at a fraction of the operational weight |
| Authorization | RBAC + ABAC | Pure RBAC, pure ABAC, OPA | Small role count, expressive scoping, debuggable |
| Secrets | Envelope encryption in Postgres | Vault, cloud KMS only | No extra dependency in V1; KEK boundary preserved |
