# ADR 001: Plinth implementation boundaries

Status: accepted

## Context

Plinth needs one lifecycle across the CLI, HTTP API, fake backend, Kubernetes
backend, and operator. The platform also needs revision history without
storing secret values in its database.

## Decisions

- The manifest is validated before it enters the repository. A revision is an
  immutable manifest snapshot; rollback creates a new desired revision from a
  historical snapshot.
- The file store is the dependency-free local implementation. Postgres stores
  the same `Service` JSON state plus normalized audit and team tables behind
  the same repository interfaces.
- The Kubernetes provider uses typed `client-go` clients, shared informers for
  Deployment events, server-side apply for existing resources, labels for
  standalone ownership, and owner references when invoked by the operator.
- The default-deny network policy allows only labeled platform ingress and
  Kubernetes DNS egress; teams must explicitly add any application-specific
  egress policy required by their service.
- Secret values are never accepted in a Plinth manifest or written to state.
  `secrets` names become references to an existing `<service>-secrets` Secret
  in the workload namespace; provisioning that Secret belongs to the
  cluster's chosen secret-management system.
- Generated Pods keep the root filesystem read-only and expose only an
  ephemeral, non-root-owned runtime-data volume for applications that need a
  local working directory.
- Swagger UI and the playground are embedded testing surfaces served by the
  control plane. They use the same OpenAPI contract and API routes as the
  CLI.
- The fake backend returns deterministic readiness and error-rate signals so
  convergence, rollback, and progressive rollout tests do not require a
  cluster.
- The Kubernetes backend reads a configurable Prometheus instant-query API for
  progressive rollout error rates. A rollout without a configured metrics URL
  fails closed instead of treating an unknown signal as zero errors.

## Consequences

The local and CI path stays fast and reproducible. A real cluster must provide
the referenced secrets, cert-manager issuer, ingress/DNS, metrics, and log
integrations. Those integrations are intentionally platform dependencies,
not hidden application-database state.
