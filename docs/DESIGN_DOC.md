# Plinth Design Document

**Canonical product document:** [`plinth.md`](plinth.md)

This document explains how Plinth will be built. It must remain an implementation companion to `plinth.md`, not a second product specification. If a design detail conflicts with the canonical document, update this document or record the decision in `docs/ADR/`.

## 1. Product shape

Plinth is a small Kubernetes control plane. Its core responsibility is to translate a service manifest into desired Kubernetes resources and continuously reconcile those resources with the cluster's observed state.

The developer-facing contract is deliberately narrow:

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

The platform owns the Kubernetes details required for a safe service: networking, TLS, observability, health checks, resource controls, security context, disruption protection, and default-deny networking.

The same lifecycle is available through the CLI and an HTTP API. Swagger UI is the browsable API reference. The playground is a deliberately small browser client for sending test requests to that API; it is not a service catalog or operational dashboard.

## 2. Core control loop

The system has three states:

1. **Desired state** — the validated manifest and the selected revision stored by Plinth.
2. **Observed state** — the resources and status reported by Kubernetes.
3. **Reconciliation** — an idempotent level-triggered loop that calculates and applies the difference.

The loop must be safe to run repeatedly. It must also be correct when:

- it starts with no resources;
- resources already exist and match;
- a user manually changes or deletes a resource;
- an action fails halfway through;
- the reconciler process is killed and restarted;
- the Kubernetes API is temporarily unavailable;
- two reconcilers observe the same object.

The fake backend is not throwaway code. It proves the control-loop semantics without requiring a cluster and remains a permanent test backend.

## 3. Repository shape

```text
cmd/
  plinth/              CLI
  plinthd/             control plane
api/
  openapi.yaml         HTTP contract used by Swagger UI
internal/
  api/                  HTTP API, Swagger UI, and playground
  backend/              fake and client-go providers
  manifest/             parsing and validation
  reconcile/            reconciliation loop and worker
  state/                file-backed and Postgres state stores
  tenancy/              teams, namespaces, and quotas
operator/              CRD and Kubernetes reconcile controller
examples/              service manifests for Tessera and Lattice
deploy/                manifests and platform dependencies
docs/                  design, operations, ADRs, comparison
docs/walkthrough.md    end-to-end verification path
```

The exact package boundaries can evolve, but the responsibilities should not be mixed. The reconciler should express desired behavior, while providers translate that behavior to a backend.

## 4. API, Swagger UI, and playground

The API and CLI are two entry points to the same lifecycle; neither gets a separate deployment model. The API contract should cover manifest submission, status, logs, rollback, pause, destroy, and the status needed to explain why a revision is not converging.

The implementation should serve three clearly separated surfaces:

1. the control-plane API used by the CLI and automation;
2. Swagger UI, which makes the API contract discoverable and lets an engineer try safe requests;
3. the playground, which guides a complete test scenario and displays the resulting status, events, and logs.

The playground must be disposable and test-oriented. It must not grow into the dashboard explicitly excluded by `plinth.md`. The API contract, generated documentation, and browser client must stay aligned, with an automated check or build step catching drift.

## 5. Manifest and state model

The manifest is the user-facing input. Validation happens before any cluster mutation and reports actionable field-level errors. At minimum it validates:

- service name and image;
- port and replica count;
- environment/configuration shape;
- secret references;
- resource quantities;
- allowed lifecycle operations.

The local path stores validated desired state and revision history in a file-backed store so it can run with no external services. The control-plane deployment can select Postgres behind the same state boundary. A rollback does not become a special imperative deployment path: it selects an earlier revision as desired state and lets the same reconciler converge to it.

Configuration, secrets, and platform configuration are separate. Secret values are not stored in the application database. Developers declare secret names; Plinth emits references to an existing `<service>-secrets` Secret, while the target cluster's secret-management system provisions and rotates its values.

## 6. Kubernetes adapter

The Kubernetes adapter uses the API directly through `client-go`, not `kubectl`. Its fake-client tests cover resource creation and repeat application. A live cluster run is an environment-dependent verification step. The adapter demonstrates:

- typed clients and informers;
- watches, work queues, and resync;
- optimistic concurrency with resource versions;
- owner references and cascading deletion;
- server-side apply;
- status observation and conditions.

The adapter creates and manages the resources needed for the golden path, including Deployments, Services, Ingresses, ConfigMaps, the chosen external Secret injection reference, PodDisruptionBudgets, and NetworkPolicies.

The control plane is not in the workload request path. If Plinth is down after resources have been applied, services must continue serving. When Plinth returns, reconciliation resumes from desired and observed state.

## 7. Golden path

The golden path is a platform policy, not an optional template. A service submitted through Plinth receives:

1. a reachable endpoint and TLS;
2. a DNS name;
3. metrics scraping;
4. structured log shipping;
5. liveness and readiness probes;
6. resource requests and limits;
7. non-root execution with a read-only root filesystem;
8. a pod disruption budget;
9. a default-deny network policy.

The read-only root filesystem is paired with a small ephemeral runtime-data
volume at `/home/nonroot/.lattice-data`, owned by the non-root runtime group.
This keeps application state writes explicit and disposable without making the
container root filesystem writable.

The generated resources should carry clear ownership and labels so status, logs, audit entries, and cleanup can be associated with the Plinth service and revision. The default-deny NetworkPolicy explicitly allows ingress from namespaces labeled `plinth.dev/ingress=allowed` and DNS egress to `kube-system`; application-specific egress remains an intentional platform policy decision.

## 8. Tenancy and safety

Teams map to namespaces and receive explicit RBAC permissions. Resource quotas prevent one team from consuming the cluster without bounds. Every state-changing action records who changed what, when, the selected revision, and the previous revision.

Rollback is revision selection plus reconciliation. Progressive rollout is a guarded version transition: Plinth observes readiness and error-rate signals, continues while the release is within its policy, and aborts back to the last known-good revision when the policy is violated. The Kubernetes adapter reads the Prometheus instant-query API when `PLINTH_PROMETHEUS_URL` is configured; it fails closed when a rollout has no metric source. The default query expects `http_requests_total` series labeled with `namespace`, `service`, and `status`, and a custom four-placeholder query can be supplied for a cluster's metric schema.

The implementation keeps policy simple and explainable: team definitions are persisted alongside service state, map to namespaces, and are enforced on API reads and writes. Complexity belongs in a later decision record only when a demonstrated requirement demands it.

## 9. Operator comparison

The repository includes a small Kubernetes operator with a CRD, event watch plus periodic resync, status conditions, and finalizers. [`COMPARISON.md`](COMPARISON.md) compares that implementation with the standalone Plinth control plane. The controller is intentionally small and hand-written, while following the reconcile/status/finalizer shape that kubebuilder scaffolding would generate.

The comparison should cover ownership of desired state, deployment and failure behavior, Kubernetes coupling, operational surface, user experience, testing, and when a CRD is worth the cost.

## 10. GitOps and self-hosting

The repository includes Argo CD application manifests and Tessera/Lattice examples for the manifests-in-git model. The operator kustomization includes equivalent `PlinthService` objects, so Argo CD can apply the same golden path from Git. Applying them to a live cluster remains an environment-dependent verification step.

This phase also proves the important boundary: the control plane may be unavailable while already-running workloads continue serving. The test cases include a bad image tag, a service that never becomes ready, a missing secret, and an aborted rollout.

## 11. Delivery and evidence

Every phase ends with evidence, not just code:

- a runnable increment;
- focused tests for convergence and idempotency;
- a failure drill;
- an end-to-end walkthrough that can be run through the CLI, Swagger UI, and playground;
- a short explanation of the observed behavior;
- updated documentation when the implementation teaches us something new.

The fake backend remains a permanent test backend. No multi-cloud, service mesh, cost management, plugin system, or infrastructure provisioning is included; the UI is limited to Swagger and the test playground required by `plinth.md`.

## 12. Decisions and remaining environment choices

The core implementation choices are recorded in [`ADR/001-implementation-boundaries.md`](ADR/001-implementation-boundaries.md). The remaining choices are environment-specific:

- which TLS and DNS components are available in the target cluster;
- how metrics and structured logs are collected;
- how production error rate is measured for progressive rollout;
- which identity provider supplies production team membership;
- the exact Argo CD repository and destination configuration.

These are implementation decisions, not permission to expand the product beyond `plinth.md`.
