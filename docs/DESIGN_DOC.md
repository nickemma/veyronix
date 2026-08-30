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

## 3. Planned repository shape

```text
cmd/
  plinth/              CLI
  plinthd/             control plane
api/
  openapi.yaml         HTTP contract used by Swagger UI
internal/
  app/                 composition and application wiring
  platform/            shared technical infrastructure
  modules/
    manifest/          parsing, validation, and revisions
    desired/           desired-state storage
    reconcile/         reconciliation loop
    tenancy/           teams, namespaces, RBAC, quotas
    audit/             append-only change history
  providers/
    fake/              in-memory backend for phase 1 and tests
    kubernetes/        client-go adapter
operator/              CRD and kubebuilder controller
examples/              service manifests for Tessera and Lattice
deploy/                manifests and platform dependencies
web/playground/        test-only browser client for the Plinth API
docs/                  design, operations, ADRs, comparison
walkthrough.md         end-to-end verification path
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

The first vertical slice stores validated desired state and revision history in a file-backed store so it can run with no external services. Phase 2 replaces that store with Postgres behind the same state boundary. A rollback does not become a special imperative deployment path: it selects an earlier revision as desired state and lets the same reconciler converge to it.

Configuration, secrets, and platform configuration are separate. Secret values are not stored in the application database. The implementation of secret injection must be chosen before the golden-path phase and documented in an ADR; the product contract is that developers declare secret names while Plinth handles safe injection.

## 6. Kubernetes adapter

The Kubernetes adapter uses the API directly through `client-go`, not `kubectl`. Its fake-client tests cover resource creation and repeat application; cluster verification is the next step. The adapter demonstrates:

- typed clients and informers;
- watches, work queues, and resync;
- optimistic concurrency with resource versions;
- owner references and cascading deletion;
- server-side apply;
- status observation and conditions.

The adapter will create and manage the resources needed for the golden path, including Deployments, Services, Ingresses, ConfigMaps, Secrets or their chosen secret-injection equivalent, PodDisruptionBudgets, and NetworkPolicies.

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

The generated resources should carry clear ownership and labels so status, logs, audit entries, and cleanup can be associated with the Plinth service and revision.

## 8. Tenancy and safety

Teams map to namespaces and receive explicit RBAC permissions. Resource quotas prevent one team from consuming the cluster without bounds. Every state-changing action records who changed what, when, the selected revision, and the previous revision.

Rollback is revision selection plus reconciliation. Progressive rollout is a guarded version transition: Plinth observes health and error-rate signals, continues while the release is within its policy, and aborts back to the last known-good revision when the policy is violated.

The first implementation should keep policy simple and explainable. Complexity belongs in a later decision record only when a demonstrated requirement demands it.

## 9. Operator comparison

Phase 6 implements the same core behavior as a genuine Kubernetes operator: a CRD, kubebuilder-generated scaffolding, a controller reconcile function, status subresources and conditions, and finalizers. [`COMPARISON.md`](COMPARISON.md) will compare that implementation with the standalone Plinth control plane from direct experience.

The comparison should cover ownership of desired state, deployment and failure behavior, Kubernetes coupling, operational surface, user experience, testing, and when a CRD is worth the cost.

## 10. GitOps and self-hosting

The final phase adds Argo CD and the manifests-in-git model. Plinth should be able to deploy Tessera and Lattice through its own golden path.

This phase also proves the important boundary: the control plane may be unavailable while already-running workloads continue serving. The test cases include a bad image tag, a service that never becomes ready, a missing secret, and an aborted rollout.

## 11. Delivery and evidence

Every phase ends with evidence, not just code:

- a runnable increment;
- focused tests for convergence and idempotency;
- a failure drill;
- an end-to-end walkthrough that can be run through the CLI, Swagger UI, and playground;
- a short explanation of the observed behavior;
- updated documentation when the implementation teaches us something new.

The first implementation target is the fake backend. No Kubernetes integration, UI, multi-cloud abstraction, service mesh, cost management, plugin system, or infrastructure provisioning should be started before the control loop is understood and demonstrated.

## 12. Deferred decisions

The following choices must be resolved by an ADR when their phase arrives:

- how desired state and revisions are represented in Postgres;
- how configuration and secret references become Kubernetes inputs;
- which TLS and DNS components are available in the target cluster;
- how metrics and structured logs are collected;
- how error rate is measured for progressive rollout;
- the exact CRD shape and operator ownership boundaries;
- how Argo CD and the manifests-in-git workflow are integrated.
- how the OpenAPI contract is generated and how Swagger UI and the playground are served.

These are implementation decisions, not permission to expand the product beyond `plinth.md`.
