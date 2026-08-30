# Plinth

Plinth is a small internal developer platform built to make deploying a service to Kubernetes a golden path.

The developer writes one `plinth.yaml` file and runs:

```bash
plinth up
```

Plinth stores the desired state, observes what is actually running, and reconciles the difference until the service converges. The developer does not need to write Kubernetes manifests, configure ingress or TLS, wire up monitoring, or remember the rollback procedure.

This repository is starting from scratch. There is no working control plane yet; the project is built in deliberately small increments so every important behavior can be demonstrated and explained.

## The idea

The load-bearing concept is reconciliation:

```text
desired state ──┐
                ├─ reconcile repeatedly ──> actual state converges
actual state ───┘
```

The loop must be idempotent and level-triggered. It should recover after a process restart, repair manual drift such as a deleted pod, and safely retry an interrupted action. The Kubernetes adapter comes after this behavior is proven against an in-memory fake backend.

## What Plinth provides

From a small manifest, Plinth will provide:

- a running, reachable service with TLS and a DNS name;
- metrics scraping and structured log shipping;
- liveness and readiness probes;
- resource requests and limits;
- a non-root, read-only filesystem security context;
- a pod disruption budget and default-deny network policy;
- revision history and rollback to a previous revision;
- team isolation with namespaces, RBAC, quotas, and an audit log;
- a progressive rollout that can abort when error rate worsens.

The platform is intentionally opinionated. Correct defaults should be applied automatically instead of exposed as a long list of decisions every service team must get right.

## Example

```yaml
name: tessera-gateway
image: ghcr.io/nickemma/tessera:v0.4.1
port: 8080
replicas: 3
env:
  LOG_LEVEL: info
secrets:
  - DATABASE_URL
resources:
  cpu: 500m
  memory: 512Mi
```

The initial lifecycle is intentionally small:

```bash
plinth up
plinth status
plinth logs
plinth rollback
plinth pause
plinth destroy
```

Configuration, secrets, and platform configuration remain separate concepts. Environment promotion from dev to staging to production is also part of the intended platform experience, while staying within the project's scope.

## Build order

The work is organized around learning and proof, not around building a large feature set all at once:

1. Build a tiny fake-backend reconciler and prove convergence after drift and process death.
2. Define and validate the manifest, persist desired state and revisions in Postgres, and expose the CLI.
3. Replace the fake backend with a Kubernetes API adapter using `client-go`; learn watches, queues, resync, resource versions, owner references, and server-side apply.
4. Add the golden path defaults: TLS, DNS, metrics, logs, probes, limits, security, disruption protection, and network policy.
5. Add teams, namespaces, RBAC, quotas, audit history, rollback, and progressive rollout safety.
6. Rebuild the core as a Kubernetes operator with a CRD, then document the standalone-control-plane versus operator trade-offs.
7. Add GitOps with Argo CD and deploy Tessera and Lattice through Plinth itself.

Each stage should leave behind a working demonstration and a failure scenario that explains why the design is shaped this way.

## Repository guide

- [`docs/plinth.md`](docs/plinth.md) — the single source of truth for goals, scope, phases, and completion criteria.
- [`docs/DESIGN_DOC.md`](docs/DESIGN_DOC.md) — the implementation model derived from that source of truth.
- [`docs/RUNBOOK.md`](docs/RUNBOOK.md) — operational procedures and failure drills.
- [`docs/COMPARISON.md`](docs/COMPARISON.md) — the standalone control plane versus the Kubernetes operator.
- [`docs/ADR/README.md`](docs/ADR/README.md) — where important implementation decisions will be recorded.
- [`walkthrough.md`](walkthrough.md) — the end-to-end test path using the CLI, Swagger UI, and playground.

If another document conflicts with `docs/plinth.md`, `docs/plinth.md` wins and the other document must be corrected.

## Scope boundary

Plinth is not a product dashboard, multi-cloud platform, service mesh, cost-management system, plugin system, or infrastructure-provisioning tool. Swagger UI and the test playground are documentation/testing surfaces, not a dashboard. Those other capabilities are explicitly deferred. A finished, understandable control plane is more valuable here than an unfinished platform with a larger feature list.

## Status

The first vertical slice is working: the fake reconciler, queued worker, file-backed state, CLI, HTTP API, Swagger UI, playground, drift repair, rollback, and restart recovery are implemented. The Kubernetes adapter and fake-client tests are now in place; cluster verification, Postgres, tenancy, the operator, and GitOps are the next phases.
